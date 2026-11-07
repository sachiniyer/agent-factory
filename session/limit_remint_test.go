package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The #6959 wedge: a parked row's retry re-parses the banner still on its
// pane, and a clock-time ("reset at 3pm") or relative ("try again in N")
// phrase resolves against the RETRY's clock — so every re-read mints a reset
// later than the stored one and the park's unconditional overwrite rolls the
// wall forward forever. The property: a parked row never re-mints its reset
// from a banner it already parked on — the re-read re-parks with the ORIGINAL
// reset or nothing — while a genuinely new sighting still parks with its own.
//
// staleBanner is the unchanged-banner anchor the retry's readiness wait and
// the status poll report: the send attempt appends BELOW the last banner
// match, so the recorded prefix and match count do not move between reads.
var staleBanner = LimitBanner{Prefix: "session transcript above\n", Count: 1}

// clockResetEvidence is the phrase a clock-time banner's parse consumed.
const clockResetEvidence = "reset at 3pm"

// clockReset returns the instant "reset at 3pm" parses to when read at the
// given sighting — before 3pm it is today, after reset+grace it is tomorrow,
// the roll #6959 names.
func clockReset(sightedAt time.Time) time.Time {
	day := time.Date(sightedAt.Year(), sightedAt.Month(), sightedAt.Day(), 15, 0, 0, 0, time.UTC)
	if !sightedAt.Before(day) {
		day = day.Add(24 * time.Hour)
	}
	return day
}

// requireParkedReset asserts the row is parked and its stored reset is the
// expected instant — the claim the auto-resume scheduler waits on.
func requireParkedReset(t *testing.T, inst *Instance, want time.Time) {
	t.Helper()
	require.True(t, inst.LimitReached(), "the row must stay parked at the wall")
	got, ok := inst.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(want),
		"a re-read of the banner the row parked on must not mint a new reset: got %v, want %v", got, want)
}

// TestLimitPark_RereadSameBannerKeepsReset_PlainResume covers the ordinary
// limit-resume shape: the row parks on a clock-time banner, the retry's send
// clears the wall, and the still-displayed banner is re-parsed against the
// later clock — by the next status poll or by the send path's own park. The
// re-parse resolves the phrase to TOMORROW, and adopting it re-parks the row
// a day out on every pass: the session never resumes.
func TestLimitPark_RereadSameBannerKeepsReset_PlainResume(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)         // today 15:00
	reReadAt := parked.Add(3 * time.Minute) // past reset + the 2m grace
	rolled := clockReset(reReadAt)          // the SAME phrase re-parsed: tomorrow 15:00

	for _, tc := range []struct {
		name    string
		account string
	}{
		// A named account's wall lives in the durable observation ledger; an
		// ambient wall has no ledger row, so only the row's own recorded claim
		// can recognize the re-read.
		{"named account", "work"},
		{"ambient identity", ""},
	} {
		t.Run(tc.name+" re-park after the resume's clear", func(t *testing.T) {
			inst := &Instance{Program: tmux.ProgramClaude, Account: tc.account}
			inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner)
			requireParkedReset(t, inst, parked)

			// resumeFromLimitLockedOutcome's send lands and the retry clears
			// the wall; the unchanged banner still on the pane is then
			// re-detected at the later clock.
			inst.ClearLimitReached()
			require.True(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, staleBanner, inst.stateEpoch))

			requireParkedReset(t, inst, parked)
			if tc.account != "" {
				observations := inst.AccountLimitObservations()
				require.Len(t, observations, 1)
				require.True(t, observations[0].ResetAt.Equal(parked),
					"the durable wall claim must keep the first parse: got %v", observations[0].ResetAt)
			}
		})

		t.Run(tc.name+" poll re-read while parked", func(t *testing.T) {
			inst := &Instance{Program: tmux.ProgramClaude, Account: tc.account}
			inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner)

			require.True(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, staleBanner, inst.stateEpoch))
			requireParkedReset(t, inst, parked)
		})
	}
}

