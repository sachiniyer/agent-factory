package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/sachiniyer/agent-factory/cmd"
)

const ProgramClaude = "claude"
const ProgramCodex = "codex"
const ProgramAider = "aider"
const ProgramGemini = "gemini"
const ProgramAmp = "amp"
const ProgramOpencode = "opencode"

// SupportedPrograms is the canonical list of known agent programs.
//
// APPEND-ONLY: app/handle_overlay.go indexes this slice by overlay position, so
// inserting an entry silently re-points every agent after it at the wrong menu
// row. New agents go on the end.
var SupportedPrograms = []string{ProgramClaude, ProgramCodex, ProgramAider, ProgramGemini, ProgramAmp, ProgramOpencode}

// SupportedProgramsString returns the canonical user-facing agent enum list.
func SupportedProgramsString() string {
	return strings.Join(SupportedPrograms, ", ")
}

// IsSupportedProgram reports whether name is one of the canonical agent enum
// names. It answers the membership question against the same slice
// SupportedProgramsString renders, so a caller's validation and its error
// message can never disagree about what the list is.
func IsSupportedProgram(name string) bool {
	for _, supported := range SupportedPrograms {
		if name == supported {
			return true
		}
	}
	return false
}

// TmuxSession represents a managed tmux session
type TmuxSession struct {
	// Initialized by NewTmuxSession
	//
	// The name of the tmux session and the sanitized name used for tmux commands.
	sanitizedName string
	// program is the command the pane runs. It is mutated by Restore() while
	// other goroutines read it, so all access goes through the programMu-guarded
	// accessors in program.go (#1254) — never touch these fields directly.
	program   string
	programMu sync.RWMutex
	// submitMu serializes the whole clear → paste → Enter transaction. A second
	// submit must never clear the first submit's freshly pasted composer while
	// that first call is still waiting to send Enter (#2178 review).
	submitMu sync.Mutex
	// lastPastedTail is the normalized distinctive tail of the most recent
	// payload paste that tmux accepted. The next submit may use it only as
	// provenance for the cleared-composer diagnostic: matching arbitrary pane
	// text is not enough to claim that a prior delivery was stranded (#2225).
	// Protected by submitMu; it never gates delivery.
	lastPastedTail string
	// ptyFactory is used to create a PTY for the tmux session.
	ptyFactory PtyFactory
	// cmdExec is used to execute commands in the tmux session.
	cmdExec cmd.Executor

	// Initialized by Start or Restore
	//
	// monitor monitors the tmux pane content and sends signals to the UI when it's status changes.
	// The pointer is swapped by Restore() (a fresh monitor on every restore) on the
	// restore/RPC/event-loop goroutines while the daemon's per-second poll reads it — and mutates
	// its dead/prevOutputHash fields — inside HasUpdated(). Every access therefore goes through the
	// monitorMu-guarded helpers in monitor.go; never touch these two fields directly (#1528).
	//
	// There is deliberately no attach-client PTY here (#1592 Phase 2 PR7): the
	// local runtime's data plane is the daemon's clientless agent-server
	// (pipe-pane → WS broker, PR5/6), and interactive full-screen attach is a WS
	// subscriber in the client (apiclient.AttachStream). The tmux-server-mediated
	// attach driver — the attach ptmx, its io.Copy/detach-key goroutines, and the
	// SIGTERM/SIGKILL abandon-drain machinery — was retired here.
	monitor   *statusMonitor
	monitorMu sync.Mutex
}

const TmuxPrefix = "af_"

// ErrSessionGone is returned by PTY/tmux operations when the underlying tmux
// session no longer exists. Non-daemon callers (preview pane, sidebar resize,
// terminal pane) use errors.Is to detect this case and degrade gracefully
// (render an inactive-session state, skip silently) instead of logging at
// ERROR level (#496). The daemon's statusMonitor has its own latch (#489) and
// does not use this sentinel.
var ErrSessionGone = errors.New("tmux session no longer exists")

// ErrSessionNotStarted is positive evidence that a failed Start left no pane
// process able to write into the fresh worktree. The launch may have failed
// before its process began, or bounded readiness-timeout cleanup may have both
// removed the session and confirmed its pane exited. An answered launch failure
// is not enough: later name absence cannot prove that a pane never ran or finished
// flushing. LocalBackend may remove a newly-created worktree only when this marker
// is present; every other Start error is conservatively treated as a session that
// may still be using that workspace.
//
// A readiness timeout for a legacy spelling still does not prove tmux failed to
// create one: tmux may have rewritten that spelling (#2207). Only names admitted
// by hasStableTmuxSpelling can turn a confirmed absence into this marker.
var ErrSessionNotStarted = errors.New("tmux session definitely did not start")

// DetachKeyByte is the ASCII byte for the key used to detach from attached sessions.
// Default is 23 (Ctrl-W). Set via SetDetachKey.
var DetachKeyByte byte = 23

// DetachKeyDisplay is the human-readable name for the detach key (e.g. "ctrl-w").
var DetachKeyDisplay string = "ctrl-w"

// SetDetachKey sets the global detach key byte and display name.
func SetDetachKey(b byte, display string) {
	DetachKeyByte = b
	DetachKeyDisplay = display
}

// repoHash returns a short hex hash of the repo path for use in tmux session names.
func repoHash(repoPath string) string {
	h := sha256.Sum256([]byte(repoPath))
	return hex.EncodeToString(h[:4]) // 8 hex chars
}

