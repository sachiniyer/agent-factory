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

// An AUTOMATIC replacement that meets a wall during readiness must file it
// under the INCOMING identity (#4404 review). The outgoing account's wall is
// what started the swap; re-parking with only the reset time left
// limit_account on that outgoing account, so the create-time router and the
// swap scheduler both read the incoming account — the one readiness had just
// proven walled — as healthy, and deletion retained nothing for it either.
func TestReparkReplacementLimitChargesTheIncomingAccount(t *testing.T) {
	outgoingReset := time.Now().Add(30 * time.Minute).UTC()
	incomingReset := time.Now().Add(2 * time.Hour).UTC()
	inst := &Instance{Program: "codex", Account: "work", accountAutoSelected: true, liveness: LiveRunning}
	inst.SetLimitReached(outgoingReset)
	inst.inFlightOp = OpRespawning
	_, err := inst.SelectAccountAutomatically("work", "personal")
	require.NoError(t, err)

	require.NoError(t, inst.ReparkReplacementLimitUnderResumeFence(incomingReset))

	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok, "the replacement is parked at a wall")
	require.Equal(t, "codex", agent)
	require.Equal(t, "personal", account, "the wall belongs to the identity readiness just ran")
	reset, ok := inst.LimitResetAt()
	require.True(t, ok)
	require.WithinDuration(t, incomingReset, reset, time.Second)
	require.ElementsMatch(t, []AccountLimitObservationData{
		{Agent: "codex", Account: "work", ResetAt: outgoingReset},
		{Agent: "codex", Account: "personal", ResetAt: incomingReset},
	}, inst.AccountLimitObservations(), "both walls stay on record — the outgoing one did not lift")

	data := inst.ToInstanceData()
	limited, observations := AccountLimitEvidenceFromData(data)
	require.Equal(t, "personal", limited, "the durable row a restart or a delete reads agrees")
	require.Contains(t, observations, AccountLimitObservationData{Agent: "codex", Account: "personal", ResetAt: incomingReset})
	require.NotNil(t, data.PendingAccountSwap, "the committed swap stays owed to its retry")
	require.Equal(t, "work", data.PendingAccountSwap.From)
	require.Equal(t, "personal", data.PendingAccountSwap.To)
	require.False(t, data.PendingAccountSwap.Manual)
}

// The privileged re-park is still fenced: without the resume fence it refuses,
// exactly as the attribution-free twin it replaced on this path does.
func TestReparkReplacementLimitRequiresTheResumeFence(t *testing.T) {
	inst := &Instance{Program: "codex", Account: "personal", accountAutoSelected: true, liveness: LiveRunning}
	require.Error(t, inst.ReparkReplacementLimitUnderResumeFence(time.Now().Add(time.Hour)))
	require.Equal(t, LiveRunning, inst.GetLiveness())
	require.Empty(t, inst.AccountLimitObservations(), "a refused re-park records no evidence")
}
