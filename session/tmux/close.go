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

// PaneState is what a bounded teardown could ESTABLISH about a tmux session, and
// it is returned SEPARATELY from the error on purpose (#1917).
//
// Bounding the tmux commands introduced a third answer next to "killed" and
// "failed to kill": "the server never replied, so I do not know". That answer has
// to reach the caller who deletes or moves the workspace, and returning it only as
// an error type did not work — four separate layers reduced it to
// log-the-error-and-carry-on, which is byte-for-byte identical to having no
// timeout at all, and each one ended in a destructive step running against a
// session that might still be alive.
//
// An error can be swallowed by accident; a second return value cannot. Every
// caller must name it, and a caller that wants to ignore it has to write the
// ignore down. That is the whole point of the type: it makes the unknown case
// impossible to drop silently rather than merely possible to detect.
type PaneState int

const (
	// PaneStateUnknown (the ZERO VALUE): a bounded tmux command tripped its
	// deadline, so the server never answered and the session may still be RUNNING —
	// or nobody established its state at all. No caller may take a destructive step
	// on this: deleting or moving a workspace an agent is still writing to destroys
	// the user's work on a guess. Retry instead.
	//
	// Unknown is the zero value deliberately (#1917). The safe outcome must be the
	// LAZY outcome: a state nobody set refuses to destroy rather than permitting it.
	PaneStateUnknown PaneState = iota
	// PaneStateKnown: every tmux command in the teardown ANSWERED. The session is
	// gone, or it survived a kill tmux reported on — either way its state was
	// established, and the caller's own best-effort contract (#478/#967) governs.
	PaneStateKnown
)

// Close terminates the tmux session and cleans up resources. It reports whether
// tmux actually established the session's fate (see PaneState) alongside any
// error: callers that go on to touch the session's workspace MUST gate on the
// state, not on the error.
//
// Post-#1592-PR7 a TmuxSession holds no attach PTY or client child (the
// tmux-server-mediated attach driver was retired), so Close is now just
// kill-session plus the leaked-process reap — no PTY close, no attach-goroutine
// drain, no killAttach/termAttach coordination.
// closeRun executes ONE Close and OWNS its state, mirroring git's cleanupRun.
//
// Close used to assert PaneStateKnown up front and downgrade by hand at each place
// a deadline could trip — and a missed one (the has-session probe) shipped, letting
// a caller delete a workspace whose session tmux had never confirmed dead. The
// author no longer writes the state: every bounded tmux command goes through
// run.tmux, which records a tripped deadline, and state() derives the answer.
type closeRun struct {
	t       *TmuxSession
	unknown bool
}

// tmux runs one bounded tmux command and RECORDS a tripped deadline. The only
// place in the close path that decides what a deadline means.
func (r *closeRun) tmux(args ...string) error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	err := r.t.runTmuxBounded(ctx, args...)
	if err != nil && ctx.Err() != nil {
		r.unknown = true
		return fmt.Errorf("%w: %s after %s", ErrTmuxTimeout, args[0], tmuxCommandTimeout)
	}
	return err
}

// probe asks whether the session still exists. A timed-out probe marks the run
// unknown via the shared probe helper and reports ok=false, so "could not ask" is
// never read as "not there".
func (r *closeRun) probe() (exists bool, ok bool) {
	exists, known := probeSession(r.t.cmdExec, r.t.sanitizedName)
	if !known {
		r.unknown = true
	}
	return exists, known
}

func (r *closeRun) state() PaneState {
	if r.unknown {
		return PaneStateUnknown
	}
	return PaneStateKnown
}

