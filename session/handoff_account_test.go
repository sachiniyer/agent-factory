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
