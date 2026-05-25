package tmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/log"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const ProgramClaude = "claude"
const ProgramCodex = "codex"
const ProgramAider = "aider"
const ProgramGemini = "gemini"

// SupportedPrograms is the canonical list of known agent programs.
var SupportedPrograms = []string{ProgramClaude, ProgramCodex, ProgramAider, ProgramGemini}

// TmuxSession represents a managed tmux session
type TmuxSession struct {
	// Initialized by NewTmuxSession
	//
	// The name of the tmux session and the sanitized name used for tmux commands.
	sanitizedName string
	program       string
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
	// monitor monitors the tmux pane content and sends signals to the UI when it's status changes
	monitor *statusMonitor

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

// SetProgram updates the program command before the session is started.
func (t *TmuxSession) SetProgram(program string) {
	t.program = program
}

// Start creates and starts a new tmux session, then attaches to it. Program is the command to run in
// the session (ex. claude). workdir is the git worktree directory.
func (t *TmuxSession) Start(workDir string) error {
	// Check if the session already exists
	if t.DoesSessionExist() {
		return fmt.Errorf("tmux session already exists: %s", t.sanitizedName)
	}

	// Create a new detached tmux session and start claude in it
	cmd := exec.Command("tmux", "new-session", "-d", "-s", t.sanitizedName, "-c", workDir, t.program)

	ptmx, err := t.ptyFactory.Start(cmd)
	if err != nil {
		// Cleanup any partially created session if any exists.
		if t.DoesSessionExist() {
			cleanupCmd := exec.Command("tmux", "kill-session", "-t", t.sanitizedName)
			if cleanupErr := t.cmdExec.Run(cleanupCmd); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			}
		}
		return fmt.Errorf("error starting tmux session: %w", err)
	}

	// Poll for session existence with exponential backoff
	timeout := time.After(2 * time.Second)
	sleepDuration := 5 * time.Millisecond
	for !t.DoesSessionExist() {
		select {
		case <-timeout:
			ptmx.Close()
			if cleanupErr := t.Close(); cleanupErr != nil {
				err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
			}
			return fmt.Errorf("timed out waiting for tmux session %s: %v", t.sanitizedName, err)
		default:
			time.Sleep(sleepDuration)
			// Exponential backoff up to 50ms max
			if sleepDuration < 50*time.Millisecond {
				sleepDuration *= 2
			}
		}
	}
	ptmx.Close()

	// Set history limit to enable scrollback (default is 2000, we'll use 10000 for more history)
	historyCmd := exec.Command("tmux", "set-option", "-t", t.sanitizedName, "history-limit", "10000")
	if err := t.cmdExec.Run(historyCmd); err != nil {
		log.InfoLog.Printf("Warning: failed to set history-limit for session %s: %v", t.sanitizedName, err)
	}

	// Enable mouse scrolling for the session
	mouseCmd := exec.Command("tmux", "set-option", "-t", t.sanitizedName, "mouse", "on")
	if err := t.cmdExec.Run(mouseCmd); err != nil {
		log.InfoLog.Printf("Warning: failed to enable mouse scrolling for session %s: %v", t.sanitizedName, err)
	}

	// Attach to the session we just created. Pass empty workDir so a missing
	// session here surfaces as an error rather than recursively re-spawning.
	err = t.Restore("")
	if err != nil {
		if cleanupErr := t.Close(); cleanupErr != nil {
			err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
		}
		return fmt.Errorf("error restoring tmux session: %w", err)
	}

	return nil
}

// CheckAndHandleTrustPrompt checks the pane content once for a trust prompt and dismisses it if found.
// Returns true if the prompt was found and handled.
func (t *TmuxSession) CheckAndHandleTrustPrompt() bool {
	content, err := t.CapturePaneContent()
	if err != nil {
		return false
	}

	if strings.Contains(t.program, ProgramClaude) {
		if strings.Contains(content, "Do you trust the files in this folder?") ||
			strings.Contains(content, "new MCP server") {
			if err := t.TapEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust/MCP screen: %v", err)
				return false
			}
			return true
		}
	} else {
		if strings.Contains(content, "Open documentation url for more info") {
			if err := t.TapDAndEnter(); err != nil {
				log.ErrorLog.Printf("could not tap enter on trust screen: %v", err)
				return false
			}
			return true
		}
	}
	return false
}

