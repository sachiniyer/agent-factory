package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// runtimeActionAdmissionViews covers every refusal arm of refuseRuntimeAction
// plus the admitted shapes, so the verdict-only half can be proven identical
// to the error form across the whole admission tree — not just sampled.
func runtimeActionAdmissionViews() map[string]LifecycleView {
	return map[string]LifecycleView{
		"running-idle":     {Title: "s", Liveness: LiveRunning, Started: true},
		"ready-idle":       {Title: "s", Liveness: LiveReady, Started: true},
		"unset":            {Title: "s", Liveness: LivenessUnset},
		"killed":           {Title: "s", Liveness: LiveRunning, Started: true, UserKilled: true},
		"startup-unknown":  {Title: "s", Liveness: LiveRunning, Started: true, StartupStateUnknown: true},
		"pending-swap":     {Title: "s", Liveness: LiveRunning, Started: true, PendingAccountSwap: true, PendingAccountSwapFrom: "acct-a", PendingAccountSwapTo: "acct-b"},
		"busy":             {Title: "s", Liveness: LiveRunning, Started: true, InFlightOp: OpArchiving, Status: Running},
		"restoring-fenced": {Title: "s", Liveness: LiveRunning, Started: true, InFlightOp: OpRestoring, Status: Running},
		"archived":         {Title: "s", Liveness: LiveArchived},
		"archived-fenced":  {Title: "s", Liveness: LiveArchived, InFlightOp: OpRestoring},
		"lost":             {Title: "s", Liveness: LiveLost, Started: true},
		"lost-unstarted":   {Title: "s", Liveness: LiveLost},
		"lost-fenced":      {Title: "s", Liveness: LiveLost, Started: true, InFlightOp: OpRestoring},
		"dead":             {Title: "s", Liveness: LiveDead, Started: true},
		"limit":            {Title: "s", Liveness: LiveLimitReached, Started: true},
		"limit-unstarted":  {Title: "s", Liveness: LiveLimitReached},
		"reserved-root":    {Title: RootSessionTitle, Liveness: LiveRunning, Started: true},
		"mission-pending":  {Title: "s", Liveness: LiveRunning, Started: true, PendingHandoffMission: true},
		"teardown-pending": {Title: "s", Liveness: LiveRunning, Started: true, RebuildTeardownPending: true},
	}
}

func runtimeActionUnderTest() []RuntimeAction {
	return []RuntimeAction{
		RuntimeActionRestoreArchived,
		RuntimeActionRestoreArchivedFenced,
		RuntimeActionRestoreLostOrDead,
		RuntimeActionRecoverLost,
		RuntimeActionRecoverFenced,
		RuntimeActionResumeLimit,
		RuntimeActionHandoff,
		RuntimeAction(99), // the unknown-action arm
	}
}

// TestAdmitsRuntimeActionMatchesValidate pins the contract the predicate form
// exists under: across every refusal arm and every action, the boolean half
// must answer exactly what the error form's nil-ness answers. A drift here
// would split the admission question in two (#6845).
func TestAdmitsRuntimeActionMatchesValidate(t *testing.T) {
	for name, view := range runtimeActionAdmissionViews() {
		for _, action := range runtimeActionUnderTest() {
			admit := view.AdmitsRuntimeAction(action)
			err := view.ValidateRuntimeAction(action)
			require.Equal(t, err == nil, admit,
				"%s action %d: AdmitsRuntimeAction=%v but ValidateRuntimeAction err=%v",
				name, action, admit, err)
		}
	}
}

// TestAdmitsRuntimeActionDoesNotAllocate pins the property the fix exists for:
// the per-tick predicate path — the lost-restore gate, the snapshot's
// CanHandoff bit — must never heap-allocate the refusal text it discards
// (#6845).
func TestAdmitsRuntimeActionDoesNotAllocate(t *testing.T) {
	for name, view := range runtimeActionAdmissionViews() {
		for _, action := range runtimeActionUnderTest() {
			allocs := testing.AllocsPerRun(64, func() {
				_ = view.AdmitsRuntimeAction(action)
			})
			require.Zero(t, allocs, "%s action %d must not allocate", name, action)
		}
	}
}

// TestRuntimeActionRefusalText pins representative refusal messages through
// the lazy error form, so splitting the verdict from the prose cannot change
// the text a user (or an existing test asserting on it) reads.
func TestRuntimeActionRefusalText(t *testing.T) {
	busy := LifecycleView{Title: "s", Liveness: LiveLost, Started: true, InFlightOp: OpArchiving, Status: Lost}
	require.EqualError(t, busy.ValidateRuntimeAction(RuntimeActionRestoreLostOrDead),
		`session "s" is busy (5); try again in a moment`)

	swap := LifecycleView{Title: "s", Liveness: LiveRunning, Started: true,
		PendingAccountSwap: true, PendingAccountSwapFrom: "acct-a", PendingAccountSwapTo: "acct-b"}
	err := swap.ValidateRuntimeAction(RuntimeActionRestoreArchived)
	require.Error(t, err)
	require.Contains(t, err.Error(), "committed account swap")

	root := LifecycleView{Title: RootSessionTitle, Liveness: LiveRunning, Started: true}
	require.EqualError(t, root.ValidateRuntimeAction(RuntimeActionHandoff),
		`session "root" is the daemon-managed root agent and cannot be handed off`)

	running := LifecycleView{Title: "s", Liveness: LiveRunning, Started: true}
	require.EqualError(t, running.ValidateRuntimeAction(RuntimeActionRecoverLost),
		`session "s" is not lost`)
	require.EqualError(t, running.ValidateRuntimeAction(RuntimeAction(99)),
		"unknown runtime action 99")
}

// TestInstanceAdmitsRuntimeAction exercises the locking form on a live row.
func TestInstanceAdmitsRuntimeAction(t *testing.T) {
	inst := probeLocalInstance(t)
	require.False(t, inst.AdmitsRuntimeAction(RuntimeActionRecoverLost), "a Running session is not recovery-eligible")
	require.True(t, inst.AdmitsRuntimeAction(RuntimeActionHandoff), "a started Running local session can hand off")
}

func BenchmarkRuntimeActionAdmission(b *testing.B) {
	view := LifecycleView{Title: "bench", Liveness: LiveRunning, Started: true, Status: Running}
	b.Run("validate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = view.ValidateRuntimeAction(RuntimeActionRecoverLost)
		}
	})
	b.Run("admit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = view.AdmitsRuntimeAction(RuntimeActionRecoverLost)
		}
	})
}
