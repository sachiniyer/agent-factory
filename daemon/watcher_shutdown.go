package daemon

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// The daemon's own stop is not a watch task's failure (#7392).
//
// A watch command dies in two ways that belong to the daemon's lifecycle, not
// to the script:
//
//   - systemd stops it. Every watch spawn runs in a transient scope bound to
//     the daemon's unit (BindsTo+After, systemdunit.NewBoundChildCommand), so
//     stopping the unit stops the scope in the same transaction, and the
//     scope's SIGTERM reaches the watch shell just before the daemon's own.
//   - systemd refuses to start it. While the daemon's unit has a stop job
//     installed, systemd-run cannot start a scope bound to that unit
//     ("Transaction … is destructive (agent-factory-daemon.service has 'stop'
//     job queued …)"), so a restart in that window exits 1 having run nothing.
//
// Both used to count toward the crash-loop breaker. On 11-15 a daemon whose
// drain hung for 90s kept its watch loops running and restarting the whole time
// — watchers.Stop is the last thing a shutdown does — and burned both of the
// box's watch tasks' budgets inside its own stop.
//
// So the supervisor carries a shutdown latch, set as the first step of a daemon
// stop (drainDaemon), before anything that can block. From that point no watch
// command is restarted and no exit is counted. The two lifecycle-shaped exits
// above, which can land just BEFORE the latch, are held for a grace to see it
// arrive; only when the daemon's stop does not follow are they counted, exactly
// as before.

// scopeRefusalPrefix is how systemd-run reports a transient scope the manager
// would not start; scopeStopQueued is systemd's TransactionIsDestructive text
// naming the daemon's own unit and its stop job. systemd's bus errors are not
// translated, and "queued" covers a job that is already running.
const scopeRefusalPrefix = "Failed to start transient scope unit: "

var scopeStopQueued = "is destructive (" + systemdunit.DaemonUnitName + " has 'stop' job queued"

// scopeRefusedForDaemonStop reports whether a run's output is systemd-run's
// refusal to start the run's scope because the daemon's unit is being stopped.
// A refused systemd-run runs nothing, so its message is the run's newest line.
// The text is only a hint: run counts the exit anyway unless the daemon's stop
// actually arrives within the grace, so a script that prints this cannot dodge
// the breaker.
func scopeRefusedForDaemonStop(tail *tailBuffer) bool {
	line := tail.lastLine()
	return strings.HasPrefix(line, scopeRefusalPrefix) && strings.Contains(line, scopeStopQueued)
}

// daemonStopping returns the channel beginShutdown closes. It is created on
// first use so a supervisor built as a bare literal still has one.
func (s *watcherSupervisor) daemonStopping() <-chan struct{} {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.shutdownCh == nil {
		s.shutdownCh = make(chan struct{})
	}
	return s.shutdownCh
}

// beginShutdown latches the daemon's stop: from here no watch command is
// restarted, no exit counts toward the crash-loop breaker, and reconcile and
// arming read the supervisor as stopping. Running watch commands keep running
// until Stop. Idempotent and safe on a nil supervisor, so every shutdown path
// can call it unconditionally.
func (s *watcherSupervisor) beginShutdown() {
	if s == nil {
		return
	}
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.shutdownCh == nil {
		s.shutdownCh = make(chan struct{})
	}
	if !s.shutdownBegun {
		s.shutdownBegun = true
		close(s.shutdownCh)
	}
}

// shuttingDown reports whether beginShutdown has run.
func (s *watcherSupervisor) shuttingDown() bool {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	return s.shutdownBegun
}

// stopArrival says which stop ended a lifecycle grace, if any.
type stopArrival int

const (
	noStopArrived stopArrival = iota
	// watcherStopArrived is this watcher's own stop: a reload, a disable, a
	// restart, or the supervisor's Stop.
	watcherStopArrived
	// daemonStopArrived is the daemon's shutdown latch.
	daemonStopArrived
)

// lifecycleGrace names the branch of run() that is holding an exit to see
// whether the daemon's stop caused it.
type lifecycleGrace string

const (
	graceSigterm      lifecycleGrace = "sigterm"
	graceScopeRefused lifecycleGrace = "scope-refused"
	graceBreaker      lifecycleGrace = "breaker"
	graceCleanExit    lifecycleGrace = "clean-exit"
)

// watchGraceEntered is a test seam, nil in production. It runs on the
// watcher's goroutine as an exit enters one of run()'s lifecycle graces, before
// the grace starts to wait, so a test can land a stop inside exactly the branch
// it means to pin (#7404). A test sets it before the watcher starts and
// restores it after the supervisor's Stop.
var watchGraceEntered func(taskID string, grace lifecycleGrace)

// boundScopeStopSlack covers what follows a bound scope's stop timeout before
// the daemon's own stop reaches the latch: systemd SIGKILLs and reaps the
// scope, starts the daemon unit's stop job, and the daemon's signal handler
// begins its drain (#7404).
const boundScopeStopSlack = 500 * time.Millisecond

// Pin (#7404). Every lifecycle grace waits watcherStopGrace for the daemon's
// stop. systemd holds that stop until every bound scope has stopped, which
// takes up to the bound-scope stop timeout plus the slack. If the grace were
// shorter, a watch that a TERM-ignoring sibling scope delayed would have its
// stop-caused exit counted, and its fifth could write a durable give-up. A
// negative constant cannot be converted to uint, so raising the scope timeout
// past the grace fails the build.
const _ = uint(watcherStopGrace - (systemdunit.BoundChildStopTimeoutDuration + boundScopeStopSlack))

// awaitDaemonStop holds an exit in the grace named by grace for up to
// w.sup.stopGrace, and returns the moment either stop fires. That way a
// watcher in a grace never makes Stop, or a stopping daemon, wait the grace
// out. It reports which stop arrived, or noStopArrived when the grace
// expired.
func (w *taskWatcher) awaitDaemonStop(grace lifecycleGrace) stopArrival {
	if hook := watchGraceEntered; hook != nil {
		hook(w.taskID, grace)
	}
	timer := time.NewTimer(w.sup.stopGrace)
	defer timer.Stop()
	select {
	case <-w.stopCh:
		return watcherStopArrived
	case <-w.sup.daemonStopping():
		return daemonStopArrived
	case <-timer.C:
		return noStopArrived
	}
}