// Restore attaches to an existing tmux session. If the session is missing
// (e.g. the tmux server died after a machine reboot, see #386) and workDir is
// non-empty, a fresh session is spawned in workDir using the same program so
// persisted instances can resume across reboots. If the session is missing
// and workDir is empty, the missing-session condition is surfaced as an error;
// real failures (PTY open errors, Start failures such as missing binaries or
// vanished worktrees) are always surfaced.
//
// When re-spawning, the program string is rewritten via resumeProgram so
// agents that expose a "resume the most recent session in cwd" flag pick
// the prior conversation back up instead of starting fresh (#595). Agents
// without such a flag, or programs that already include one, are left
// untouched.
func (t *TmuxSession) Restore(workDir string) error {
	if !t.DoesSessionExist() {
		if workDir == "" {
			return fmt.Errorf("tmux session %q does not exist", t.sanitizedName)
		}
		log.InfoLog.Printf("tmux session %q missing on Restore; re-spawning in %s", t.sanitizedName, workDir)
		t.program = resumeProgram(t.program)
		return t.Start(workDir)
	}

	attachCmd := exec.Command("tmux", "attach-session", "-t", t.sanitizedName)
	ptmx, err := t.ptyFactory.Start(attachCmd)
	if err != nil {
		return fmt.Errorf("error opening PTY: %w", err)
	}
	t.ptmx = ptmx
	t.monitor = newStatusMonitor()
	// Save a closure that SIGKILLs the attach-session child so Detach() can
	// force io.Copy(os.Stdout, t.ptmx) to unblock when the tmux server is
	// too contended to let the client exit on its own. Closing ptmx (the
	// master end) doesn't wake a blocking Read on a non-pollable character
	// device — only the slave end closing (i.e. the client child exiting)
	// does. See Detach() and the wgWaitSigkillDeadline comment for the
	// full reasoning (#598 follow-up).
	t.killAttach = func() (int, error) {
		if attachCmd.Process == nil {
			return 0, errors.New("attach process not started")
		}
		return attachCmd.Process.Pid, attachCmd.Process.Kill()
	}
	return nil
}

