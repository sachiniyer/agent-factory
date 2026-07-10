package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

const ProgramClaude = "claude"
const ProgramCodex = "codex"
const ProgramAider = "aider"
const ProgramGemini = "gemini"
const ProgramAmp = "amp"

// SupportedPrograms is the canonical list of known agent programs.
var SupportedPrograms = []string{ProgramClaude, ProgramCodex, ProgramAider, ProgramGemini, ProgramAmp}

// SupportedProgramsString returns the canonical user-facing agent enum list.
func SupportedProgramsString() string {
	return strings.Join(SupportedPrograms, ", ")
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
	// ptmx is a PTY is running the tmux attach command. This can be resized to change the
	// stdout dimensions of the tmux pane. On detach, we close it and set a new one.
	// This should never be nil.
	ptmx *os.File
	// monitor monitors the tmux pane content and sends signals to the UI when it's status changes.
	// The pointer is swapped by Restore() (a fresh monitor on every (re)attach) on the
	// restore/RPC/event-loop goroutines while the daemon's per-second poll reads it — and mutates
	// its dead/prevOutputHash fields — inside HasUpdated(). Every access therefore goes through the
	// monitorMu-guarded helpers in monitor.go; never touch these two fields directly (#1528).
	monitor   *statusMonitor
	monitorMu sync.Mutex

	// Initialized by Attach
	// Deinitilaized by Detach
	//
	// Channel to be closed at the very end of detaching. Used to signal callers.
	attachCh chan struct{}
	// While attached, we use some goroutines to manage the window size and stdin/stdout. This stuff
	// is used to terminate them on Detach. We don't want them to outlive the attached window.
	ctx    context.Context
	cancel func()
	wg     *sync.WaitGroup

	// killAttach SIGKILLs the tmux attach-session client child whose slave end
	// of the PTY is keeping io.Copy(os.Stdout, t.ptmx) blocked in Attach().
	// Set by Restore() once the PTY-backed child is running; cleared by the
	// detach/teardown paths after wg drains. Returns (pid, err) so the
	// fallback log can name the pid that was signalled. See Detach() for the
	// "why" — this is the defensive escape hatch for #598 when the tmux
	// server is too contended to let the client exit on its own.
	killAttach func() (int, error)

	// termAttach SIGTERMs that same attach-session child. Detach() signals it
	// proactively (SIGTERM → short grace → SIGKILL backstop) so io.Copy
	// unblocks without a tmux-server round-trip, instead of racing the 1s
	// SIGKILL deadline against the daemon's capture-pane poll — the #1157
	// fix for the ~32% of detaches that used to stall the full second. Set
	// and cleared in lockstep with killAttach (same pairing invariant that
	// the #602 regression broke — see Detach's inline clear).
	termAttach func() (int, error)

	// attachMu serializes attach lifecycle teardown. Detach is invoked by an
	// Attach-spawned stdin goroutine that is intentionally outside wg, so Close
	// cannot rely on wg.Wait before touching attachCh/cancel/ptmx (#1477).
	attachMu sync.Mutex
}

const TmuxPrefix = "af_"

// slowDetachWgWaitThreshold is the wg.Wait elapsed above which Detach()
// emits an ErrorLog entry. Picked above the worst recorded non-pathological
// wait (~120ms during normal io.Copy drain) and well below the smallest
// observed pathological wait (~42s in #598), so the threshold cleanly
// separates "noise" from "regression of the contention fix". Exported as
// a var so future regressions can be detected without recompiling.
var slowDetachWgWaitThreshold = 5 * time.Second

// wgWaitSigkillDeadline is the wg.Wait elapsed above which the
// detach/teardown paths SIGKILL the tmux attach-session child to force
// io.Copy to return. The 1s value is generous enough to absorb routine
// kernel scheduling but short enough that the user-visible hang is bounded
// regardless of tmux-server load — even with the pause-while-attached gate
// in app/app.go, the daemon (separate process) still polls capture-pane
// every second and can contend with the attach client's exit round-trip
// (see the #598 follow-up). var, not const, so tests can lower it.
var wgWaitSigkillDeadline = 1 * time.Second

// wgWaitAbandonDeadline bounds the secondary wait after the SIGKILL/pgrep
// fallback has already fired. If wg.Wait still hasn't returned by this
// deadline, the io.Copy goroutine is wedged in a way our escape hatches
// can't unstick (kernel-level PTY drain bug, syscall stuck in D-state).
// Leaking that one goroutine until the OS eventually drains the PTY is
// strictly better than holding the user's TUI hostage for tens of seconds
// — the original incident in #598 was a 51s hang at 00:05:14 because the
// fallback was missing. Set to 2× wgWaitSigkillDeadline so the total
// worst-case detach is ~3s. var, not const, so tests can lower it.
var wgWaitAbandonDeadline = 2 * time.Second

// proactiveGraceDeadline bounds how long Detach() waits for the attach
// goroutines to drain after a proactive SIGTERM (termAttach) before falling
// through to waitForAttachDrain's SIGKILL backstop. Signalling the child
// directly bypasses the tmux server, so a healthy client's io.Copy unblocks
// within a scheduler tick or two; 150ms is generous headroom over that while
// keeping a wedged detach an order of magnitude faster than the old 1s
// wgWaitSigkillDeadline race against the daemon's capture-pane poll (#1157).
// var, not const, so tests can lower it.
var proactiveGraceDeadline = 150 * time.Millisecond

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
// replaced here so DoesSessionExist() and kill paths match the name tmux
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