// TestLimitPark_RereadSameBannerKeepsReset_ManualSwap is the deliverManual
// AccountMission shape: the send path clears the parked wall before pasting
// the mission — an outgoing wall is not evidence about the replacement
// identity — and the readiness wait re-parses the SAME banner still on the
// replacement pane. The re-parse's rolled instant must not become the row's
// new reset.
func TestLimitPark_RereadSameBannerKeepsReset_ManualSwap(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	rolled := clockReset(reReadAt)

	inst := &Instance{
		Program:    tmux.ProgramClaude,
		Account:    "personal",
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal",
			ReplacementPanesStarted: true,
			MissionDeliveryStatus:   PromptCouldNotConfirm,
		},
	}
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	// The retry's send clears the wall, then readiness re-reads the unchanged
	// banner and parks again — the exact deliverManualAccountMission order.
	inst.ClearLimitReached()
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))

	requireParkedReset(t, inst, parked)
	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ResetAt.Equal(parked),
		"the incoming wall's durable claim keeps the first parse: got %v", observations[0].ResetAt)
}

// TestLimitPark_RereadSameBannerKeepsReset_AutomaticSwap is the
// settleReplacementRuntime probeAlive arm: a refused automatic-swap retry
// keeps the row parked across its readiness wait, which re-reads the SAME
// banner off the SAME replacement pane. The re-park must keep the first
// parse's reset — master's unconditional overwrite minted tomorrow.
func TestLimitPark_RereadSameBannerKeepsReset_AutomaticSwap(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	rolled := clockReset(parked.Add(3 * time.Minute))

	inst := automaticAccountSwapLimitInstance("personal")
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ResetAt.Equal(parked),
		"the durable wall claim must keep the first parse: got %v", observations[0].ResetAt)
}

// TestLimitPark_RereadSameBannerKeepsReset_AgentHandoff covers the handoff
// park transition: waitAndSubmitHandoffMission's readiness wait re-parses the
// banner the incoming agent's pane still shows, and the ParkHandoff event
// must not mint the rolled instant.
func TestLimitPark_RereadSameBannerKeepsReset_AgentHandoff(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	rolled := clockReset(parked.Add(3 * time.Minute))

	inst := &Instance{Program: tmux.ProgramClaude, liveness: LiveRunning}
	require.NoError(t, inst.Transition(BeginHandoff()))
	require.NoError(t, inst.Transition(ParkHandoff(parked, clockResetEvidence, staleBanner)))
	requireParkedReset(t, inst, parked)

	// The parked mission's retry re-enters the replacement fence and the wait
	// re-reads the unchanged banner.
	require.NoError(t, inst.Transition(BeginHandoff()))
	require.NoError(t, inst.Transition(ParkHandoff(rolled, clockResetEvidence, staleBanner)))
	requireParkedReset(t, inst, parked)
}

// TestLimitPark_RereadSameBannerKeepsReset_CrossAgentSwap is the manual-swap
// shape with the incoming identity in a DIFFERENT agent namespace: the retry
// re-reads the same banner on the codex replacement pane, and the wall's
// recorded namespace must match for the re-read to keep the original reset.
func TestLimitPark_RereadSameBannerKeepsReset_CrossAgentSwap(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	rolled := clockReset(parked.Add(3 * time.Minute))

	inst := &Instance{
		Program:    tmux.ProgramCodex,
		Account:    "personal",
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", AccountAgent: tmux.ProgramCodex,
			ReplacementPanesStarted: true,
			MissionDeliveryStatus:   PromptCouldNotConfirm,
		},
	}
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	inst.ClearLimitReached()
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)
}

// TestLimitPark_CommittedSwapCopyKeepsRecordedReset pins the pre-spawn
// re-check shape: ParkCommittedAccountSwapAtLimit copies a wall fact it never
// parsed a banner for, so the roll can only arrive through a caller's stale
// input. A copy whose phrase matches the row's recorded wall keeps the first
// parse — the live claim and the durable observation both refuse the rolled
// instant — while a copy carrying a genuinely different claim still applies.
func TestLimitPark_CommittedSwapCopyKeepsRecordedReset(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	rolled := clockReset(parked.Add(3 * time.Minute))

	inst := automaticAccountSwapLimitInstance("personal")
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	// A refused retry's re-admission hands the copy the wall's own phrase —
	// the first parse stays authoritative for the same claim.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, rolled, clockResetEvidence, sightedAt.Add(3*time.Hour)))
	requireParkedReset(t, inst, parked)
	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ResetAt.Equal(parked),
		"a same-phrase copy must not move the durable reset claim: got %v", observations[0].ResetAt)

	// A copy carrying a genuinely different claim is a new fact and applies.
	newReset := parked.Add(6 * time.Hour)
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, newReset, "", sightedAt.Add(4*time.Hour)))
	requireParkedReset(t, inst, newReset)
}