// waitForAttachDrain waits for the attach goroutines (io.Copy +
// monitorWindowSize x2) to finish, falling back to SIGKILLing the
// attach-session child if the wait exceeds wgWaitSigkillDeadline. The
// fallback exists because closing the PTY master (t.ptmx) does not wake a
// blocking Read on a character device — that read only returns when the
// slave end closes, which happens when the tmux client child exits, which
// requires a round-trip through a potentially contended tmux server (#598).
//
// Three-stage bound to guarantee the user-visible detach is finite even
// when our escape hatches fail (#598 follow-up regression at 00:05:14
// 2026-05-20 — a 51s hang because killAttach was nil and the post-SIGKILL
// wait was unbounded):
//
//  1. wg.Wait returns within wgWaitSigkillDeadline (the happy path).
//  2. Otherwise: try the recorded killAttach closure if present, then a
//     pgrep-based "find the tmux attach-session for this name and kill it"
//     as last-resort. Then wait at most wgWaitAbandonDeadline for the
//     stuck goroutine to drain.
//  3. Otherwise: log ERROR, return, and let the goroutine leak. The
//     kernel will eventually drain the PTY and the goroutine will exit on
//     its own — a leaked goroutine is strictly better than freezing the
//     TUI.
//
// Returns the elapsed wait so callers that surface diagnostics about a slow
// wg.Wait (Detach) can do so without re-measuring. On the abandon path
// returns wgWaitAbandonDeadline (not the literal elapsed) so the caller's
// slowDetachWgWaitThreshold check still fires cleanly.
func (t *TmuxSession) waitForAttachDrain() time.Duration {
	// Capture the WaitGroup pointer locally so the helper goroutine below
	// doesn't race against the Detach/Close defer that nils t.wg after
	// we return. The abandon path leaks the goroutine on purpose; capture
	// here means the leaked goroutine reads its own local, not a field
	// concurrent goroutines may have mutated.
	wg := t.wg
	if wg == nil {
		return 0
	}
	waitStart := time.Now()
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		return time.Since(waitStart)
	case <-time.After(wgWaitSigkillDeadline):
		// Primary fallback: SIGKILL the recorded attach process.
		if t.killAttach != nil {
			pid, killErr := t.killAttach()
			log.WarningLog.Printf("tmux: wg.Wait exceeded %v; SIGKILLing tmux attach-session pid=%d to unblock io.Copy",
				wgWaitSigkillDeadline, pid)
			if killErr != nil {
				log.WarningLog.Printf("tmux: SIGKILL attempt failed: %v", killErr)
			}
		} else {
			// Last-resort fallback: locate the attach client via pgrep -f
			// and SIGKILL by pid. We get here when the pairing invariant
			// between t.ptmx and t.killAttach was violated (a bug
			// elsewhere) — the Problem A fix should prevent this, but
			// the safety net protects against any future regression.
			log.WarningLog.Printf("tmux: wg.Wait exceeded %v but no attach process recorded; attempting pgrep-based fallback",
				wgWaitSigkillDeadline)
			if killed, err := killTmuxAttachByName(t.sanitizedName); err != nil {
				log.WarningLog.Printf("tmux: pgrep fallback failed: %v", err)
			} else if killed > 0 {
				log.WarningLog.Printf("tmux: pgrep fallback killed %d attach-session process(es) for %s",
					killed, t.sanitizedName)
			} else {
				log.WarningLog.Printf("tmux: pgrep fallback found no matching attach-session process for %s",
					t.sanitizedName)
			}
		}
		// Secondary bound: even if the SIGKILL/pgrep attempts above
		// failed (or there was nothing to kill), do not block the TUI
		// indefinitely waiting for the io.Copy goroutine to drain. If
		// it's still stuck after wgWaitAbandonDeadline more, abandon
		// the wait, leak the goroutine, and return. See the package
		// doc on wgWaitAbandonDeadline.
		select {
		case <-waitDone:
			return time.Since(waitStart)
		case <-time.After(wgWaitAbandonDeadline):
			log.ErrorLog.Printf("tmux: wg.Wait exceeded %v even after SIGKILL/pgrep fallback; "+
				"abandoning wg.Wait. The io.Copy goroutine may leak until the kernel drains the PTY. "+
				"This is preferable to freezing the TUI but indicates a deeper PTY/tmux-server issue.",
				wgWaitAbandonDeadline)
			return wgWaitSigkillDeadline + wgWaitAbandonDeadline
		}
	}
}

// pgrepRunner abstracts the "pgrep -f <pattern>" call so tests can stub
// process discovery without actually shelling out. Returns the matched
// pids (one per line of pgrep stdout) or an error if pgrep fails for a
// reason other than "no matches" (which pgrep signals with exit code 1
// and the runner reports as len(pids) == 0, nil).
type pgrepRunner func(pattern string) (pids []int, err error)

// killByPid abstracts SIGKILL-by-pid so tests can record calls without
// actually killing real processes.
type killByPidFn func(pid int) error

// pgrepRunnerVar / killByPidVar are package-level hooks tests can swap.
// Production uses defaultPgrepRunner + defaultKillByPid via exec/syscall.
var (
	pgrepRunnerVar pgrepRunner = defaultPgrepRunner
	killByPidVar   killByPidFn = defaultKillByPid
)

