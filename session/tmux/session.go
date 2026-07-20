package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

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
	// accessors in program.go (#1254) — never touch these two fields directly.
	program   string
	programMu sync.RWMutex
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

var whiteSpaceRegex = regexp.MustCompile(`\s+`)

// repoHash returns a short hex hash of the repo path for use in tmux session names.
func repoHash(repoPath string) string {
	h := sha256.Sum256([]byte(repoPath))
	return hex.EncodeToString(h[:4]) // 8 hex chars
}

// toTmuxName builds the tmux session name from a user-supplied title.
//
// Characters that tmux does not preserve verbatim in session names must be
// replaced here so ExistsOrUnknown() and kill paths match the name tmux
// actually created (#574). Verified against tmux 3.4:
//   - '.' and ':' are silently rewritten to '_'; using them as-is causes
//     Start() to poll for a name tmux never created and time out, orphaning
//     the session.
//   - '$' is rewritten to a literal backslash+'$' (tmux uses '$' as the
//     session-id prefix), which has the same round-trip failure.
//   - '#' is preserved verbatim but is tmux's format-escape character, so
//     it can corrupt status-line and display-message output that includes
//     the session name. Sanitized defensively.
//
// Other punctuation (',', ';', '@', '%', '(', etc.) is preserved verbatim
// by tmux and round-trips correctly, so we leave it alone to keep names
// recognizable.
func toTmuxName(title string, repoPath string) string {
	title = whiteSpaceRegex.ReplaceAllString(title, "")
	title = strings.ReplaceAll(title, ".", "_") // tmux silently rewrites '.' to '_'
	title = strings.ReplaceAll(title, ":", "_") // tmux silently rewrites ':' to '_'
	title = strings.ReplaceAll(title, "#", "_") // tmux treats '#' as format-escape
	title = strings.ReplaceAll(title, "$", "_") // tmux escapes '$' to '\$' in session names
	if repoPath != "" {
		return fmt.Sprintf("%s%s_%s", TmuxPrefix, repoHash(repoPath), title)
	}
	return fmt.Sprintf("%s%s", TmuxPrefix, title)
}

// NewTmuxSession creates a new TmuxSession with the given name and program (no repo scoping).
func NewTmuxSession(name string, program string) *TmuxSession {
	return newTmuxSession(toTmuxName(name, ""), program, MakePtyFactory(), cmd.MakeExecutor())
}

// NewTmuxSessionForRepo creates a new TmuxSession with a repo-scoped name to avoid collisions.
func NewTmuxSessionForRepo(name string, repoPath string, program string) *TmuxSession {
	return newTmuxSession(toTmuxName(name, repoPath), program, MakePtyFactory(), cmd.MakeExecutor())
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
