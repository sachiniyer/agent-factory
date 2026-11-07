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

	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(reset, ""))

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
	requireAccountLimitObservation(t, observations, tmux.ProgramClaude, "personal", reset)

	// The durable InstanceData row persists both the live attribution and the
	// observation, so a restart reloads the incoming wall rather than the
	// hetero-bound outgoing row.
	data := inst.ToInstanceData()
	require.Equal(t, LiveLimitReached, data.Liveness)
	require.Equal(t, tmux.ProgramClaude, data.LimitAgent)
	require.Equal(t, "personal", data.LimitAccount)
	require.True(t, data.LimitResetAt.Equal(reset))
	requireAccountLimitObservation(t, data.AccountLimitObservations, tmux.ProgramClaude, "personal", reset)
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
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset, ""))
	})

	t.Run("no pending replacement", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		inst.pendingAccountSwap = nil
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset, ""))
	})

	t.Run("manual pending replacement", func(t *testing.T) {
		// The manual path has its own attributing twin
		// (ParkManualAccountSwapAtLimit); the automatic twin must refuse a
		// manual pending replacement so the two stay mutually exclusive and a
		// manual wall can never bypass the manual-only MissionDeliveryStatus
		// flip by parking through the automatic entry point.
		inst := automaticAccountSwapLimitInstance("personal")
		inst.pendingAccountSwap.Manual = true
		require.Error(t, inst.ParkAutomaticAccountSwapAtLimit(reset, ""))
		require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
			"a refused park must not mutate delivery state")
	})
}

// TestParkAutomaticAccountSwapAtLimit_DoesNotFlipManualDeliveryMarker is the
// parity guarantee the caller-scoped fix rests on: the manual twin flips
// pendingAccountSwap.MissionDeliveryStatus to PromptNotDelivered because the
// manual operator-confirmation flow (the c action / web Retry / CLI retry-limit)
// needs positive non-delivery evidence to resume without an ambiguous-delivery
// block. The marker also gates the AUTOMATIC swap's scheduled retry since
// #6767 — accountSwapScheduledResumeEligible reads it through
// PendingAccountSwapDeliveryUnconfirmed — but its ambiguous verdict belongs to
// the notice send's own evidence contract (deliverAutomaticAccountNotice's
// Begin/Record pair and its re-verification watch): a wall found before this
// attempt's send cannot retroactively settle a notice an earlier send may
// already have delivered, so the flip stays manual-scoped. The automatic twin
// mirrors the manual twin's attribution+recording but deliberately omits it.
func TestParkAutomaticAccountSwapAtLimit_DoesNotFlipManualDeliveryMarker(t *testing.T) {
	inst := automaticAccountSwapLimitInstance("personal")
	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus)

	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Now().Add(time.Hour), ""))

	require.Equal(t, PromptCouldNotConfirm, inst.pendingAccountSwap.MissionDeliveryStatus,
		"the automatic twin must NOT rewrite the notice send's delivery verdict")
	require.False(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"a non-manual swap must never report a manual delivery-unconfirmed state")
}

// TestParkAutomaticAccountSwapAtLimit_RefusedRetryKeepsTheSightingInstant is
// the #6880 retained-observation wedge: a refused committed-swap retry re-parks
// on the same wall, and that re-park must be the parked re-sight
// setLimitReachedLocked already treats as a no-op — the observation's event
// time is the wall's own sighting, not whenever af happened to refuse again.
// Master's unconditional wasParked=false re-stamped the row's own ObservedAt
// on every refusal, so a usage proof recorded BETWEEN two refused parks could
// never defeat the copy and the session stayed walled by its own observation
// forever. A proof dated between the two parks must free the session. The
// proof's ProvenAt sits before the second park's instant — never future-dated,
// the shape production's usageReadingProof accepts.
func TestParkAutomaticAccountSwapAtLimit_RefusedRetryKeepsTheSightingInstant(t *testing.T) {
	parkedAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	provenAt := parkedAt.Add(time.Minute)
	reparkedAt := parkedAt.Add(2 * time.Minute)
	clock := parkedAt
	previous := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = previous })

	inst := automaticAccountSwapLimitInstance("personal")
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Time{}, ""))

	// The refused retry sees the same wall again under a later clock and
	// re-parks — the shape of every committed-swap refusal loop.
	clock = reparkedAt
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Time{}, ""))

	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(parkedAt),
		"a refused retry must not advance the sighting instant of the wall that refused it: got %v, want %v",
		observations[0].ObservedAt, parkedAt)
	require.True(t, AccountLimitObservationRetracted(observations[0],
		AccountLimitRetractionData{Agent: tmux.ProgramClaude, Account: "personal", ProvenAt: provenAt}),
		"a proof dated between the two parks must defeat the parked copy")
}