// killTmuxAttachByName locates tmux attach-session client(s) bound to the
// given sanitized session name via `pgrep -f` and SIGKILLs each match.
// Returns the number of processes signalled and any error encountered.
//
// The pgrep pattern is anchored to the literal `tmux attach-session ... -t
// <name>` invocation we run in Restore(), so the worst that can happen is
// missing a kill (graceful) — not killing the wrong process. A bare name
// match could collide with other tmux invocations (e.g. `tmux kill-session
// -t <name>`), which we explicitly do NOT want to interrupt mid-flight.
//
// Exit code 1 from pgrep means "no matches" and is treated as success
// with 0 kills; any other exit code is surfaced as an error.
func killTmuxAttachByName(sanitizedName string) (int, error) {
	pattern := fmt.Sprintf(`tmux attach-session -t %s$`, regexp.QuoteMeta(sanitizedName))
	pids, err := pgrepRunnerVar(pattern)
	if err != nil {
		return 0, fmt.Errorf("pgrep -f %q: %w", pattern, err)
	}
	killed := 0
	for _, pid := range pids {
		if killErr := killByPidVar(pid); killErr != nil {
			log.WarningLog.Printf("tmux: SIGKILL pid=%d (pgrep fallback) failed: %v", pid, killErr)
			continue
		}
		killed++
	}
	return killed, nil
}

