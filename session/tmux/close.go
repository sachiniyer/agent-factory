package tmux

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// errPaneQueryFoundNoPane marks a pane-PID query that tmux ANSWERED with no
// pane at all — measured: `display-message -p -t '=missing:' '#{pane_pid}'`
// exits 0 with EMPTY output for a session that does not exist.
//
// Distinct from every other panePID failure on purpose. "tmux gave me output I
// could not parse" and "tmux said there is no pane" are not the same claim, and
// only the second is evidence of absence (Codex on #2966).
var errPaneQueryFoundNoPane = errors.New("tmux reported no pane for this session")

// ErrSessionVanishedBeforeCapture marks a pane-list read that failed because
// tmux says the session does not exist. Whether that is a determinate EMPTY or a
// lost ancestry depends on the caller: see captureSessionProcessTrees.
var ErrSessionVanishedBeforeCapture = errors.New("tmux session was gone before its panes could be listed")

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
	// deadline, the pane process remained alive after bounded teardown, or nobody
	// established its state at all. No caller may take a destructive step on this:
	// deleting or moving a workspace an agent is still writing to destroys the
	// user's work on a guess. Retry instead.
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

// closeProcessOutcome carries the process-tree half of teardown separately from
// tmux's PaneState. Close remains latency-oriented and discards it; the destructive
// CloseAndWaitForPaneExit path requires both answers before authorizing workspace
// cleanup.
type closeProcessOutcome struct {
	captureErr error
	remaining  []proctree.Process
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
	state, err, _ := t.close(false)
	return state, err
}