// toTmuxName builds the tmux session name from a user-supplied title.
//
// tmux 3.4's session_check_name rewrites '.' and ':', then utf8_stravis
// escapes backslashes, variable-like '$' sequences, and control bytes. The
// command layer also expands '#' formats before storing the name. Mirroring
// those version-dependent transformations with a punctuation denylist has
// repeatedly missed another character (#574, #2207), and a missed character
// makes Start probe a different name from the one tmux created.
//
// Use a positive policy instead: Unicode letters, numbers and combining marks
// keep human-readable titles intact, while only '_' and '-' are admitted from
// ASCII punctuation. Whitespace is removed for compatibility with the existing
// naming scheme; every other rune becomes '_'. Persisted sessions bypass this
// function through NewTmuxSessionFromSanitizedName, so their exact names remain
// restorable across upgrades.
func toTmuxName(title string, repoPath string) string {
	title = strings.Map(func(r rune) rune {
		switch {
		case stableTmuxNameRune(r):
			return r
		case unicode.IsSpace(r):
			return -1
		default:
			return '_'
		}
	}, title)
	if repoPath != "" {
		return fmt.Sprintf("%s%s_%s", TmuxPrefix, repoHash(repoPath), title)
	}
	return fmt.Sprintf("%s%s", TmuxPrefix, title)
}

// stableTmuxNameRune is the positive punctuation policy shared by fresh-name
// construction and the post-start proof that tmux could not have rewritten the
// name. Keeping one predicate makes widening or narrowing that policy atomic.
func stableTmuxNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || r == '_' || r == '-'
}

// SanitizedNameForRepo returns the exact tmux session name a fresh local
// session with title will use in repoPath. Admission checks call this same
// derivation as NewTmuxSessionForRepo so the namespace they reserve cannot drift
// from the namespace Start eventually claims.
func SanitizedNameForRepo(title, repoPath string) string {
	return toTmuxName(title, repoPath)
}

// hasStableTmuxSpelling reports whether tmux stores name byte-for-byte. Fresh
// names produced by toTmuxName satisfy this positive policy. Legacy persisted
// exact names may not; Start must keep treating an apparently absent legacy name
// as unknown because tmux may have rewritten it when it was created (#2207).
func hasStableTmuxSpelling(name string) bool {
	if !strings.HasPrefix(name, TmuxPrefix) {
		return false
	}
	for _, r := range name {
		if !stableTmuxNameRune(r) {
			return false
		}
	}
	return true
}

// NewTmuxSession creates a new TmuxSession with the given name and program (no repo scoping).
func NewTmuxSession(name string, program string) *TmuxSession {
	return newTmuxSession(toTmuxName(name, ""), program, MakePtyFactory(), cmd.MakeExecutor())
}

// NewTmuxSessionForRepo creates a new TmuxSession with a repo-scoped name to avoid collisions.
func NewTmuxSessionForRepo(name string, repoPath string, program string) *TmuxSession {
	return newTmuxSession(SanitizedNameForRepo(name, repoPath), program, MakePtyFactory(), cmd.MakeExecutor())
}

// NewTmuxSessionFromSanitizedName creates a new TmuxSession with an exact pre-computed name.
// Used when restoring sessions from storage where the tmux name was already persisted.
func NewTmuxSessionFromSanitizedName(sanitizedName string, program string) *TmuxSession {
	return newTmuxSession(sanitizedName, program, MakePtyFactory(), cmd.MakeExecutor())
}

// NewTmuxSessionWithDeps creates a new TmuxSession with provided dependencies for testing.
func NewTmuxSessionWithDeps(name string, program string, ptyFactory PtyFactory, cmdExec cmd.Executor) *TmuxSession {
	return newTmuxSession(toTmuxName(name, ""), program, ptyFactory, cmdExec)
}

// NewTmuxSessionFromSanitizedNameWithDeps creates a TmuxSession with an exact
// pre-computed name AND injected dependencies. Used by restore-survival tests
// that reconstruct sessions by their exact persisted names while keeping the
// tmux interactions mock-backed (hermetic).
func NewTmuxSessionFromSanitizedNameWithDeps(sanitizedName, program string, ptyFactory PtyFactory, cmdExec cmd.Executor) *TmuxSession {
	return newTmuxSession(sanitizedName, program, ptyFactory, cmdExec)
}

// SanitizedName returns the sanitized tmux session name.
func (t *TmuxSession) SanitizedName() string {
	return t.sanitizedName
}

func newTmuxSession(sanitizedName string, program string, ptyFactory PtyFactory, cmdExec cmd.Executor) *TmuxSession {
	return &TmuxSession{
		sanitizedName: sanitizedName,
		program:       program,
		ptyFactory:    ptyFactory,
		cmdExec:       cmdExec,
	}
}

// NewSiblingSession builds a second TmuxSession in the same worktree that
// shares this session's PTY factory and command executor. Used for the #930
// per-tab sessions (e.g. an instance's shell tab alongside its agent tab):
// the sibling inherits the agent session's dependencies so a mock-backed agent
// session produces a mock-backed sibling in tests, keeping them hermetic, while
// production sessions get the real factory/executor. sanitizedName is the exact
// tmux session name (already repo-scoped/sanitized by the caller); program is
// the command to run.
func (t *TmuxSession) NewSiblingSession(sanitizedName, program string) *TmuxSession {
	return newTmuxSession(sanitizedName, program, t.ptyFactory, t.cmdExec)
}
