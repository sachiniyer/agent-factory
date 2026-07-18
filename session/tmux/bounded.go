package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// Bounded tmux control commands (#1787).
//
// The clientless WS PTY paths drive tmux with short, purely-local control
// commands (capture-pane, display-message, pipe-pane). They are sub-100ms when
// the tmux server is healthy, but `exec.Command` gives them no deadline at all:
// a wedged tmux server (one stuck under load, blocked on a stalled pane, or
// hung on its socket) parks the tmux CLIENT forever, and the Go caller blocks
// with it. Two paths turn that into a user-visible hang:
//
//   - The WS stream handler resolves Subscribe() — which snapshots the pane via
//     CaptureVisiblePaneGrid/CursorPosition — BEFORE websocket.Accept sends the
//     101, so a stalled capture means the client never gets a socket. The unix
//     transport has no handshake timeout and the HTTP server deliberately sets
//     no WriteTimeout, so nothing else bounds it.
//   - EnablePipePane/DisablePipePane run while the broker holds captureMu. A
//     stall there strands the mutex, so EVERY later capture start/stop for that
//     session deadlocks behind it — not just the one that was unlucky.
//
// Bounding the commands themselves fixes both: the deadline is what makes the
// captureMu hold finite, so no lock restructuring is needed. CapturePaneContentContext
// already established the exec.CommandContext pattern here; this generalizes it
// for the commands that had no context to thread.
//
// #1917 extended the same treatment to the TEARDOWN commands (kill-session,
// has-session, display-message #{pane_pid}, list-panes), which had stayed on bare
// exec.Command. daemon.KillSession runs the whole teardown with no deadline of its
// own while holding a per-session kills-in-flight guard, so a single unbounded tmux
// call there does not merely stall one kill: it makes that session permanently
// undeletable for the daemon's entire lifetime.
//
// #2099/#2105 closed the last gaps, which were regressions against the invariant
// this comment used to assert outright ("every tmux command in this package is now
// bounded") while three capture paths were in fact still unbounded — the claim was
// load-bearing enough that later code trusted it instead of checking:
//
//   - The CAPTURE/POLL paths: CapturePaneContent (the daemon's per-second status
//     poll, via HasUpdated), CapturePaneContentWithOptions, and the submit path's
//     capturePaneForDelivery. The daemon polls instances SEQUENTIALLY, so one
//     wedged session froze the status of every instance behind it.
//   - CapturePaneContentContext had a ctx but ran on plain exec.CommandContext,
//     which sets no WaitDelay and kills only the direct process — so a child
//     holding the inherited stdout pipe kept Output() blocked on pipe EOF and
//     silently defeated the deadline. A ctx alone is NOT a bound; it has to go
//     through boundedTmuxCommand.
//   - The remaining send-keys/buffer/sweep commands (TapEnter, TapDAndEnter,
//     sendEnter, load-buffer, paste-buffer, delete-buffer, show-environment,
//     tmux -V, tmux ls, CleanupSessions' kill-session).
//
// The invariant is real again, and stated as an obligation rather than a fact:
// every tmux command in this package MUST be bounded, and session/tmux's wedge
// tests (bounded_test.go, kill_wedge_test.go, capture_wedge_test.go) are what
// hold new ones to it. The one deliberate exception is start.go's `new-session`,
// which is not a control command: it is handed to the PtyFactory as the session's
// own long-lived process, and its lifetime is managed by the ptmx, not a deadline.
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var tmuxCommandTimeout = 10 * time.Second

// tmuxWaitDelay bounds how long cmd.Wait blocks after the tmux process exits or
// is killed on the deadline, before the inherited stdout/stderr pipes are
// force-closed. Killing tmux does not necessarily close the capture pipe: any
// process that inherited it (tmux runs pipe-pane's shell command through
// /bin/sh, and a hung server can leave a child holding the fd) keeps the read
// end open, and Output()/Run() block on pipe EOF until it dies — which would
// silently defeat the deadline above. Mirrors gitWaitDelay (#856/#896).
const tmuxWaitDelay = 2 * time.Second

// ErrTmuxTimeout marks a tmux command that was killed on tmuxCommandTimeout
// rather than failing on its own. Callers distinguish it from ErrSessionGone:
// a timeout means the server is wedged and the session's real state is UNKNOWN,
// so it must never be mistaken for "the session is gone" (which would let a
// caller tear down a session that is merely slow).
var ErrTmuxTimeout = errors.New("tmux command timed out")

// boundedTmuxCommand builds a tmux command bound to ctx. It runs in its own
// process group so the deadline tears down the whole tree — tmux plus any child
// that inherited the capture pipe — rather than SIGKILLing only the tmux client
// and orphaning a child that still holds the fd (exec.CommandContext's default
// Cancel kills just the direct process; #610/#769/#856 precedent).
func boundedTmuxCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid targets the whole group led by cmd.Process.Pid. A group
		// already gone (ESRCH) maps to os.ErrProcessDone, which Wait ignores
		// rather than reporting as a command failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = tmuxWaitDelay
	return cmd
}