// defaultPgrepRunner shells out to `pgrep -f <pattern>` and parses the
// pid list. Exit status 1 = "no matches" returns (nil, nil); any other
// non-zero exit is an error.
func defaultPgrepRunner(pattern string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing pgrep pid %q: %w", line, parseErr)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// defaultKillByPid sends SIGKILL to the given pid. ESRCH (process already
// exited) is silently ignored — the goal is "no longer a blocker", which
// an already-dead process satisfies.
func defaultKillByPid(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

type statusMonitor struct {
	// Store hashes to save memory.
	prevOutputHash []byte
	// dead is set once a capture-pane failure has been confirmed by
	// DoesSessionExist() reporting the tmux session is gone. While true,
	// HasUpdated short-circuits and emits no further logs so a stale
	// instance can't flood agent-factory.log (#489). A successful Start or
	// Restore replaces the monitor with a fresh one, which naturally clears
	// this state on respawn.
	dead bool
}

func newStatusMonitor() *statusMonitor {
	return &statusMonitor{}
}

// hash hashes the string.
func (m *statusMonitor) hash(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

// TapEnter sends an enter keystroke to the tmux pane.
func (t *TmuxSession) TapEnter() error {
	// Detach failure (or Close) clears t.ptmx (#474), so callers that fire
	// keystrokes against a detached session must surface ErrSessionGone
	// instead of panicking on a nil Write (#510).
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte{0x0D})
	if err != nil {
		return fmt.Errorf("error sending enter keystroke to PTY: %w", err)
	}
	return nil
}

// TapDAndEnter sends 'D' followed by an enter keystroke to the tmux pane.
func (t *TmuxSession) TapDAndEnter() error {
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte{0x44, 0x0D})
	if err != nil {
		return fmt.Errorf("error sending enter keystroke to PTY: %w", err)
	}
	return nil
}

func (t *TmuxSession) SendKeys(keys string) error {
	if t.ptmx == nil {
		return ErrSessionGone
	}
	_, err := t.ptmx.Write([]byte(keys))
	return err
}

// SendKeysCommand sends text to the tmux pane using the `tmux send-keys` command
// instead of writing to the PTY. This is more reliable for headless/scheduled runs
// where the PTY connection may not persist. Text is sent literally (with -l flag)
// followed by a pause to let terminal control sequences drain, then Enter to submit.
func (t *TmuxSession) SendKeysCommand(text string) error {
	// Send text literally to avoid key name interpretation
	textCmd := exec.Command("tmux", "send-keys", "-t", t.sanitizedName, "-l", text)
	if err := t.cmdExec.Run(textCmd); err != nil {
		return fmt.Errorf("error sending text via send-keys: %w", err)
	}

	// Wait for terminal control sequences (e.g. OSC color responses) to drain
	// before sending Enter, otherwise they can corrupt the input
	time.Sleep(500 * time.Millisecond)

	// Send Enter separately to submit
	enterCmd := exec.Command("tmux", "send-keys", "-t", t.sanitizedName, "Enter")
	return t.cmdExec.Run(enterCmd)
}

// HasUpdated checks if the tmux pane content has changed since the last tick. It also returns true if
// the tmux pane has a prompt for aider or claude code.
func (t *TmuxSession) HasUpdated() (updated bool, hasPrompt bool) {
	// Once the underlying tmux session has been confirmed gone, stay silent
	// instead of relogging capture-pane failures every daemon tick (#489).
	if t.monitor.dead {
		return false, false
	}

	content, err := t.CapturePaneContent()
	if err != nil {
		// If the tmux session no longer exists, log once and latch the
		// monitor as dead so the daemon's per-second poll doesn't spam
		// the log (#489). Transient capture-pane failures while the
		// session is still alive are rare and still surface every tick.
		// CapturePaneContent has already probed DoesSessionExist on the
		// error path, so use the wrapped sentinel rather than re-probing.
		if errors.Is(err, ErrSessionGone) {
			log.ErrorLog.Printf("tmux session %s is gone; status monitor going silent (capture-pane error: %v)", t.sanitizedName, err)
			t.monitor.dead = true
			return false, false
		}
		log.ErrorLog.Printf("error capturing pane content in status monitor: %v", err)
		return false, false
	}

	// Only set hasPrompt for claude and aider. Use these strings to check for a prompt.
	if strings.Contains(t.program, ProgramClaude) {
		hasPrompt = strings.Contains(content, "No, and tell Claude what to do differently")
	} else if strings.Contains(t.program, ProgramAider) {
		hasPrompt = strings.Contains(content, "(Y)es/(N)o/(D)on't ask again")
	} else if strings.Contains(t.program, ProgramGemini) {
		hasPrompt = strings.Contains(content, "Yes, allow once")
	}

	newHash := t.monitor.hash(content)
	if !bytes.Equal(newHash, t.monitor.prevOutputHash) {
		t.monitor.prevOutputHash = newHash
		return true, hasPrompt
	}
	return false, hasPrompt
}

func (t *TmuxSession) Attach() (chan struct{}, error) {
	// Detach clears t.ptmx after closing it so a Restore failure in the
	// detach path can't leave a stale closed handle behind (issue #464).
	// Refuse to attach without a live PTY rather than binding goroutines
	// to a nil or closed file and hanging.
	if t.ptmx == nil {
		return nil, fmt.Errorf("cannot attach: no PTY available, call Restore first")
	}

	t.attachCh = make(chan struct{})

	t.wg = &sync.WaitGroup{}
	t.wg.Add(1)
	t.ctx, t.cancel = context.WithCancel(context.Background())

	// The first goroutine should terminate when the ptmx is closed. We use the
	// waitgroup to wait for it to finish.
	// The 2nd one returns when you press escape to Detach. It doesn't need to be
	// in the waitgroup because is the goroutine doing the Detaching; it waits for
	// all the other ones.
	go func() {
		defer t.wg.Done()
		_, _ = io.Copy(os.Stdout, t.ptmx)
		// When io.Copy returns, it means the connection was closed
		// This could be due to normal detach or Ctrl-D
		// Check if the context is done to determine if it was a normal detach
		select {
		case <-t.ctx.Done():
			// Normal detach, do nothing
		default:
			// If context is not done, it was likely an abnormal termination (Ctrl-D)
			// Print warning message
			fmt.Fprintf(os.Stderr, "\n\033[31mError: Session terminated without detaching. Use %s to properly detach from tmux sessions.\033[0m\n", DetachKeyDisplay)
		}
	}()

	go func() {
		// Close the channel after 50ms
		timeoutCh := make(chan struct{})
		go func() {
			time.Sleep(50 * time.Millisecond)
			close(timeoutCh)
		}()

		// Read input from stdin and check for Ctrl+q
		buf := make([]byte, 32)
		for {
			nr, err := os.Stdin.Read(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				continue
			}

			// Nuke the first bytes of stdin, up to 64, to prevent tmux from reading it.
			// When we attach, there tends to be terminal control sequences like ?[?62c0;95;0c or
			// ]10;rgb:f8f8f8. The control sequences depend on the terminal (warp vs iterm). We should use regex ideally
			// but this works well for now. Log this for debugging.
			//
			// There seems to always be control characters, but I think it's possible for there not to be. The heuristic
			// here can be: if there's characters within 50ms, then assume they are control characters and nuke them.
			select {
			case <-timeoutCh:
			default:
				log.InfoLog.Printf("nuked first stdin: %s", buf[:nr])
				continue
			}

			// Check for detach key
			if nr == 1 && buf[0] == DetachKeyByte {
				// Closest point to "user pressed detach" we can observe —
				// the elapsed in this trace is whatever Detach() itself
				// took, which matches what blocks the app-side <-ch.
				log.WarningLog.Printf("[detach-trace] tmux-stdin-reader-saw-detach-key name=%s",
					t.sanitizedName)
				// Detach from the session
				t.Detach()
				return
			}

			// Forward other input to tmux
			_, _ = t.ptmx.Write(buf[:nr])
		}
	}()

	t.monitorWindowSize()
	return t.attachCh, nil
}

// DetachSafely disconnects from the current tmux session without panicking
func (t *TmuxSession) DetachSafely() error {
	// Only detach if we're actually attached
	if t.attachCh == nil {
		return nil // Already detached
	}

	var errs []error

	// Cancel context FIRST so the io.Copy goroutine in Attach() sees a normal
	// detach rather than an abnormal termination (same race fix as Detach).
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}

	// Close the attached pty session.
	if t.ptmx != nil {
		if err := t.ptmx.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing attach pty session: %w", err))
		}
		t.ptmx = nil
	}

	// Clean up attach state
	if t.attachCh != nil {
		close(t.attachCh)
		t.attachCh = nil
	}

	// Same bounded wait as Detach (#598 follow-up) — share the SIGKILL
	// fallback so the safety variant can't hang either.
	_ = t.waitForAttachDrain()
	t.wg = nil

	t.ctx = nil
	t.killAttach = nil

	if len(errs) > 0 {
		return fmt.Errorf("errors during detach: %v", errs)
	}
	return nil
}

