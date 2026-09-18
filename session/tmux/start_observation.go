package tmux

import "sync/atomic"

// StartObservation names a point at which Start is about to observe whether
// the pane it launched is still alive. A pane program can exit at any moment,
// and Start's report depends only on which side of these points the exit
// lands on:
//
//   - before StartBeforeExistencePoll resolves, the session never appears to
//     the poll and Start times out;
//   - between that and StartBeforeAttachProbe, the attach probe finds it gone
//     and Start reports a pane that vanished before attach;
//   - after the attach probe, Start succeeds and the pane's exit is the
//     caller's to observe.
//
// Tests that need a specific one of those outcomes drive it through
// SetStartObservationHookForTest instead of racing a real exit against the
// probes (#4406).
type StartObservation int

const (
	// StartBeforeExistencePoll is after the new-session command was launched
	// and before the first existence probe.
	StartBeforeExistencePoll StartObservation = iota + 1
	// StartBeforeAttachProbe is after the existence poll saw the session and
	// before Restore's liveness probe.
	StartBeforeAttachProbe
)

type startObservationHook func(sanitizedName string, point StartObservation)

var startObservationHookValue atomic.Pointer[startObservationHook]

// SetStartObservationHookForTest runs hook at each StartObservation of every
// Start in this process, until the returned restore is called. The hook runs on
// Start's own goroutine, so it can deterministically end the pane before the
// observation that follows it.
func SetStartObservationHookForTest(hook func(sanitizedName string, point StartObservation)) (restore func()) {
	typed := startObservationHook(hook)
	previous := startObservationHookValue.Swap(&typed)
	return func() { startObservationHookValue.Store(previous) }
}

func (t *TmuxSession) observeStart(point StartObservation) {
	if hook := startObservationHookValue.Load(); hook != nil && *hook != nil {
		(*hook)(t.sanitizedName, point)
	}
}
