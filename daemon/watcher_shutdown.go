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

// daemonStopArrives waits up to grace for this watcher's own stop or the
// daemon's shutdown and reports whether either came.
func (w *taskWatcher) daemonStopArrives(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-w.stopCh:
		return true
	case <-w.sup.daemonStopping():
		return true
	case <-timer.C:
		return false
	}
}
