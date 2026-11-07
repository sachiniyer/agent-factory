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

// TestParkCommittedAccountSwapAtLimit_RefusedRetryKeepsTheSightingInstant is
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
//
// The committed twin is the refused retry's path — the send-path pair keeps
// wasParked=false because a LimitReachedError out of the fresh runtime is a
// new sighting by construction. Even handed a later instant, the parked
// re-sight of the unchanged claim must not restamp.
func TestParkCommittedAccountSwapAtLimit_RefusedRetryKeepsTheSightingInstant(t *testing.T) {
	wallSightedAt := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	provenAt := wallSightedAt.Add(time.Hour)
	reparkedAt := wallSightedAt.Add(2 * time.Hour)

	inst := automaticAccountSwapLimitInstance("personal")

	// First refusal: the copy lands stamped with the wall's own sighting
	// instant — a park's copy never postdates its source observation.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, time.Time{}, "", wallSightedAt))

	// The refused retry re-parks while still parked at that wall — the parked
	// re-sight must be a no-op even though the caller's instant is later.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, time.Time{}, "", reparkedAt))

	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(wallSightedAt),
		"a refused retry must not advance the sighting instant of the wall that refused it: got %v, want %v",
		observations[0].ObservedAt, wallSightedAt)
	require.True(t, AccountLimitObservationRetracted(observations[0],
		AccountLimitRetractionData{Agent: tmux.ProgramClaude, Account: "personal", ProvenAt: provenAt}),
		"a proof dated between the two parks must defeat the parked copy")
}

// TestParkedAccountSwap_PostSendResightKeepsTheLiveWall is the other half of
// the retained-observation property, composed with #6921's settle: a wall
// sighted BEFORE the pending notice send is stale evidence the send demonstrably
// got past, so pendingSwapIncomingWallLocked retires it — but a sighting
// landing AFTER the attempt instant is the wall genuinely re-observed, and a
// parked re-sight that swallowed it (#6802's no-op) would leave ObservedAt
// older than the send and make the settle drop a live wall (#6799).
//
// The scenario is the automatic arm's parked-across-the-send window
// (daemon/swap_notice_confirm.go): the row parks at Ta, the notice send lands
// ambiguous at T3 > Ta, and the pane's next limit detection at T4 re-sights
// the same claim through the poll entry. The re-stamp must happen exactly
// once — the first sighting crossing the attempt boundary — and
// SettlePendingAccountSwapNoticeDelivery must then keep the wall parked.
func TestParkedAccountSwap_PostSendResightKeepsTheLiveWall(t *testing.T) {
	parkedAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	sentAt := parkedAt.Add(10 * time.Minute)
	resightedAt := parkedAt.Add(20 * time.Minute)
	clock := parkedAt
	previous := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = previous })

	inst := automaticAccountSwapLimitInstance("personal")
	inst.pendingAccountSwap.ReplacementPanesStarted = true

	// The incoming identity's wall parks the swap at Ta.
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Time{}, ""))

	// The notice send lands at T3 with an ambiguous verdict; the automatic
	// arm keeps the row parked across it and the resume fence comes down.
	inst.inFlightOp = OpNone
	inst.lastPromptAttemptAt = sentAt
	require.NoError(t, inst.RecordPendingAccountSwapNoticeDelivery("work", "personal", PromptCouldNotConfirm))

	// The pane reports the wall again at T4 — a fresh banner parse, not the
	// stored phrase's re-read, so the poll entry lets it through as a new
	// observation. Parked re-sight of the unchanged claim, but it CROSSES
	// the send attempt and must advance the sighting instant.
	clock = resightedAt
	require.True(t, inst.SetLimitReachedAtEpoch(time.Time{}, "outage notice: back soon", LimitBanner{}, inst.stateEpoch))
	require.True(t, inst.GetLiveness() == LiveLimitReached)

	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(resightedAt),
		"the first sighting after the pending send is a new fact: got %v, want %v",
		observations[0].ObservedAt, resightedAt)

	// A tick later re-reading the SAME now-current observation adds nothing:
	// the parked no-op still holds for the unchanged claim once it is fresh.
	clock = resightedAt.Add(time.Minute)
	require.True(t, inst.SetLimitReachedAtEpoch(time.Time{}, "outage notice: still down", LimitBanner{}, inst.stateEpoch))
	observations = inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(resightedAt),
		"once the sighting is already post-send, parked re-sights stay no-ops: got %v", observations[0].ObservedAt)

	// #6921's settle: positive pane evidence for the T3 send retires the swap,
	// but the parked wall must survive — its newest sighting postdates the
	// send, so it is a live wall, not stale pre-send evidence.
	require.NoError(t, inst.SettlePendingAccountSwapNoticeDelivery("work", "personal", sentAt))
	require.Nil(t, inst.pendingAccountSwap, "the settle retires the notice obligation")
	require.Equal(t, LiveLimitReached, inst.GetLiveness(),
		"a wall sighted after the retired send is live evidence, not stale — dropping it is #6799")
	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "personal", account)
}

// TestParkCommittedAccountSwapAtLimit_CopyNeverRewindsTheSightingInstant is the
// second half of the copy's event-time property: a parked copy must never
// REWIND the row's own later sighting. When the row's own runtime reported
// the wall at T40 and a committed copy then lands with a different reset
// claim and a source instant of T10, the overwrite must keep T40 — the row
// genuinely saw the wall at T40, so a usage proof dated in (T10, T40) cannot
// retract a wall the row observed AFTER the proof (#6880). Master's
// unconditional ObservedAt overwrite rewound the row to T10, which let the
// T30 proof retract that wall and parked the session on its own copy.
func TestParkCommittedAccountSwapAtLimit_CopyNeverRewindsTheSightingInstant(t *testing.T) {
	reset := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	laterReset := reset.Add(time.Hour)
	sightedAt := time.Date(2026, 11, 10, 8, 40, 0, 0, time.UTC)
	sourceSightedAt := sightedAt.Add(-30 * time.Minute)
	provenAt := sightedAt.Add(-10 * time.Minute)

	clock := sightedAt
	previous := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = previous })

	inst := automaticAccountSwapLimitInstance("personal")

	// The row's own runtime reports the wall at T40 — a genuine sighting the
	// row made, stamped with the pane's own instant.
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(reset, ""))

	// A committed copy arrives carrying a TIGHTER reset claim — a different
	// fact that updates the row — but stamped with its source's earlier
	// instant T10. The retained claim may change; the sighting instant must
	// never move backwards below a sighting the row genuinely made.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, laterReset, "", sourceSightedAt))

	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ResetAt.Equal(laterReset),
		"the tighter reset claim is the retained fact")
	require.True(t, observations[0].ObservedAt.Equal(sightedAt),
		"a copy must never rewind the row's own later sighting instant: got %v, want %v",
		observations[0].ObservedAt, sightedAt)
	require.False(t, AccountLimitObservationRetracted(observations[0],
		AccountLimitRetractionData{Agent: tmux.ProgramClaude, Account: "personal", ProvenAt: provenAt}),
		"a proof in (T10, T40) must not retract a wall the row saw at T40")
}