// Detach disconnects from the current tmux session. Logs errors instead of panicking
// so the application can attempt graceful recovery.
func (t *TmuxSession) Detach() {
	detachStart := time.Now()
	log.WarningLog.Printf("[detach-trace] tmux.Detach-entry name=%s", t.sanitizedName)
	defer func() {
		log.WarningLog.Printf("[detach-trace] tmux.Detach-exit name=%s total=%v",
			t.sanitizedName, time.Since(detachStart))
		close(t.attachCh)
		t.attachCh = nil
		t.cancel = nil
		t.ctx = nil
		t.wg = nil
		// NOTE: t.killAttach is deliberately NOT cleared here. The Restore()
		// call below sets a fresh killAttach paired with the new t.ptmx; if
		// we cleared it here we'd leave the next Attach lifecycle in a
		// ptmx-valid / killAttach-nil state, which is exactly the
		// invariant break that caused the 51s detach hang in the #598
		// follow-up regression. killAttach is now paired with t.ptmx
		// inline below — set/cleared together. See the in-line clear next
		// to "t.ptmx = nil".
	}()

	// Cancel context FIRST so monitorWindowSize goroutines exit promptly and
	// the io.Copy goroutine in Attach() sees a normal detach rather than an
	// abnormal termination. Without this, closing the PTY can wake the
	// io.Copy goroutine before cancel() runs, causing a spurious
	// "Session terminated without detaching" warning.
	stepStart := time.Now()
	t.cancel()
	log.WarningLog.Printf("[detach-trace] tmux.Detach-cancel-done name=%s elapsed=%v",
		t.sanitizedName, time.Since(stepStart))

	// Close the attached pty session so the io.Copy goroutine returns.
	stepStart = time.Now()
	closeErr := t.ptmx.Close()
	log.WarningLog.Printf("[detach-trace] tmux.Detach-ptmx.Close-done name=%s elapsed=%v",
		t.sanitizedName, time.Since(stepStart))

	// Wait for the attach goroutines (io.Copy + monitorWindowSize x2) to
	// finish before mutating t.ptmx. monitorWindowSize reads t.ptmx via
	// updateWindowSize, so clearing the field before wg.Wait races against
	// those reads (#512). waitForAttachDrain bounds the wait by SIGKILLing
	// the attach-session child if wg.Wait exceeds wgWaitSigkillDeadline —
	// see #598 follow-up for the diagnosis.
	waitElapsed := t.waitForAttachDrain()
	log.WarningLog.Printf("[detach-trace] tmux.Detach-wg.Wait-done name=%s elapsed=%v",
		t.sanitizedName, waitElapsed)
	// Defense-in-depth: if wg.Wait still exceeded the slow threshold after
	// the SIGKILL fallback ran, that means killAttach didn't unstick the
	// goroutine — a deeper bug than what this fix targets. Keep the loud
	// log so we hear about it.
	if waitElapsed > slowDetachWgWaitThreshold {
		log.ErrorLog.Printf("tmux.Detach: wg.Wait took %v — likely tmux server "+
			"contention from background capture-pane operations. Sessions paused "+
			"while attached should have prevented this; bug?", waitElapsed)
	}

	// Now safe to clear t.ptmx. Clearing unconditionally before Restore
	// means a Restore failure (or a Close failure) can't leave the closed
	// handle dangling on the struct — a subsequent Attach would otherwise
	// silently bind goroutines to a closed file and hang (#464).
	// Pair the clear with t.killAttach: the closure references the dying
	// attachCmd whose process is being torn down, so it must not survive
	// past this point. Restore() below will assign both fields together
	// for the next attach lifecycle; this is the invariant the #598
	// follow-up regression broke.
	t.ptmx = nil
	t.killAttach = nil

	if closeErr != nil {
		log.ErrorLog.Printf("error closing attach pty session: %v", closeErr)
		return
	}

	// Call t.Restore to set a new t.ptmx. The session is alive (we just
	// detached from it), so pass empty workDir — a missing session here is a
	// real problem and should surface, not silently re-spawn and lose history.
	stepStart = time.Now()
	if err := t.Restore(""); err != nil {
		log.ErrorLog.Printf("error restoring pty after detach: %v", err)
	}
	log.WarningLog.Printf("[detach-trace] tmux.Detach-Restore-done name=%s elapsed=%v",
		t.sanitizedName, time.Since(stepStart))
}

