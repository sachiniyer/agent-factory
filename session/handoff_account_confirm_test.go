package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// confirmableManualSwap is the manual account swap a #4429 confirm acts on: the
// replacement panes started, the mission verdict is recorded, and the admitted
// launch plan is still held — exactly what a resend that could not confirm
// leaves behind.
func confirmableManualSwap(status PromptDeliveryStatus) *Instance {
	return &Instance{
		Title:    "swapped",
		Account:  "personal",
		started:  true,
		liveness: LiveRunning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			Mission: "continue", MissionDeliveryStatus: status,
		},
		accountSwapLaunch: &accountSwapLaunchPlan{account: "personal", base: "claude", program: "claude"},
	}
}

// Confirming a manual account swap retires the whole transaction. The normal
// completion clears the pending swap AND its launch plan, and
// tabSpawnBlockedLocked refuses on either, so a confirm that left the plan
// behind kept every new tab refused as "account swap in progress" until the
// daemon restarted.
func TestConfirmPendingManualAccountSwapDeliveryReleasesTabSpawn(t *testing.T) {
	for _, status := range []PromptDeliveryStatus{PromptSentUnverified, PromptCouldNotConfirm, PromptDelivered} {
		t.Run(string(status), func(t *testing.T) {
			inst := confirmableManualSwap(status)
			require.Error(t, inst.TabSpawnBlocked(), "fixture: the pending swap blocks tab creation")
			require.True(t, inst.CanConfirmPendingManualAccountSwapDelivery())

			require.NoError(t, inst.ConfirmPendingManualAccountSwapDelivery("work", "personal"))

			swapPending, _ := inst.PendingManualAccountSwap()
			require.False(t, swapPending, "the confirm retires the pending swap")
			require.Nil(t, inst.accountSwapLaunch, "the confirm retires the admitted launch plan with it")
			require.NoError(t, inst.TabSpawnBlocked(),
				"a confirmed swap must not keep refusing new tabs")
			require.Equal(t, LiveRunning, inst.GetLiveness(), "confirm does not move liveness")
		})
	}
}

// The startup-unknown and orphaned-fence shapes are the ones this verb exists
// for: the daemon has probed the pane, so the confirm lifts both and keeps the
// row's liveness.
func TestConfirmPendingManualAccountSwapDeliveryResolvesTheWedge(t *testing.T) {
	inst := confirmableManualSwap(PromptSentUnverified)
	inst.startupStateUnknown = true
	inst.started = false
	inst.inFlightOp = OpRespawning
	inst.liveness = LiveLimitReached
	require.True(t, inst.CanConfirmPendingManualAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingManualAccountSwapDelivery("work", "personal"))

	require.False(t, inst.StartupStateUnknown(), "the probe-backed confirm resolves the unknown flag")
	require.True(t, inst.Started(), "and restores the started bit the flag lowered")
	require.Equal(t, OpNone, inst.GetInFlightOp(), "the orphaned respawn fence is dropped")
	require.Equal(t, LiveLimitReached, inst.GetLiveness(), "dropping the fence keeps liveness")
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

func TestCanConfirmPendingManualAccountSwapDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
		want   bool
	}{
		{name: "sent-unverified on a running row", mutate: func(*Instance) {}, want: true},
		{name: "could-not-confirm", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptCouldNotConfirm }, want: true},
		{name: "delivered crash window", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptDelivered }, want: true},
		{name: "ready", mutate: func(i *Instance) { i.liveness = LiveReady }, want: true},
		{name: "limit reached", mutate: func(i *Instance) { i.liveness = LiveLimitReached }, want: true},
		{name: "startup unknown", mutate: func(i *Instance) { i.startupStateUnknown = true; i.liveness = LivenessUnset }, want: true},
		{name: "orphaned respawn fence", mutate: func(i *Instance) { i.inFlightOp = OpRespawning }, want: true},

		{name: "not delivered belongs to automatic recovery", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "no verdict recorded", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = "" }},
		{name: "automatic swap", mutate: func(i *Instance) { i.pendingAccountSwap.Manual = false }},
		{name: "replacement panes never started", mutate: func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{name: "no pending swap", mutate: func(i *Instance) { i.pendingAccountSwap = nil }},
		{name: "kill tombstone", mutate: func(i *Instance) { i.userKilled = true }},
		{name: "lost", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "lost and startup unknown", mutate: func(i *Instance) { i.liveness = LiveLost; i.startupStateUnknown = true }},
		{name: "dead", mutate: func(i *Instance) { i.liveness = LiveDead }},
		{name: "archived", mutate: func(i *Instance) { i.liveness = LiveArchived }},
		{name: "no proven liveness", mutate: func(i *Instance) { i.liveness = LivenessUnset }},
		{name: "another op in flight", mutate: func(i *Instance) { i.inFlightOp = OpKilling }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptSentUnverified)
			tc.mutate(inst)
			require.Equal(t, tc.want, inst.CanConfirmPendingManualAccountSwapDelivery())
		})
	}
}

// Every refusal leaves the transaction exactly as it was: a confirm that
// fails must not half-retire the swap or its launch plan.
func TestConfirmPendingManualAccountSwapDeliveryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		mutate   func(*Instance)
	}{
		{name: "different source account", from: "other", to: "personal", mutate: func(*Instance) {}},
		{name: "different target account", from: "work", to: "other", mutate: func(*Instance) {}},
		{name: "automatic swap", from: "work", to: "personal", mutate: func(i *Instance) { i.pendingAccountSwap.Manual = false }},
		{name: "no replacement panes", from: "work", to: "personal", mutate: func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{name: "not delivered", from: "work", to: "personal", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "kill tombstone", from: "work", to: "personal", mutate: func(i *Instance) { i.userKilled = true }},
		{name: "lost", from: "work", to: "personal", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "busy", from: "work", to: "personal", mutate: func(i *Instance) { i.inFlightOp = OpArchiving }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptSentUnverified)
			tc.mutate(inst)
			pendingBefore := *inst.pendingAccountSwap
			opBefore := inst.inFlightOp

			require.Error(t, inst.ConfirmPendingManualAccountSwapDelivery(tc.from, tc.to))

			require.NotNil(t, inst.pendingAccountSwap, "a refused confirm keeps the pending swap")
			require.Equal(t, pendingBefore, *inst.pendingAccountSwap)
			require.NotNil(t, inst.accountSwapLaunch, "a refused confirm keeps the launch plan")
			require.Equal(t, opBefore, inst.inFlightOp)
		})
	}
	t.Run("no pending swap", func(t *testing.T) {
		inst := confirmableManualSwap(PromptSentUnverified)
		inst.pendingAccountSwap = nil
		require.Error(t, inst.ConfirmPendingManualAccountSwapDelivery("work", "personal"))
	})
}
