package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandoffAccountPreservesCustomProgram(t *testing.T) {
	inst := handoffTestInstance(t, "claude")
	inst.Program = "claude --model opus"
	inst.Account = "work"
	require.NoError(t, inst.BeginManualAccountSwap())
	entry, err := inst.SelectAccountForHandoff("work", "personal", "claude", HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	require.Equal(t, "claude --model opus", inst.AgentProgram())
	require.Equal(t, "claude --model opus", inst.ToInstanceData().Program)
	require.Equal(t, "claude", entry.From.Agent)
	require.Equal(t, "claude", entry.To)
	require.Equal(t, "work", entry.FromAccount)
	require.Equal(t, "personal", entry.ToAccount)
	require.Len(t, inst.Handoffs(), 1)
	require.NoError(t, inst.RevertHandoff(entry))
	require.Equal(t, "claude --model opus", inst.AgentProgram())
}

func TestPendingManualAccountSwapDeliveryEvidenceIsMissionScoped(t *testing.T) {
	inst := &Instance{
		liveness: LiveRunning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
	}
	require.NoError(t, inst.RecordPendingManualAccountSwapMissionDelivery(
		"work", "personal", PromptCouldNotConfirm,
	))
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed())

	inst.RecordPromptAttempt(PromptNotDelivered, time.Now())
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"an unrelated session prompt must not authorize redelivery of the handoff mission")
	require.Equal(t, PromptCouldNotConfirm, inst.ToInstanceData().PendingAccountSwap.MissionDeliveryStatus)
}

func TestStartupUnknownManualAccountSwapDoesNotExposeRetry(t *testing.T) {
	inst := &Instance{
		liveness:            LiveRunning,
		startupStateUnknown: true,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"startup uncertainty must not erase the mission's ambiguous verdict")
	require.False(t, inst.CanRetryPendingManualAccountSwapDelivery(),
		"an unknown replacement runtime must stay inert instead of advertising a delivery retry")
}

func TestUnavailableManualAccountSwapDoesNotExposeRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		liveness   Liveness
		inFlightOp InFlightOp
		userKilled bool
	}{
		{name: "retry in progress", liveness: LiveRunning, inFlightOp: OpRespawning},
		{name: "kill tombstone", liveness: LiveReady, userKilled: true},
		{name: "lost", liveness: LiveLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{
				liveness:   tc.liveness,
				inFlightOp: tc.inFlightOp,
				userKilled: tc.userKilled,
				pendingAccountSwap: &AccountSwapData{
					Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
					MissionDeliveryStatus: PromptCouldNotConfirm,
				},
			}
			require.False(t, inst.CanRetryPendingManualAccountSwapDelivery())
		})
	}
}

func TestRestoreLegacyAccountSwapDoesNotTrustGenericDeliveryEvidence(t *testing.T) {
	data := InstanceData{
		PendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
		LastPromptAttemptAt:      time.Now(),
		LastPromptDeliveryStatus: PromptNotDelivered,
	}
	restored := data.restoreMissingAccountSwapMissionEvidence()
	require.Equal(t, PromptCouldNotConfirm, restored.PendingAccountSwap.MissionDeliveryStatus,
		"session-wide non-delivery may belong to another prompt and cannot authorize mission redelivery")
	require.Empty(t, data.PendingAccountSwap.MissionDeliveryStatus,
		"migration must not mutate the caller's checkpoint")
}

func TestRestoreAccountSwapMissingMissionEvidenceFailsClosed(t *testing.T) {
	data := InstanceData{
		PendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
	}
	restored := data.restoreMissingAccountSwapMissionEvidence()
	require.Equal(t, PromptCouldNotConfirm, restored.PendingAccountSwap.MissionDeliveryStatus,
		"missing durable evidence after replacement startup must not authorize mission redelivery")
}

func TestParkManualAccountSwapRecordsMissionNonDelivery(t *testing.T) {
	inst := &Instance{
		Program:    "claude",
		Account:    "personal",
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(time.Now().Add(time.Hour)))
	require.Equal(t, PromptNotDelivered, inst.ToInstanceData().PendingAccountSwap.MissionDeliveryStatus)
	require.False(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"an incoming limit observed before submission must retain scheduled recovery")
}

func TestParkAutoAccountSwapAtLimitStampsIncomingIdentity(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	inst := &Instance{
		Program:    "codex",
		Account:    "incoming",
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		// The scheduler-driven replacement carries its pending transaction
		// under the same fence as a same-identity resume, while the parked
		// episode's metadata still names the OUTGOING account.
		pendingAccountSwap: &AccountSwapData{From: "outgoing", To: "incoming"},
		limitObservedAt:    first.Add(-48 * time.Hour),
		limitResetAt:       first.Add(6 * 24 * time.Hour),
		limitAgent:         "codex",
		limitAccount:       "outgoing",
	}
	incomingReset := first.Add(2 * time.Hour)
	require.NoError(t, inst.ParkAutoAccountSwapAtLimit(incomingReset))

	require.True(t, inst.LimitReached())
	require.True(t, inst.limitObservedAt.Equal(first),
		"the incoming identity's wall is a fresh sighting, not the outgoing episode's stamp")
	require.True(t, inst.limitResetAt.Equal(incomingReset))
	require.Equal(t, "incoming", inst.limitAccount)
	require.Equal(t, "codex", inst.limitAgent)
	require.Equal(t, PromptNotDelivered, inst.ToInstanceData().PendingAccountSwap.MissionDeliveryStatus,
		"a pre-submission wall is non-delivery evidence for the transaction")
	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.Equal(t, "incoming", observations[0].Account)
	require.True(t, observations[0].ObservedAt.Equal(first),
		"the incoming account's ledger entry carries this sighting, not the parked episode's")
}

func TestParkAutoAccountSwapAtLimitRefusesWithoutAutoTransaction(t *testing.T) {
	resetAt := time.Now().Add(time.Hour)
	for name, inst := range map[string]*Instance{
		"no fence":        {liveness: LiveRunning, pendingAccountSwap: &AccountSwapData{From: "a", To: "b"}},
		"no pending swap": {liveness: LiveRunning, inFlightOp: OpRespawning},
		"manual pending":  {liveness: LiveRunning, inFlightOp: OpRespawning, pendingAccountSwap: &AccountSwapData{Manual: true, From: "a", To: "b"}},
	} {
		require.Error(t, inst.ParkAutoAccountSwapAtLimit(resetAt), name)
		require.False(t, inst.LimitReached(), name)
	}
}