// Close terminates the tmux session and cleans up resources
func (t *TmuxSession) Close() error {
	var errs []error

	// Coordinate with any in-flight Attach goroutines (mirrors Detach):
	// cancel context first so monitorWindowSize goroutines exit before we
	// nil out t.ptmx, otherwise they can race against updateWindowSize and
	// panic dereferencing a nil PTY. Safe to call when Attach was never
	// invoked because cancel/wg are only set by Attach.
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}

	if t.ptmx != nil {
		if err := t.ptmx.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing PTY: %w", err))
		}
	}

	// Same bounded wait as Detach (#598 follow-up). The tmux kill-session
	// below will eventually force the attach client to exit on its own, but
	// that depends on the same tmux-server round-trip that #598 showed can
	// stall for tens of seconds. Sharing the SIGKILL fallback keeps Close
	// snappy when used from user-driven teardown paths (terminal pane
	// close).
	_ = t.waitForAttachDrain()
	t.wg = nil

	t.ptmx = nil
	t.ctx = nil
	t.killAttach = nil

	if t.attachCh != nil {
		close(t.attachCh)
		t.attachCh = nil
	}

	cmd := exec.Command("tmux", "kill-session", "-t", t.sanitizedName)
	if err := t.cmdExec.Run(cmd); err != nil {
		errs = append(errs, fmt.Errorf("error killing tmux session: %w", err))
	}

	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}

	errMsg := "multiple errors occurred during cleanup:"
	for _, err := range errs {
		errMsg += "\n  - " + err.Error()
	}
	return errors.New(errMsg)
}

