package daemon

import (
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Run health: how the supervisor classifies an exit (signal vs failure),
// paces restarts, and what a run that proves healthy earns back — a reset
// backoff chain and, when the only thing on record is a stale terminal
// status, "started" again (#5441). Extracted from watcher.go to keep it
// under the file-length ceiling (#1145); the task-store half lives in
// task/watch_recovery.go.
const (
	// watchRecoveredStatus is the last_run_status a watch earns back once a
	// run has stayed alive a full crash window with only a stale terminal
	// status on record (#5441). It reuses the existing "started" word: the
	// current run is live, and nothing stronger (a delivery, a stop) is
	// claimed.
	watchRecoveredStatus = "started"
)

func (w *taskWatcher) setRunAlive(alive bool) {
	w.mu.Lock()
	w.runAlive = alive
	w.mu.Unlock()
}

// recoverStaleTerminalStatus retires a stale terminal last_run_status once a
// run has stayed alive a full crash window (#5441). The window is the same
// budget the crash-loop breaker counts against, so a still-crash-looping
// watch never reaches this point and nothing clears while a failure is real.
// statusMu serializes the write against a terminal publication racing in on
// the same task — giveUpSummary's persist takes the same mutex — and the
// runAlive check under mu keeps a callback that outlived its own run from
// writing over a fresher terminal outcome. The store-side write is itself
// conditional: only a persisted "stopped"/"errored:*" becomes
// watchRecoveredStatus; a delivered, dropped, or parked outcome committed in
// the window is never clobbered.
func (w *taskWatcher) recoverStaleTerminalStatus() {
	if w.stopRequested() {
		return
	}
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	w.mu.Lock()
	alive := w.runAlive
	w.mu.Unlock()
	if !alive {
		return
	}
	recovered, err := w.sup.recoverStatus(w.taskID, watchRecoveredStatus)
	if err != nil {
		log.WarningLog.Printf("watch task %s: could not retire a stale terminal status: %v", w.taskID, err)
		return
	}
	if recovered {
		log.InfoLog.Printf("watch task %s: run stayed healthy for one %s crash window; last_run_status recovered to %q", w.taskID, w.sup.crashWindow, watchRecoveredStatus)
	}
}

// nextBackoff computes the delay before the next restart and the backoff to
// carry into the following cycle. A run that stayed healthy for a whole crash
// window resets the chain: it waits baseBackoff and leaves the carried backoff
// at baseBackoff, so the next failure restarts the documented 1s→2s→4s…
// sequence from the base rather than from 2*baseBackoff (#1005). An unhealthy
// run waits the current backoff and doubles it toward maxBackoff for next time.
func nextBackoff(current, base, max time.Duration, healthy bool) (wait, next time.Duration) {
	if healthy {
		return base, base
	}
	next = current * 2
	if next > max {
		next = max
	}
	return current, next
}

// exitedFromSignal reports whether err is an exec exit caused by the given
// signal (as opposed to a non-zero exit code or a start failure), whether the
// shell was started directly or its lifeline reported it.
func exitedFromSignal(err error, sig syscall.Signal) bool {
	var shellExit *watchShellExit
	if errors.As(err, &shellExit) {
		return shellExit.sig == sig
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}
