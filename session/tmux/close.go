package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Close terminates the tmux session and cleans up resources
func (t *TmuxSession) Close() error {
	t.attachMu.Lock()
	defer t.attachMu.Unlock()

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
	t.termAttach = nil

	t.closeAttachChLocked()

	// Capture the panes' process trees before kill-session — afterwards any
	// survivor is reparented to init and its ancestry is unrecoverable
	// (#1104).
	leaked := SessionProcessTrees(t.cmdExec, t.sanitizedName)

	cmd := exec.Command("tmux", "kill-session", "-t", exactTarget(t.sanitizedName))
	if err := t.cmdExec.Run(cmd); err != nil {
		// Idempotent teardown (#967): a kill-session that fails because the
		// session is already gone has achieved Close's goal — a dead session
		// is the desired end state. Only a session that survives the kill is a
		// genuine failure. Probe has-session rather than matching tmux's bare
		// "exit status 1", which it reuses for unrelated errors. Mirrors the
		// `_`-ignored cleanup kill in Start (above).
		if t.DoesSessionExist() {
			errs = append(errs, fmt.Errorf("error killing tmux session: %w", err))
			// The session survived — its processes are not leaks. Do not reap.
			leaked = nil
		}
	}

	// Async so the SIGHUP grace period never adds latency to user-driven
	// teardown; the daemon and TUI processes are long-lived, so the sweep
	// always gets to finish. CLI kills run daemon-side (KillSession RPC).
	if len(leaked) > 0 {
		go reapLeakedProcesses(t.sanitizedName, leaked, reapGraceWait, reapTermWait)
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

// CloseAttachOnly tears down this client's connection to the tmux session — it
// closes the attach PTY and SIGKILLs the `tmux attach-session` child process —
// WITHOUT running `tmux kill-session`. The server-side session, and any
// worktree behind it, are left running.
//
// It is the non-destructive sibling of Close: Close kills the session,
// CloseAttachOnly only releases the resources this particular TmuxSession
// object opened in Restore/Attach. The daemon uses it to reclaim the PTY held
// by a throwaway TmuxSession built from disk (#867) when the canonical,
// still-tracked instance shares the same live session — calling Close there
// would kill that session out from under the canonical instance.
func (t *TmuxSession) CloseAttachOnly() error {
	t.attachMu.Lock()
	defer t.attachMu.Unlock()

	var errs []error

	// Mirror Close/Detach ordering: cancel any Attach goroutines before
	// touching t.ptmx so monitorWindowSize can't race a nil PTY. In the
	// daemon's Restore-only path these are nil and this is a no-op.
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}

	if t.ptmx != nil {
		if err := t.ptmx.Close(); err != nil {
			errs = append(errs, fmt.Errorf("error closing PTY: %w", err))
		}
	}

	// Best-effort SIGKILL of the attach-session child so the client process
	// doesn't linger. Closing the PTY master above already hangs up the
	// client's controlling terminal, so this is belt-and-suspenders: a failure
	// (process never started, or already exited) is benign and must not fail
	// the close — the leaked-fd reclamation that #867 is about is the PTY close
	// above, not this. It detaches the client only; it never kills the session.
	if t.killAttach != nil {
		if _, err := t.killAttach(); err != nil {
			log.WarningLog.Printf("tmux %s: best-effort kill of attach process failed: %v", t.sanitizedName, err)
		}
	}

	_ = t.waitForAttachDrain()
	t.wg = nil

	t.ptmx = nil
	t.ctx = nil
	t.killAttach = nil
	t.termAttach = nil

	t.closeAttachChLocked()

	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	errMsg := "multiple errors occurred during attach close:"
	for _, err := range errs {
		errMsg += "\n  - " + err.Error()
	}
	return errors.New(errMsg)
}

// paneExitWait bounds how long CloseAndWaitForPaneExit blocks for the pane
// process to die. Long enough for an agent to handle SIGHUP and flush state,
// short enough that teardown of a wedged process doesn't hang the caller.
const paneExitWait = 3 * time.Second

// CloseAndWaitForPaneExit terminates the tmux session like Close, then waits
// (bounded by paneExitWait) until the pane's root process has actually
// exited. `tmux kill-session` only delivers SIGHUP and returns immediately;
// an agent that is still flushing state files (.claude/, .turbo/, ...) races
// any directory removal that follows and leaves a half-deleted worktree
// behind ("Directory not empty", #802). Callers that delete the session's
// worktree right after teardown must use this instead of Close.
func (t *TmuxSession) CloseAndWaitForPaneExit() error {
	pid, pidErr := t.panePID()
	closeErr := t.Close()
	if pidErr != nil {
		// Session already gone (or unqueryable) — nothing to wait on.
		return closeErr
	}
	if !waitForPIDExit(pid, paneExitWait) {
		log.WarningLog.Printf("tmux session %s: pane process %d still alive %v after kill-session; "+
			"worktree cleanup may race with its in-flight writes", t.sanitizedName, pid, paneExitWait)
	}
	return closeErr
}

// panePID returns the PID of the root process running in the session's pane
// (the agent program). Must be called before kill-session — afterwards there
// is nothing left to query.
func (t *TmuxSession) panePID() (int, error) {
	// exactTarget forces an exact session match, mirroring DoesSessionExist.
	// (The bare `=name` form returns an empty pane_pid for display-message —
	// the trailing `:` in exactTarget is what makes the pid resolve. See #1006.)
	cmd := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(t.sanitizedName), "#{pane_pid}")
	output, err := t.cmdExec.Output(cmd)
	if err != nil {
		return 0, fmt.Errorf("failed to query pane pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("unexpected pane pid output %q", string(output))
	}
	return pid, nil
}

// waitForPIDExit polls pid with signal 0 until the process is gone or the
// timeout elapses. Returns true when the process exited within the timeout.
// The tmux server reaps its dead children promptly, so a lingering zombie
// (signal 0 succeeds on zombies) does not realistically pin this to the full
// timeout.
func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
