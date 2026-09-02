package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recoverFenceProbe is a backend that reports what the instance looked like at
// the moment recovery reached it, plus a one-shot hook that fires INSIDE the
// validation's critical section.
//
// Capabilities is the seam, and it is not an accident of the fake: the real
// lifecycleViewLocked resolves Recoverable through capabilitiesLocked while
// holding i.mu, precisely so the capability and the liveness axes come from one
// snapshot. That makes it the one place a test can act while the validating
// chokepoint holds — or has just released — the lock it validates under.
type recoverFenceProbe struct {
	*FakeBackend

	// duringValidate runs once, from inside the validation's lifecycle snapshot.
	duringValidate func()
	once           sync.Once

	livenessDuring Liveness
	opDuring       InFlightOp
	recoverCalls   int
}

func newRecoverFenceProbe() *recoverFenceProbe {
	return &recoverFenceProbe{FakeBackend: NewFakeBackend()}
}

func (b *recoverFenceProbe) Capabilities() Capabilities {
	if b.duringValidate != nil {
		b.once.Do(b.duringValidate)
	}
	return b.FakeBackend.Capabilities()
}

func (b *recoverFenceProbe) Recover(i *Instance) error {
	b.recoverCalls++
	b.livenessDuring = i.GetLiveness()
	b.opDuring = i.GetInFlightOp()
	// What a real backend does on success, and what clears the fence for free.
	_ = i.Transition(ConfirmLive())
	return nil
}

func lostInstance(t *testing.T, backend Backend) *Instance {
	t.Helper()
	i := &Instance{liveness: LiveReady, Title: "fenced-recover", started: true, backend: backend}
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))
	require.NoError(t, i.ValidateRuntimeAction(RuntimeActionRecoverLost))
	return i
}

// arriveDuringValidation returns a hook that delivers obs from another goroutine
// and then waits long enough for that goroutine to be parked on i.mu.
//
// The wait is what aims the observation at the gap; it is NOT the verdict. Too
// short and the broken code simply wins the race and the test reports a false
// pass — it can never make the FIXED code fail, because the fix holds the write
// lock continuously from the validation to the raise, so a late observer is
// still behind the fence when it lands.
func arriveDuringValidation(i *Instance, obs TransitionEvent) (hook func(), join func()) {
	entered := make(chan struct{})
	done := make(chan struct{})
	return func() {
			go func() {
				defer close(done)
				close(entered)
				_ = i.Transition(obs)
			}()
			<-entered
			time.Sleep(50 * time.Millisecond)
		}, func() {
			<-done
		}
}

// #3555. The race the fix closes, driven through the real interleaving rather
// than asserted about.
//
// The daemon's poll reads (op, epoch) and, seeing no op in flight, applies its
// observation. When that observation lands in the window between the recover
// precondition check and the MarkRestoring raise it is CURRENT, not stale, so
// the epoch guard cannot drop it — and tkMarkRestoring is keyed on the op axis
// alone, so the raise then happily fences a liveness that was just clobbered.
// Recovery proceeds against a session that is no longer lost; on a remote
// backend that means replacing a sandbox which had just become reachable again,
// stranding its unpushed work.
//
// Holding i.mu across both halves is what makes that unreachable: the poll's
// observation is either applied BEFORE the critical section, where validation
// sees it and refuses, or blocked until AFTER the raise, where the epoch guard
// drops it as superseded. There is no third arrival.
func TestRecoverFencedRefusesAnObservationThatLandsBetweenValidationAndTheRaise(t *testing.T) {
	probe := newRecoverFenceProbe()
	i := lostInstance(t, probe)

	// The poll's decision point: it read no op in flight, at this epoch.
	op, epoch := i.InFlightOpAndEpoch()
	require.Equal(t, OpNone, op, "the poll only observes an unfenced session")

	hook, join := arriveDuringValidation(i, ObserveLiveness(LiveRunning).AtEpoch(epoch))
	probe.duringValidate = hook

	err := i.RecoverFencedWithLiveBoundary(nil)
	join()

	require.NoError(t, err)
	require.Equal(t, 1, probe.recoverCalls, "the recovery ran")
	require.Equal(t, LiveLost, probe.livenessDuring,
		"recovery must never run against a liveness an observation clobbered inside the fence's own raise")
	require.Equal(t, OpRestoring, probe.opDuring,
		"and the fence has to be up for the whole backend call, so the poll skips it")
}

// The other arrival, and the reason the check has to live inside the fence's own
// critical section rather than ahead of it: an observation that wins the lock
// outright must be REFUSED, not fenced over. tkMarkRestoring cannot do this on
// its own (next test), so validation and the raise have to share the lock.
func TestRecoverFencedRefusesAnObservationThatWinsTheLock(t *testing.T) {
	probe := newRecoverFenceProbe()
	i := lostInstance(t, probe)

	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)), "the poll's observation lands first")

	require.Error(t, i.RecoverFencedWithLiveBoundary(nil),
		"a session the poll just saw running is not lost, so it is not recoverable")
	require.Zero(t, probe.recoverCalls, "and the backend is never reached")
	require.Equal(t, OpNone, i.GetInFlightOp(), "no fence was raised on the way out")
}

// Why the raise cannot be trusted to notice on its own: tkMarkRestoring is keyed
// on the op axis alone and deliberately leaves liveness untouched, so it will
// fence anything that is not already busy.
func TestTheBareMarkRestoringEdgeDoesNotCheckLiveness(t *testing.T) {
	i := lostInstance(t, newRecoverFenceProbe())
	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))

	require.NoError(t, i.Transition(MarkRestoring()), "the bare edge checks only the op axis")
	require.Equal(t, LiveRunning, i.GetLiveness(), "so it fences a session with nothing to recover")
	require.Equal(t, OpRestoring, i.GetInFlightOp())
}

// The automatic Lost-restore loop reaches recovery through the same entry point,
// so the same interleaving must come out the same way. It had its own copy of
// the unlocked precondition check (#3555 scope item 2): its caller in
// daemon/lostrestore.go raises no lifecycle fence of its own, so nothing else
// closed this window for it.
func TestRecoverRefusesAnObservationThatLandsBetweenValidationAndTheRaise(t *testing.T) {
	probe := newRecoverFenceProbe()
	i := lostInstance(t, probe)

	op, epoch := i.InFlightOpAndEpoch()
	require.Equal(t, OpNone, op)

	hook, join := arriveDuringValidation(i, ObserveLiveness(LiveRunning).AtEpoch(epoch))
	probe.duringValidate = hook

	err := i.Recover()
	join()

	require.NoError(t, err)
	require.Equal(t, LiveLost, probe.livenessDuring,
		"the automatic loop must not recover a session an observation just declared running")
	require.Equal(t, OpRestoring, probe.opDuring,
		"and it fences the backend call exactly as the manual path does")
}
