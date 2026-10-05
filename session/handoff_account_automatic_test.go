package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// automaticAccountSwapLimitInstance builds an Instance in the state
// settleReplacementRuntime presents to the re-park: the resume fence held
// (OpRespawning), an automatic (non-manual) pending replacement committed
// (From=work, To=personal, Manual=false) with the incoming identity already
// installed on the instance (Account=personal). The agent namespace resolves
// from Program ("claude") the way currentAgentNameLocked does with no tmux
// binding, mirroring the manual twin's fixture in handoff_account_test.go.
func automaticAccountSwapLimitInstance(account string) *Instance {
	return &Instance{
		Program:    tmux.ProgramClaude,
		Account:    account,
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			From: "work", To: account,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
}

// TestParkAutomaticAccountSwapAtLimit_AttributesAndRecordsIncomingIdentity is
// the core fix: the automatic account-swap incoming-wall re-park must publish
// the incoming identity's agent namespace and account label as the wall's
// LimitIdentity and append the incoming pair to accountLimitObservations,
// mirroring what the manual twin (ParkManualAccountSwapAtLimit) and the shared
// setLimitReachedLocked body both do. Before the fix the re-park wrote neither,
// so the row hetero-bound (outgoing identity + incoming window) and the ledger
// never gained the incoming entry. Both the live accessors and the durable
// ToInstanceData row must carry the incoming attribution.
func TestParkAutomaticAccountSwapAtLimit_AttributesAndRecordsIncomingIdentity(t *testing.T) {
	reset := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	inst := automaticAccountSwapLimitInstance("personal")

	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(reset))

	require.Equal(t, LiveLimitReached, inst.GetLiveness())
	got, ok := inst.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset), "the incoming wall's reset window is recorded")

	// The live LimitIdentity names the incoming identity, not the outgoing one.
	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent, "limitAgent is the incoming agent namespace")
	require.Equal(t, "personal", account, "limitAccount is the incoming account, not the outgoing work")

	// The durable observations ledger gains the incoming entry.
	observations := inst.AccountLimitObservations()
	require.Contains(t, observations, AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "personal", ResetAt: reset,
	}, "accountLimitObservations must record the incoming (claude, personal) wall")

	// The durable InstanceData row persists both the live attribution and the
	// observation, so a restart reloads the incoming wall rather than the
	// hetero-bound outgoing row.
	data := inst.ToInstanceData()
	require.Equal(t, LiveLimitReached, data.Liveness)
	require.Equal(t, tmux.ProgramClaude, data.LimitAgent)
	require.Equal(t, "personal", data.LimitAccount)
	require.True(t, data.LimitResetAt.Equal(reset))
	require.Contains(t, data.AccountLimitObservations, AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "personal", ResetAt: reset,
	}, "the durable row carries the incoming observation")
}

// TestParkAutomaticAccountSwapAtLimit_RefusesWithoutAutomaticPendingSwap guards
// the entry point against misuse: it requires the resume fence held AND a
// committed automatic (non-manual) pending replacement. Each axis is exercised
// independently so an unconditional-mirror regression that drops the guard, or a
// manual path that reaches it by accident, fails loudly.
func TestParkAutomaticAccountSwapAtLimit_RefusesWithoutAutomaticPendingSwap(t *testing.T) {
	reset := time.Now().Add(time.Hour)

	t.Run("resume fence not held", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		inst.inFlightOp = OpNone
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset))
	})

	t.Run("no pending replacement", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		inst.pendingAccountSwap = nil
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset))
	})

	t.Run("manual pending replacement", func(t *testing.T) {
		// The manual path has its own attributing twin
		// (ParkManualAccountSwapAtLimit); the automatic twin must refuse a
		// manual pending replacement so the two stay mutually exclusive and a
		// manual wall can never bypass the manual-only MissionDeliveryStatus
		// flip by parking through the automatic entry point.
		inst := automaticAccountSwapLimitInstance("personal")
		inst.pendingAccountSwap.Manual = true
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset))
		require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
			"a refused park must not mutate delivery state")
	})
}

// TestParkAutomaticAccountSwapAtLimit_DoesNotFlipManualDeliveryMarker is the
// parity guarantee the caller-scoped fix rests on: the manual twin flips
// pendingAccountSwap.MissionDeliveryStatus to PromptNotDelivered because the
// manual operator-confirmation flow (the c action / web Retry / CLI retry-limit)
// needs positive non-delivery evidence to resume without an ambiguous-delivery
// block. That marker is consulted only on the manual path
// (pendingManualAccountSwapDeliveryUnconfirmedLocked early-returns when the
// swap is not manual), and the automatic resume path (ResumeLimitedSessions →
// resumeFromLimitOutcome) retries after the recorded reset unconditionally,
// with no operator-delivery step. Flipping it on the automatic path would be
// dead state, and could mask a genuine ambiguous delivery on a later manual
// retry of a swap that was originally automatic. So the automatic twin mirrors
// the manual twin's attribution+recording but deliberately omits the flip.
func TestParkAutomaticAccountSwapAtLimit_DoesNotFlipManualDeliveryMarker(t *testing.T) {
	inst := automaticAccountSwapLimitInstance("personal")
	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus)

	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Now().Add(time.Hour)))

	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
		"the automatic twin must NOT flip the manual-only MissionDeliveryStatus marker")
	require.False(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"a non-manual swap must never report a manual delivery-unconfirmed state")
}