// SetDetachedSize set the width and height of the session while detached. This makes the
// tmux output conform to the specified shape.
func (t *TmuxSession) SetDetachedSize(width, height int) error {
	// Detach failure (or Close) clears t.ptmx (#474), and the tmux session
	// may have been killed externally. Guard the ioctl so a missing PTY
	// surfaces as ErrSessionGone instead of panicking on nil.Fd() or
	// logging "bad file descriptor" at ERROR (#496).
	if t.ptmx == nil {
		return ErrSessionGone
	}
	if err := t.updateWindowSize(width, height); err != nil {
		if !t.DoesSessionExist() {
			return fmt.Errorf("%w: %v", ErrSessionGone, err)
		}
		return err
	}
	return nil
}

// updateWindowSize updates the window size of the PTY.
func (t *TmuxSession) updateWindowSize(cols, rows int) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
		X:    0,
		Y:    0,
	})
}

func (t *TmuxSession) DoesSessionExist() bool {
	// Using "-t name" does a prefix match, which is wrong. `-t=` does an exact match.
	existsCmd := exec.Command("tmux", "has-session", fmt.Sprintf("-t=%s", t.sanitizedName))
	return t.cmdExec.Run(existsCmd) == nil
}

// CapturePaneContent captures the content of the tmux pane. When the
// capture fails and DoesSessionExist confirms the session is gone, the
// returned error wraps ErrSessionGone so non-daemon callers can degrade
// gracefully instead of logging at ERROR (#496).
func (t *TmuxSession) CapturePaneContent() (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes)
	cmd := exec.Command("tmux", "capture-pane", "-p", "-e", "-J", "-t", t.sanitizedName)
	output, err := t.cmdExec.Output(cmd)
	if err != nil {
		if !t.DoesSessionExist() {
			return "", fmt.Errorf("%w: capture-pane: %v", ErrSessionGone, err)
		}
		return "", fmt.Errorf("error capturing pane content: %v", err)
	}
	return string(output), nil
}

// CapturePaneContentWithOptions captures the pane content with additional options
// start and end specify the starting and ending line numbers (use "-" for the start/end of history).
// Wraps ErrSessionGone when the session has vanished, mirroring CapturePaneContent.
func (t *TmuxSession) CapturePaneContentWithOptions(start, end string) (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes)
	cmd := exec.Command("tmux", "capture-pane", "-p", "-e", "-J", "-S", start, "-E", end, "-t", t.sanitizedName)
	output, err := t.cmdExec.Output(cmd)
	if err != nil {
		if !t.DoesSessionExist() {
			return "", fmt.Errorf("%w: capture-pane: %v", ErrSessionGone, err)
		}
		return "", fmt.Errorf("failed to capture tmux pane content with options: %v", err)
	}
	return string(output), nil
}

// CleanupSessions kills all tmux sessions that start with "session-"
func CleanupSessions(cmdExec cmd.Executor) error {
	// First try to list sessions
	cmd := exec.Command("tmux", "ls")
	output, err := cmdExec.Output(cmd)

	// If there's an error and it's because no server is running, that's fine
	// Exit code 1 typically means no sessions exist
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil // No sessions to clean up
		}
		return fmt.Errorf("failed to list tmux sessions: %v", err)
	}

	// Anchor to start-of-line so `af_` embedded in a non-agent session name
	// (e.g. `my_af_project:`) is never matched and killed (#613).
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^%s[^:]*:`, regexp.QuoteMeta(TmuxPrefix)))
	matches := re.FindAllString(string(output), -1)
	for i, match := range matches {
		matches[i] = match[:strings.Index(match, ":")]
	}

	for _, match := range matches {
		log.InfoLog.Printf("cleaning up session: %s", match)
		if err := cmdExec.Run(exec.Command("tmux", "kill-session", "-t", match)); err != nil {
			return fmt.Errorf("failed to kill tmux session %s: %v", match, err)
		}
	}
	return nil
}