// TestLimitPark_RereadSamePhraseKeepsReset_CodexRelative is the Codex shape:
// a "try again in N" phrase resolves relative to the READING clock, so the
// unchanged banner mints a later instant on every pass exactly like the
// clock-time form.
func TestLimitPark_RereadSamePhraseKeepsReset_CodexRelative(t *testing.T) {
	phrase := "try again in 4 days 2 hours 46 minutes."
	sightedAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	parked := sightedAt.Add(4*24*time.Hour + 2*time.Hour + 46*time.Minute)
	reReadAt := parked.Add(3 * time.Minute)
	rolled := reReadAt.Add(4*24*time.Hour + 2*time.Hour + 46*time.Minute)

	t.Run("plain resume re-park", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramCodex}
		inst.SetLimitReachedAtEvidence(parked, phrase, staleBanner)
		inst.ClearLimitReached()
		require.True(t, inst.SetLimitReachedAtEpoch(rolled, phrase, staleBanner, inst.stateEpoch))
		requireParkedReset(t, inst, parked)
	})

	t.Run("automatic swap parked re-read", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, phrase, staleBanner, false))
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(rolled, phrase, staleBanner, false))
		requireParkedReset(t, inst, parked)
	})

	t.Run("handoff park retry", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramCodex, liveness: LiveRunning}
		require.NoError(t, inst.Transition(BeginHandoff()))
		require.NoError(t, inst.Transition(ParkHandoff(parked, phrase, staleBanner)))
		require.NoError(t, inst.Transition(BeginHandoff()))
		require.NoError(t, inst.Transition(ParkHandoff(rolled, phrase, staleBanner)))
		requireParkedReset(t, inst, parked)
	})
}

// TestLimitPark_NewSightingKeepsItsOwnReset is the property's other half: a
// sighting the row never recorded still parks with its own parse — a banner
// the wall re-worded (different phrase) is a new claim, and a wall on a
// different identity is a different fact.
func TestLimitPark_NewSightingKeepsItsOwnReset(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	later := clockReset(parked.Add(3 * time.Minute))

	t.Run("a different reset phrase is a new claim", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner)
		inst.ClearLimitReached()
		require.True(t, inst.SetLimitReachedAtEpoch(later, "reset at 9pm", staleBanner, inst.stateEpoch))
		requireParkedReset(t, inst, later)
	})

	t.Run("a banner printed below the recorded one is a new wall", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner)
		inst.ClearLimitReached()
		newBanner := LimitBanner{Prefix: "session transcript above\nbanner line\n", Count: 2}
		require.True(t, inst.SetLimitReachedAtEpoch(later, "reset at 9pm", newBanner, inst.stateEpoch))
		requireParkedReset(t, inst, later)
	})

	t.Run("a fresh runtime's identical banner is a new sighting", func(t *testing.T) {
		// The swap retry launched a replacement runtime whose pane emits the
		// same clause at the same anchor: a coincidence of identical display,
		// not a re-read — sendPathReSightingLocked already classifies the
		// sighting fresh, and its own parse is the wall's current claim.
		inst := automaticAccountSwapLimitInstance("personal")
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, inst, parked)
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(later, clockResetEvidence, staleBanner, true))
		requireParkedReset(t, inst, later)
		obs := inst.AccountLimitObservations()
		require.True(t, obs[0].ResetAt.Equal(later),
			"the fresh runtime's claim tightens the durable wall: got %v", obs[0].ResetAt)
	})

	t.Run("a reused phrase at a different anchor is a new wall", func(t *testing.T) {
		// The day-after shape: a NEW banner at a different pane position
		// happens to re-use "reset at 3pm". The phrase matches the recorded
		// wall — the durable ledger row too — but the anchor proves it is not
		// that banner, so its own parse applies and the ledger tightens.
		inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner)
		inst.ClearLimitReached()
		moved := LimitBanner{Prefix: "entirely different transcript\nwith more rows\n", Count: 1}
		require.True(t, inst.SetLimitReachedAtEpoch(later, clockResetEvidence, moved, inst.stateEpoch))
		requireParkedReset(t, inst, later)
		obs := inst.AccountLimitObservations()
		require.Len(t, obs, 1, "the same reset-claim KIND still merges")
		require.True(t, obs[0].ResetAt.Equal(later),
			"the durable row must tighten to the new wall's own instant, not keep yesterday's")
	})
}
