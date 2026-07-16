package tmux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Close terminates the tmux session and cleans up resources.
//
// Post-#1592-PR7 a TmuxSession holds no attach PTY or client child (the
// tmux-server-mediated attach driver was retired), so Close is now just
// kill-session plus the leaked-process reap — no PTY close, no attach-goroutine
// drain, no killAttach/termAttach coordination.
func (t *TmuxSession) Close() error {
	var errs []error

	// Capture the panes' process trees before kill-session — afterwards any
	// survivor is reparented to init and its ancestry is unrecoverable
	// (#1104).
	leaked := SessionProcessTrees(t.cmdExec, t.sanitizedName)

	// Bounded by tmuxCommandTimeout (#1917): an unbounded kill-session against a
	// wedged server blocks daemon.KillSession forever behind its kills-in-flight
	// guard, leaving the session undeletable until the daemon restarts.
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "kill-session", "-t", exactTarget(t.sanitizedName)); err != nil {
		switch {
		case ctx.Err() != nil:
			// The deadline tripped, so the session's real state is UNKNOWN. Do
			// NOT probe has-session: it would spawn another tmux command against
			// the same wedged server and hang identically, defeating the bound we
			// just came here for (see tmuxTimeoutContext). Report the timeout and
			// take the conservative branch — a session we cannot confirm dead may
			// well be alive, and its processes are then not leaks.
			errs = append(errs, fmt.Errorf("%w: kill-session after %s", ErrTmuxTimeout, tmuxCommandTimeout))
			leaked = nil
		case t.DoesSessionExist():
			// Idempotent teardown (#967): a kill-session that fails because the
			// session is already gone has achieved Close's goal — a dead session
			// is the desired end state. Only a session that survives the kill is a
			// genuine failure. Probe has-session rather than matching tmux's bare
			// "exit status 1", which it reuses for unrelated errors. Mirrors the
			// `_`-ignored cleanup kill in Start (above).
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

// CloseAttachOnly is the non-destructive sibling of Close: it releases whatever
// this particular TmuxSession object opened to view/drive the session, WITHOUT
// running `tmux kill-session`. The daemon uses it to discard a duplicate
// Instance built from disk (#867) when the canonical, still-tracked instance
// shares the same live session — killing there would tear down state the
// canonical instance shares.
//
// Post-#1592-PR7 a TmuxSession holds no attach PTY or client child (the
// tmux-server-mediated attach driver was retired) and Restore opens no render
// client, so a throwaway TmuxSession has nothing to release at this layer — the
// clientless WS broker/pipe-pane data plane is owned by the agent-server, on its
// own lifecycle. The method stays on the interface for the #867 discard path;
// its body is now a no-op.
func (t *TmuxSession) CloseAttachOnly() error {
	return nil
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
//
// paneExitWait bounds ONLY the waitForPIDExit poll below, NOT the whole call —
// a distinction #1917 was misread on. The tmux commands are what a wedged server
// stalls, and each carries its own tmuxCommandTimeout: display-message (panePID),
// then Close's list-panes, kill-session, and at most one has-session probe. So the
// real worst case is ~4×tmuxCommandTimeout + paneExitWait, all of it finite —
// which is the property daemon.KillSession needs, since it holds a per-session
// kills-in-flight guard across this call with no deadline of its own.
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
	//
	// Bounded by tmuxCommandTimeout (#1917): this is the FIRST tmux command on
	// the kill teardown, so an unbounded stall here wedges the kill before
	// kill-session is even attempted.
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), "#{pane_pid}")
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("%w: display-message pane_pid after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
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