// reapTmuxGroup SIGKILLs any process that outlived the tmux client on every exit
// path — normal completion or timeout — so a wedged server never leaves a
// pipe-holding child behind. The group is led by tmux, which has already exited,
// so this is ESRCH (ignored) in the common case. A nil Process means the command
// never started (a mock executor in tests, or a failed fork), so there is nothing
// to reap.
func reapTmuxGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// runTmuxBoundedWith runs a tmux command under ctx against an explicit executor,
// discarding output. The receiver-less teardown helpers (sessionExists,
// SessionProcessTrees) are handed a cmd.Executor rather than a TmuxSession, so
// they cannot use the methods below; this is the same body they share.
func runTmuxBoundedWith(ctx context.Context, cmdExec cmd.Executor, args ...string) error {
	c := boundedTmuxCommand(ctx, args...)
	err := cmdExec.Run(c)
	reapTmuxGroup(c)
	return normalizeWaitDelay(err)
}

// outputTmuxBoundedWith is outputTmuxBounded against an explicit executor.
func outputTmuxBoundedWith(ctx context.Context, cmdExec cmd.Executor, args ...string) ([]byte, error) {
	c := boundedTmuxCommand(ctx, args...)
	out, err := cmdExec.Output(c)
	reapTmuxGroup(c)
	return out, normalizeWaitDelay(err)
}

// runTmuxBounded runs a tmux command under tmuxCommandTimeout, discarding output.
func (t *TmuxSession) runTmuxBounded(ctx context.Context, args ...string) error {
	return runTmuxBoundedWith(ctx, t.cmdExec, args...)
}

// outputTmuxBounded runs a tmux command under tmuxCommandTimeout and returns stdout.
func (t *TmuxSession) outputTmuxBounded(ctx context.Context, args ...string) ([]byte, error) {
	return outputTmuxBoundedWith(ctx, t.cmdExec, args...)
}

// runTmuxBoundedStdin runs a tmux command under ctx with stdin wired to r. It
// exists because the helpers above deliberately take no stdin, and `load-buffer`
// is this package's only tmux command that STREAMS a payload in (#2099).
//
// It does NOT normalize exec.ErrWaitDelay to success, and that divergence from
// runTmuxBounded is the whole reason it is a separate helper. exec copies a
// non-*os.File stdin to the child through an OS pipe on a background goroutine,
// and WaitDelay force-closes that pipe when it elapses — which is exactly what
// keeps a killed tmux from stranding the copier, but it also means an unknown
// PREFIX of the payload may be all that landed. For the output-discarding control
// commands, normalizing is right: tmux exited cleanly and only a pipe-holding
// child lingered, so the work is done (#676/#914). For load-buffer it would claim
// a prompt was loaded when it was truncated, and the paste that follows would
// submit the mangled remainder while still reporting success — the silent
// prompt-corruption class of #1982, which is strictly worse than the hang this
// bound removes. So the error propagates and the submit fails loudly.
func runTmuxBoundedStdin(ctx context.Context, cmdExec cmd.Executor, r io.Reader, args ...string) error {
	c := boundedTmuxCommand(ctx, args...)
	c.Stdin = r
	err := cmdExec.Run(c)
	reapTmuxGroup(c)
	return err
}

// normalizeWaitDelay converts an exec.ErrWaitDelay into success: tmux itself
// exited cleanly (a non-zero exit surfaces as an ExitError, not ErrWaitDelay)
// and only an inherited child held the capture pipe open past tmuxWaitDelay —
// it was just killed by the reap. The command's work is done, so this is not a
// failure (#676/#914 precedent).
func normalizeWaitDelay(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// tmuxTimeoutContext returns the deadline every bounded tmux command runs under.
//
// Callers MUST check ctx.Err() before falling back to an ExistsOrUnknown probe
// on the error path: the probe spawns ANOTHER tmux command against the same
// wedged server, so it would hang exactly like the command that just timed out
// and defeat the bound we came here for. On a tripped deadline the session's
// real state is unknown, so callers return ErrTmuxTimeout rather than guessing
// ErrSessionGone (the same reasoning CapturePaneContentContext applies on
// cancellation, where it skips the probe and returns ctx.Err() directly).
func tmuxTimeoutContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), tmuxCommandTimeout)
}

// tmuxTimeoutContextWithin is tmuxTimeoutContext for a caller that already has a
// SHORTER deadline of its own: the bound becomes the smaller of budget and
// tmuxCommandTimeout.
//
// It exists for poll loops (#2099). A loop whose whole budget is
// pasteDeliveryMaxWait (2s) cannot bound the tmux command inside it at the flat
// tmuxCommandTimeout (10s): one stalled command would then overshoot the loop's
// entire budget 5x, and the loop's own deadline check — the code that decides to
// give up — stays unreachable for the whole 10s. Bounding each command by what is
// LEFT of the loop's budget is what makes that check actually run on time.
//
// A non-positive budget yields an already-expired context rather than an
// unbounded one: the caller is past its deadline, so the correct outcome is for
// the command to fail immediately and let the loop exit.
func tmuxTimeoutContextWithin(budget time.Duration) (context.Context, context.CancelFunc) {
	if budget > tmuxCommandTimeout {
		budget = tmuxCommandTimeout
	}
	return context.WithTimeout(context.Background(), budget)
}
