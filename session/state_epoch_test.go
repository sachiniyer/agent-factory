package session

import (
	"testing"
	"time"
)

// newEpochTestInstance builds a bare instance for the epoch unit tests. It needs
// no backend or worktree: every property under test is pure lifecycle state.
func newEpochTestInstance(t *testing.T) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "epoch", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	return inst
}

// TestStateEpoch_AdvancesOnlyOnRealChange pins the counter's contract (#2135):
// it moves when the lifecycle state actually changes and stays put otherwise. A
// counter that ticked on every write would make the poll's ordinary
// re-observation of an unchanged session invalidate another observer's in-flight
// decision, turning a correctness guard into a source of dropped updates.
func TestStateEpoch_AdvancesOnlyOnRealChange(t *testing.T) {
	inst := newEpochTestInstance(t)
	_ = inst.Transition(ObserveLiveness(LiveReady))

	start := inst.StateEpoch()
	if err := inst.Transition(ObserveLiveness(LiveReady)); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if got := inst.StateEpoch(); got != start {
		t.Fatalf("epoch = %d after re-observing the same liveness, want %d (unchanged)", got, start)
	}

	if err := inst.Transition(ObserveLiveness(LiveRunning)); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if got := inst.StateEpoch(); got == start {
		t.Fatalf("epoch = %d after a real liveness change, want it advanced", got)
	}

	// The usage-limit reset time is tracked too: a later-parsed reset time on an
	// already-limit-blocked session is a real change (#1204's persist case).
	inst.SetLimitReached(time.Time{})
	parked := inst.StateEpoch()
	inst.SetLimitReached(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))
	if got := inst.StateEpoch(); got == parked {
		t.Fatalf("epoch = %d after the reset time changed, want it advanced", got)
	}
}

// TestSetLimitReachedAtEpoch_DropsSupersededDecision is the session-level half of
// the #2135 fix: a limit decision made from an observation is applied only while
// the state it was made about is still current. A resume clearing the block moves
// the epoch, so the stale decision behind it is dropped rather than re-parking a
// session that is working again.
func TestSetLimitReachedAtEpoch_DropsSupersededDecision(t *testing.T) {
	inst := newEpochTestInstance(t)
	resetAt := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	inst.SetLimitReached(resetAt)

	// An observer captures the epoch, then a resume lands.
	observed := inst.StateEpoch()
	inst.ClearLimitReached()

	if applied := inst.SetLimitReachedAtEpoch(resetAt, observed); applied {
		t.Fatal("a decision from a superseded epoch must not apply (#2135)")
	}
	if inst.LimitReached() {
		t.Fatalf("liveness = %v, want LiveRunning: the resume must stand", inst.GetLiveness())
	}
	if got, ok := inst.LimitResetAt(); ok || !got.IsZero() {
		t.Fatalf("reset time = (%v, %v), want (zero, false)", got, ok)
	}

	// A decision made about the CURRENT state still applies — the guard is
	// per-observation, not a window in which detection is suppressed.
	if applied := inst.SetLimitReachedAtEpoch(resetAt, inst.StateEpoch()); !applied {
		t.Fatal("a decision at the current epoch must apply")
	}
	if !inst.LimitReached() {
		t.Fatalf("liveness = %v, want LiveLimitReached", inst.GetLiveness())
	}
}

// TestTransitionAtEpoch_DropsSupersededObservation: the same guard on the
// liveness chokepoint. An unscoped event is unaffected — AtEpoch is opt-in, for
// writers whose conclusion was drawn from an earlier observation.
func TestTransitionAtEpoch_DropsSupersededObservation(t *testing.T) {
	inst := newEpochTestInstance(t)
	_ = inst.Transition(ObserveLiveness(LiveRunning))

	observed := inst.StateEpoch()
	inst.SetLimitReached(time.Time{}) // something newer lands

	if err := inst.Transition(ObserveLiveness(LiveReady).AtEpoch(observed)); err != nil {
		t.Fatalf("a superseded event must be dropped silently, got error %v", err)
	}
	if got := inst.GetLiveness(); got != LiveLimitReached {
		t.Fatalf("liveness = %v, want LiveLimitReached (the stale Ready must not land)", got)
	}

	if err := inst.Transition(ObserveLiveness(LiveReady)); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if got := inst.GetLiveness(); got != LiveReady {
		t.Fatalf("liveness = %v, want LiveReady (an unscoped event still applies)", got)
	}
}

// TestToInstanceDataWithEpoch_ReadsBothUnderOneLock: the lifecycle portion of
// the payload and its lifecycle epoch must describe the same instant. The method
// is not a whole-projection freshness guard; tabs and other projection fields do
// not advance this epoch (#2135).
func TestToInstanceDataWithEpoch_ReadsBothUnderOneLock(t *testing.T) {
	inst := newEpochTestInstance(t)
	resetAt := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	inst.SetLimitReached(resetAt)

	data, epoch := inst.ToInstanceDataWithEpoch()
	if epoch != inst.StateEpoch() {
		t.Fatalf("epoch = %d, want the instance's current %d", epoch, inst.StateEpoch())
	}
	if data.Liveness != LiveLimitReached || !data.LimitResetAt.Equal(resetAt) {
		t.Fatalf("data = (%v, %v), want (LiveLimitReached, %v)", data.Liveness, data.LimitResetAt, resetAt)
	}

	inst.ClearLimitReached()
	if epoch == inst.StateEpoch() {
		t.Fatal("the epoch read earlier must not track later mutations")
	}
}