// close is the single tmux/process teardown implementation. waitForProcesses is
// false for interactive teardown (the captured-tree reaper remains asynchronous)
// and true only when a caller will mutate the worktree immediately afterward.
func (t *TmuxSession) close(waitForProcesses bool) (PaneState, error, closeProcessOutcome) {
	var errs []error
	r := &closeRun{t: t}

	// Capture the panes' process trees before kill-session — afterwards any
	// survivor is reparented to init and its ancestry is unrecoverable
	// (#1104).
	leaked, captureErr := captureSessionProcessTrees(t.cmdExec, t.sanitizedName)

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
	processes := closeProcessOutcome{captureErr: captureErr}
	if len(leaked) > 0 {
		// Close IS the requested teardown (#2765): a caller asked for this session
		// to die, so every process in its pane tree dying with it is the operation
		// succeeding, not a leak — most visibly the `af sessions archive --self`
		// caller, which lives in this very tree and is blocked on this very call.
		if waitForProcesses {
			processes.remaining = reapSessionProcesses(reapOnRequest, t.sanitizedName, leaked, reapGraceWait, reapTermWait)
		} else {
			go reapSessionProcesses(reapOnRequest, t.sanitizedName, leaked, reapGraceWait, reapTermWait)
		}
	}

	// errors.Join, not a flattened string: the ErrTmuxTimeout sentinel has to stay
	// reachable through errors.Is for callers that gate on it (#1917). The old
	// hand-built message erased it. The state is DERIVED from the run — this
	// function never names a PaneState constant.
	return r.state(), errors.Join(errs...), processes
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
var paneExitWait = 3 * time.Second

// CloseAndWaitForPaneExit terminates the tmux session like Close, then waits for
// every process captured from the pane (root, descendants, and SID members) to
// exit or finish the bounded TERM→KILL reaper. `tmux kill-session` only delivers
// SIGHUP and returns immediately; any survivor that is still flushing state files
// (.claude/, .turbo/, ...) races directory removal and leaves a half-deleted
// worktree behind ("Directory not empty", #802). Callers that delete the session's
// worktree right after teardown must use this instead of Close.
//
// paneExitWait bounds ONLY the root fallback poll below, NOT the whole call —
// a distinction #1917 was misread on. The tmux commands are what a wedged server
// stalls, and each carries its own tmuxCommandTimeout: display-message (panePID),
// then Close's list-panes, kill-session, and at most one has-session probe. The
// captured-tree reaper adds at most reapGraceWait + reapTermWait + one final second.
// So the real worst case remains finite —
// which is the property daemon.KillSession needs, since it holds a per-session
// kills-in-flight guard across this call with no deadline of its own.
func (t *TmuxSession) CloseAndWaitForPaneExit() (PaneState, error) {
	pid, pidErr := t.panePID()
	var (
		paneProcess proctree.Process
		waitForPane bool
		processErr  error
	)
	if pidErr == nil {
		// Capture the process IDENTITY before kill-session. Polling the bare PID
		// afterwards confuses both an unreaped zombie and a recycled PID with the
		// original pane still running (#2103). The process-table identity makes
		// both distinctions explicit.
		paneProcess, waitForPane, processErr = capturePaneProcess(pid)
	}
	closeState, closeErr, processes := t.close(true)

	// ONE list of reasons the worktree may not be touched, evaluated on EVERY
	// path (#2962). The shape this replaces returned early when panePID failed,
	// which skipped the two process-set gates below entirely — so on that branch
	// a list-panes that FAILED was never consulted at all, and survivors of the
	// reap were never noticed. Three separate holes, one missing funnel.
	//
	// The rule each case serves: this function does not report whether tmux
	// answered — Close does that — it reports whether the WORKTREE MAY NOW BE
	// MUTATED. Only positive evidence that nothing is left writing licenses that,
	// and a reason we could not obtain such evidence is not a weaker yes, it is a
	// no. Callers gate on the state and merely LOG the error
	// (closeTabForDestructiveTeardown), so anything short of a no here is a
	// licence to delete.
	// When BOTH reads say the session is absent, that is a determinate empty only
	// if nothing it spawned outlived it. tmux can no longer enumerate the
	// ancestry — but the AF_SESSION marker SURVIVES the session, which is the
	// same evidence CleanupSessions uses for a vanished session (#1104).
	//
	// Checking it here is what makes the answer independent of earlier attempts.
	// A teardown that refused because a pane was observed and its ancestry lost
	// gets retried (finishUserKill), and on the retry both tmux reads report
	// absence — so without this the second attempt would launder the first
	// attempt's lost ancestry into a clean empty and authorize deleting the
	// worktree under a detached descendant (Codex on #2966, round 4).
	var vanishedSurvivors error
	if processes.captureErr != nil && sessionGoneWithNoPaneObserved(processes.captureErr, pidErr) {
		vanishedSurvivors = t.sweepVanishedSessionProcesses()
	}

	refuse := func(why error) (PaneState, error) {
		log.WarningLog.Printf("tmux session %s: %v; refusing worktree cleanup", t.sanitizedName, why)
		return PaneStateUnknown, errors.Join(closeErr, why)
	}
	switch {
	case closeState != PaneStateKnown || closeErr != nil:
		// The session's fate was not established as GONE — it either could not be
		// determined, or the kill FAILED and the probe found the session still
		// alive. Close reports the survivor case as KNOWN on purpose: its own
		// contract is best-effort (#478), "tmux answered, and this session is
		// alive". That is a fine answer to Close's question and the wrong answer
		// to this one, because a live session may still be writing to the tree the
		// caller is about to delete. Reaping before deleting is only an invariant
		// if the reap is verified to have COMPLETED (#2025/#2440); a kill that was
		// merely requested proves nothing.
		if closeErr == nil {
			return refuse(fmt.Errorf("tmux did not establish that session %s is gone", t.sanitizedName))
		}
		// pidErr AND captureErr are joined, not dropped. This branch runs before
		// the cases that would otherwise report them, so returning closeErr alone
		// erases whichever ErrTmuxTimeout they carry — the sentinel #1917 keeps
		// reachable through errors.Is for callers that classify on it. Both are
		// reachable here: the pane query can time out, and so can list-panes.
		// errors.Join ignores nils, so the ordinary case is unchanged.
		return PaneStateUnknown, errors.Join(closeErr, pidErr, processes.captureErr)
	case errors.Is(pidErr, ErrTmuxTimeout):
		// A TIMED-OUT panePID is not "nothing to wait on" (#1917). The server never
		// told us which process to wait for, so even with a successful kill we skip
		// the #802 pane-exit wait and cannot know the agent stopped writing.
		return PaneStateUnknown, errors.Join(closeErr, pidErr)
	case processErr != nil:
		// We knew which PID tmux owned, but could not establish a process identity
		// to follow across teardown. A successful kill-session is not enough to
		// prove that process stopped writing.
		return PaneStateUnknown, errors.Join(closeErr, processErr)
	case vanishedSurvivors != nil:
		// The session is gone, but the marker scan says something it spawned is
		// not — or could not be checked at all.
		return refuse(vanishedSurvivors)
	case processes.captureErr != nil && !sessionGoneWithNoPaneObserved(processes.captureErr, pidErr):
		// The full process set was not established. A child may already have
		// detached/reparented and still be writing after the leader exits; leader
		// death cannot manufacture proof about descendants we failed to see.
		//
		// Reached now even when panePID failed, which is the point: that branch
		// used to return before this check, so an unreadable pane list produced a
		// confident "safe to delete".
		return refuse(fmt.Errorf("could not establish the pane's complete process tree before kill-session: %w",
			processes.captureErr))
	case len(processes.remaining) > 0:
		pids := make([]string, 0, len(processes.remaining))
		for _, process := range processes.remaining {
			pids = append(pids, strconv.Itoa(process.PID))
		}
		return refuse(fmt.Errorf("pane processes %s are still alive after bounded teardown", strings.Join(pids, ", ")))
	case waitForPane && !waitForProcessExit(paneProcess, paneExitWait):
		// kill-session returning establishes only that SIGHUP was sent, not that
		// the process stopped writing.
		return refuse(fmt.Errorf("pane process %d is still alive %v after kill-session", pid, paneExitWait))
	}

	// Every gate passed: tmux established the session is gone, the pane's process
	// set was read, and nothing from it survived the bounded reap.
	//
	// A non-timeout pidErr is deliberately dropped here rather than reported. It
	// means tmux answered and could not name a pane — but the session-gone fact
	// comes from Close, and the nothing-still-writing fact comes from the captured
	// process set, which INCLUDES the pane leader. Neither depends on the PID
	// query, so it has nothing left to tell the caller.
	return PaneStateKnown, nil
}

// sessionGoneWithNoPaneObserved reports whether an unreadable pane set is
// actually a determinate EMPTY.
//
// It is one only when BOTH reads agree the session is not there: tmux said so on
// the pane list, AND the pane PID query never named a pane either. That is the
// ordinary teardown of an already-exited agent, and refusing it would leave those
// worktrees uncollectable forever.
//
// If a pane PID WAS observed, the same tmux answer means the opposite: the
// session exited between the two reads, the ancestry list-panes would have
// returned is lost, and descendants or SID members that outlive the leader are
// unaccounted for. Leader death cannot prove they stopped writing (#1104/#802),
// so that stays a blocker (Codex on #2966).
func sessionGoneWithNoPaneObserved(captureErr, pidErr error) bool {
	// BOTH reads must say absence, and the pane query must say it SPECIFICALLY.
	// `pidErr != nil` was too weak: it also covers "display-message returned
	// output I could not parse", which happens while the session EXISTS. Pairing
	// that with a session that then vanished before list-panes would treat a lost
	// ancestry as an empty one and authorize deleting the worktree with detached
	// descendants never captured (Codex on #2966).
	return errors.Is(pidErr, errPaneQueryFoundNoPane) &&
		errors.Is(captureErr, ErrSessionVanishedBeforeCapture)
}

// sweepVanishedSessionProcesses answers "did anything this session spawned
// outlive it?" WITHOUT asking tmux, and acts on the answer rather than merely
// reporting it.
//
// It delegates to reapVanishedSessionProcesses, the flow CleanupSessions already
// uses for exactly this situation (#1104/#2765), because the ad-hoc scan this
// replaces got two things wrong that the shared flow has right (Codex on #2966,
// round 5):
//
//   - OWNERSHIP. Matching on AF_SESSION alone counts a same-named process from
//     ANOTHER agent-factory home — a leftover from a temp/dev install — as this
//     session's survivor, refusing cleanup forever over something that is not
//     ours. markedOrphanProcesses validates AF_HOME (and uid, and process
//     identity) and silently skips a foreign home, while an UNATTRIBUTABLE
//     process still blocks, which is the right split.
//   - REAPING. Only reporting a genuine SIGHUP-immune descendant leaves the
//     tombstone retried forever: each attempt rescans, finds it again, and never
//     signals it, so the leak and the stuck worktree both persist. The shared
//     flow reaps with the bounded TERM→KILL escalation and reports only what
//     SURVIVES that.
//
// A home we cannot resolve is not evidence of absence: it means no candidate can
// be attributed, so the sweep refuses.
func (t *TmuxSession) sweepVanishedSessionProcesses() error {
	ownHome, err := afHomeDir()
	if err != nil {
		return fmt.Errorf("tmux session %s is gone and its surviving processes cannot be attributed to this "+
			"agent-factory home: %w", t.sanitizedName, err)
	}
	// nil candidates and nil captureErr on purpose: there is no captured tree to
	// refresh — the marker scan IS the evidence standing in for the ancestry tmux
	// lost, so passing the capture failure through would make it a blocker again
	// and defeat the point.
	return reapVanishedSessionProcesses(t.sanitizedName, ownHome, nil, nil)
}

// capturePaneProcess turns tmux's bare pane PID into a process-table identity
// before teardown. A PID absent from a successful snapshot is accepted as gone
// only when the kernel agrees with ESRCH. If the PID still exists, the snapshot
// was unable to identify it (or it became a zombie in the observation gap), and
// callers must keep cleanup unsafe rather than manufacturing an exit.
func capturePaneProcess(pid int) (proctree.Process, bool, error) {
	snap, err := proctree.Snapshot()
	if err != nil {
		return proctree.Process{}, false, fmt.Errorf("cannot inspect pane process %d before kill-session: %w", pid, err)
	}
	if process, ok := snap[pid]; ok {
		return process, true, nil
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return proctree.Process{}, false, nil
	} else if err != nil {
		return proctree.Process{}, false, fmt.Errorf("cannot establish whether pane process %d already exited: %w", pid, err)
	}
	return proctree.Process{}, false, fmt.Errorf("pane process %d still exists but was absent from the process-table snapshot", pid)
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
		if missingTmuxSession(err, t.sanitizedName) {
			return 0, errPaneQueryFoundNoPane
		}
		return 0, fmt.Errorf("failed to query pane pid: %w", err)
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		// tmux answered and named no pane. Measured: this is what a missing
		// session produces — exit 0, empty output — so it is the one panePID
		// failure that is evidence of ABSENCE rather than of an unreadable answer.
		return 0, errPaneQueryFoundNoPane
	}
	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("unexpected pane pid output %q", string(output))
	}
	return pid, nil
}

// waitForProcessExit waits on the pre-teardown process identity, not merely its
// PID. proctree treats zombies as exited and PID reuse as a different identity,
// so neither can masquerade as a pane that is still writing (#2103).
func waitForProcessExit(process proctree.Process, timeout time.Duration) bool {
	return len(proctree.WaitForExits([]proctree.Process{process}, timeout)) == 0
}
