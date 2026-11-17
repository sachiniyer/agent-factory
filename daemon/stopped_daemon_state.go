package daemon

import "sync/atomic"

// The process-wide state a daemon leaves for in-process code that has no
// handle on it. Task deliveries find its tracker (taskDeliveries) and its
// drain predicate (daemonDrainingFn), and the control client asks it whether
// it may ensure a daemon (#7440, #7474).

// inProcessDaemon is the lifecycle of the daemon running in this process, or
// nil in a process that runs none, such as the CLI.
var inProcessDaemon atomic.Pointer[daemonLifecycle]

// inProcessDaemonStopped latches once this process's daemon has returned.
var inProcessDaemonStopped atomic.Bool

// daemonReturnRestoresProcessDefaults makes a returning daemon restore the
// inert process defaults instead of latching its stopped state. Production
// never sets it: the process exits once its daemon returns. Only a test binary
// that runs daemon after daemon in one process sets it, in TestMain.
var daemonReturnRestoresProcessDefaults = false

// inProcessDaemonEnsureRefusal is the stopping refusal when this process's
// daemon is stopping or restarting, or has stopped, and nil otherwise: always
// nil in a process that runs no daemon. A delivery in such a daemon must never
// ensure a daemon (#7474). Its control socket closing is the stop, not a
// daemon to repair, and an ensure from inside it could spawn a successor
// behind the stopper's back and wait for it. Its dead socket is a shutdown
// abort instead, which keeps a fire's marker for the next daemon. An upgrade
// hand-off is unchanged: there the live upgrade gate already answers an
// ensure, and nothing spawns.
func inProcessDaemonEnsureRefusal() error {
	if inProcessDaemonStopped.Load() {
		return errDaemonStopping()
	}
	if l := inProcessDaemon.Load(); l != nil && l.stoppingWithoutHandoff() {
		return errDaemonStopping()
	}
	return nil
}

// installInProcessDaemon publishes a starting daemon's lifecycle and delivery
// tracker, and returns what its return must run.
func installInProcessDaemon(lifecycle *daemonLifecycle, deliveries *joinedRuns) (onReturn func()) {
	setDaemonDraining(lifecycle.isQuiescing)
	taskDeliveries.Store(deliveries)
	inProcessDaemon.Store(lifecycle)
	inProcessDaemonStopped.Store(false)
	return func() {
		if daemonReturnRestoresProcessDefaults {
			setDaemonDraining(nil)
			taskDeliveries.CompareAndSwap(deliveries, nil)
			inProcessDaemon.CompareAndSwap(lifecycle, nil)
			return
		}
		latchStoppedDaemon(deliveries)
	}
}

// latchStoppedDaemon leaves a returned daemon's gates closed for the rest of
// the process (#7474). Restoring the defaults would admit a delivery untracked
// (a nil tracker admits every run) and read its transport error as a task
// failure that clears its marker, possibly after the home's lock is released.
// Only a goroutine starved through the whole teardown can still arrive, and
// it must be refused before it writes, keeping its marker for the next daemon.
func latchStoppedDaemon(deliveries *joinedRuns) {
	deliveries.close()
	setDaemonDraining(func() bool { return true })
	inProcessDaemonStopped.Store(true)
	inProcessDaemon.Store(nil)
}