func (t *TmuxSession) Close() (PaneState, error) {
	var errs []error
	r := &closeRun{t: t}

	// Capture the panes' process trees before kill-session — afterwards any
	// survivor is reparented to init and its ancestry is unrecoverable
	// (#1104).
	leaked := SessionProcessTrees(t.cmdExec, t.sanitizedName)

	// Bounded by tmuxCommandTimeout (#1917), through the run so the deadline counts
	// itself: an unbounded kill-session against a wedged server blocks
	// daemon.KillSession forever behind its kills-in-flight guard, leaving the
	// session undeletable until the daemon restarts.
	if err := r.tmux("kill-session", "-t", exactTarget(t.sanitizedName)); err != nil {
		switch {
		case r.unknown:
			// r.tmux already wrapped this as ErrTmuxTimeout.
			errs = append(errs, err)
			// The deadline tripped, so the session's real state is UNKNOWN. Do NOT
			// probe has-session: it would spawn another tmux command against the same
			// wedged server and hang identically, defeating the bound we just came
			// here for (see tmuxTimeoutContext). A session we cannot confirm dead may
			// well be alive, and its processes are then not leaks.
			leaked = nil
		default:
			// kill-session ANSWERED with a failure, fast. Ask what actually happened —
			// but the probe is bounded too, so it has three answers, not two. A
			// timed-out probe marks the run unknown inside r.probe, so the caller
			// learns the fate is unknown instead of receiving an ordinary kill error
			// and deleting the workspace on it.
			exists, ok := r.probe()
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("%w: has-session probe after kill-session failed (%v)", ErrTmuxTimeout, err))
				leaked = nil
			case exists:
				errs = append(errs, fmt.Errorf("error killing tmux session: %w", err))
				// Idempotent teardown (#967): a kill-session that fails because the
				// session is already gone has achieved Close's goal — a dead session is
				// the desired end state. Only a session that survives the kill is a
				// genuine failure. Probe has-session rather than matching tmux's bare
				// "exit status 1", which it reuses for unrelated errors.
				//
				// The state stays KNOWN: tmux answered, and this session is alive.
				// Callers keep their pre-#1917 best-effort contract here (#478) — see
				// the note on that trade in teardownKill.
				//
				// The session survived — its processes are not leaks. Do not reap.
				leaked = nil
			}
		}
	}

	// Async so the SIGHUP grace period never adds latency to user-driven
	// teardown; the daemon and TUI processes are long-lived, so the sweep
	// always gets to finish. CLI kills run daemon-side (KillSession RPC).
	if len(leaked) > 0 {
		go reapLeakedProcesses(t.sanitizedName, leaked, reapGraceWait, reapTermWait)
	}

	// errors.Join, not a flattened string: the ErrTmuxTimeout sentinel has to stay
	// reachable through errors.Is for callers that gate on it (#1917). The old
	// hand-built message erased it. The state is DERIVED from the run — this
	// function never names a PaneState constant.
	return r.state(), errors.Join(errs...)
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
func (t *TmuxSession) CloseAndWaitForPaneExit() (PaneState, error) {
	pid, pidErr := t.panePID()
	state, closeErr := t.Close()
	if pidErr != nil {
		// A TIMED-OUT panePID is not "nothing to wait on" (#1917). It means the
		// server never told us which process to wait for — so even if the
		// kill-session that follows succeeds, we skip the #802 pane-exit wait and
		// have no way to know the agent stopped writing. Returning the successful
		// Close's KNOWN state here would tell the caller it may delete the worktree
		// while the HUP'd agent is still flushing into it. Keep the unknown, and
		// keep the sentinel reachable.
		if errors.Is(pidErr, ErrTmuxTimeout) {
			return PaneStateUnknown, errors.Join(closeErr, pidErr)
		}
		// Any other panePID failure means tmux ANSWERED: the session is already
		// gone or the pane is unqueryable, so there is genuinely nothing to wait on
		// and Close's own state stands.
		return state, closeErr
	}
	if !waitForPIDExit(pid, paneExitWait) {
		// Pre-existing #802 behavior, deliberately unchanged: kill-session was
		// CONFIRMED delivered, so this pane is dying — it is merely slow. Treating a
		// slow flush as unknown would defer routine kills of any agent that takes
		// >3s to exit. The unknown cases above are the ones where tmux never spoke.
		log.WarningLog.Printf("tmux session %s: pane process %d still alive %v after kill-session; "+
			"worktree cleanup may race with its in-flight writes", t.sanitizedName, pid, paneExitWait)
	}
	return state, closeErr
}

// panePID returns the PID of the root process running in the session's pane
// (the agent program). Must be called before kill-session — afterwards there
// is nothing left to query.
func (t *TmuxSession) panePID() (int, error) {
	// exactTarget forces an exact session match, mirroring ExistsOrUnknown.
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
