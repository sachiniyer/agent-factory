package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// freezeInstanceNow pins instanceNow at the instant the test needs — the
// residue and expiry decisions compare the recorded reset against the
// reading clock, and the parked fixture's instant is a fixed 2026 date the
// process clock will never reach.
func freezeInstanceNow(at time.Time) func() {
	prev := instanceNow
	instanceNow = func() time.Time { return at }
	return func() { instanceNow = prev }
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
		t.Run(tc.name+" residue after the send landed is not a wall", func(t *testing.T) {
			// The resume send has landed and the wall cleared; the recorded
			// reset is now past, so the still-visible banner is residue — the
			// poll must never re-park the row onto the claim that already
			// fired. Without this the row re-parks, the sweep's retry state
			// resets to a due episode, and the prompt replays on every poll
			// (#6959 review).
			defer freezeInstanceNow(reReadAt)()
			inst := &Instance{Program: tmux.ProgramClaude, Account: tc.account}
			inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
			requireParkedReset(t, inst, parked)
			inst.ClearLimitReached()

			require.False(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, staleBanner, inst.stateEpoch),
				"the row's own expired parked banner is residue, not a wall")
			require.False(t, inst.LimitReached(), "residue must never re-park the row")
			if tc.account != "" {
				observations := inst.AccountLimitObservations()
				require.Len(t, observations, 1)
				require.True(t, observations[0].ResetAt.Equal(parked),
					"the durable wall claim must keep the first parse: got %v", observations[0].ResetAt)
			}

			// A genuinely new sighting — a banner printed BELOW the residue —
			// still parks with its own parse.
			newBanner := LimitBanner{Prefix: staleBanner.Prefix + "banner line\n", Count: 2}
			require.True(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, newBanner, inst.stateEpoch))
			requireParkedReset(t, inst, rolled)
		})

		t.Run(tc.name+" poll re-read while parked", func(t *testing.T) {
			inst := &Instance{Program: tmux.ProgramClaude, Account: tc.account}
			inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)

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
	// the first parse stays authoritative for the same claim. The copy's
	// sighting stamps no newer than the row's own record — a fresher source
	// instant would make the copy genuinely newer evidence (#6959 review).
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, rolled, clockResetEvidence, instanceNow().Add(-time.Minute)))
	requireParkedReset(t, inst, parked)
	observations := inst.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ResetAt.Equal(parked),
		"a same-phrase copy must not move the durable reset claim: got %v", observations[0].ResetAt)

	// A copy carrying a genuinely different claim is a new fact and applies.
	newReset := parked.Add(6 * time.Hour)
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, newReset, "", sightedAt.Add(4*time.Hour)))
	requireParkedReset(t, inst, newReset)

	t.Run("a copy sighted no newer than the record borrows the recorded claim", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		inst.ClearLimitReached()
		require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, rolled, clockResetEvidence, instanceNow().Add(-time.Minute)))
		requireParkedReset(t, inst, parked)
	})

	t.Run("a copy sighted newer than the record keeps the source's reset", func(t *testing.T) {
		// A sibling re-observed the same wall AFTER this row's own record:
		// its resolved deadline is the wall's current claim and stands —
		// phrase equality alone cannot tell a re-parse from a fresher fact.
		inst := automaticAccountSwapLimitInstance("personal")
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		inst.ClearLimitReached()
		later := clockReset(parked.Add(25 * time.Hour))
		require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, later, clockResetEvidence, instanceNow().Add(2*time.Hour)))
		requireParkedReset(t, inst, later)
	})
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
		// The re-read must land INSIDE the recorded window: an expired own
		// banner is residue and the park refuses outright, which is the other
		// half of the property the residue subtests already pin. Freezing
		// keeps the assertion on the drift itself as real time passes.
		defer freezeInstanceNow(parked.Add(-time.Minute))()
		inst := &Instance{Program: tmux.ProgramCodex}
		inst.SetLimitReachedAtEvidence(parked, phrase, staleBanner, false)
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
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
		inst.ClearLimitReached()
		require.True(t, inst.SetLimitReachedAtEpoch(later, "reset at 9pm", staleBanner, inst.stateEpoch))
		requireParkedReset(t, inst, later)
	})

	t.Run("a banner printed below the recorded one is a new wall", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
		inst.ClearLimitReached()
		newBanner := LimitBanner{Prefix: "session transcript above\nbanner line\n", Count: 2}
		require.True(t, inst.SetLimitReachedAtEpoch(later, "reset at 9pm", newBanner, inst.stateEpoch))
		requireParkedReset(t, inst, later)
	})

	t.Run("a grown count overrides the send-path phrase fallback", func(t *testing.T) {
		// A wall recorded at byte zero carries an empty prefix but still
		// counts its match; a second emission on the same pane re-using the
		// phrase arrives with count 2. The anchorless phrase fallback must
		// defer to that positional evidence — without the count gate the
		// sighting classifies as a parked re-sight and the first wall's
		// claim vetoes the second wall's reset (#6959 review).
		inst := automaticAccountSwapLimitInstance("personal")
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence,
			LimitBanner{Prefix: "", Count: 1}, false))
		requireParkedReset(t, inst, parked)
		secondWall := LimitBanner{Prefix: "usage limit reached. reset at 3pm\nmore transcript\n", Count: 2}
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(later, clockResetEvidence, secondWall, false))
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

	t.Run("a fresh runtime on a cleared row is a new sighting", func(t *testing.T) {
		// The healthy-handoff shape: the row cleared its wall and sits
		// UNPARKED when a replacement runtime arrives — only the shadow
		// remembers the old banner, and a coincidentally identical emission
		// on the fresh pane is new wall evidence, not a re-read.
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
		inst.ClearLimitReached()
		require.NoError(t, inst.ParkManualAccountSwapAtLimit(later, clockResetEvidence, staleBanner, true))
		requireParkedReset(t, inst, later)
		obs := inst.AccountLimitObservations()
		require.True(t, obs[0].ResetAt.Equal(later),
			"the fresh runtime's claim must stand over the shadowed wall: got %v", obs[0].ResetAt)
	})

	t.Run("a fresh handoff runtime's identical banner is a new sighting", func(t *testing.T) {
		// The A→B→A shape: B's delivery never parked, so the shadow still
		// names A's old wall when the handoff back spawns a NEW incoming A
		// runtime. The producer marks that sighting fresh — a coincidentally
		// identical phrase at the same anchor on the new pane is the wall's
		// current claim, not a re-parse of the retired pane's banner.
		inst := &Instance{Program: tmux.ProgramClaude, Account: "personal", liveness: LiveRunning}
		require.NoError(t, inst.Transition(BeginHandoff()))
		require.NoError(t, inst.Transition(ParkHandoff(parked, clockResetEvidence, staleBanner)))
		requireParkedReset(t, inst, parked)
		require.NoError(t, inst.Transition(BeginHandoff()))
		require.NoError(t, inst.Transition(ParkHandoff(later, clockResetEvidence, staleBanner).OnFreshRuntime()))
		requireParkedReset(t, inst, later)
	})

	t.Run("a reused phrase at a different anchor is a new wall", func(t *testing.T) {
		// The day-after shape: a NEW banner at a different pane position
		// happens to re-use "reset at 3pm". The phrase matches the recorded
		// wall — the durable ledger row too — but the anchor proves it is not
		// that banner, so its own parse applies and the ledger tightens.
		inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
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

// TestLimitPark_RestartKeepsRecordedWall is the restart arm of #6959: the
// stale-read decision must survive a daemon restart. A parked swap row's
// first same-pane re-read after the restart used to be classified fresh —
// the banner anchor and "last wall" shadow lived only in memory — and rolled
// the reset forward. The persisted anchor and shadow now decide exactly as
// they did before the restart; a record too old to carry the anchor still
// recognizes its wall by the reset phrase it consumed.
func TestLimitPark_RestartKeepsRecordedWall(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	rolled := clockReset(reReadAt)

	restore := func(t *testing.T, inst *Instance) *Instance {
		t.Helper()
		data := inst.ToInstanceData()
		encoded, err := json.Marshal(data)
		require.NoError(t, err)
		var decoded InstanceData
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		// The restart half the test needs is the limit-claim and shadow
		// restore; the worktree machinery is not under test, so mark the
		// record startup-unknown to skip its strict empty-path check.
		decoded.StartupStateUnknown = true
		restored, err := FromInstanceData(decoded)
		require.NoError(t, err)
		return restored
	}

	t.Run("manual swap: persisted anchor recognizes the same pane", func(t *testing.T) {
		inst := &Instance{
			Program: tmux.ProgramClaude, Account: "personal", Path: initTempGitRepo(t),
			liveness: LiveRunning, inFlightOp: OpRespawning,
			pendingAccountSwap: &AccountSwapData{
				Manual: true, From: "work", To: "personal",
				ReplacementPanesStarted: true,
				MissionDeliveryStatus:   PromptCouldNotConfirm,
			},
		}
		require.NoError(t, inst.ParkManualAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, inst, parked)

		restored := restore(t, inst)
		require.True(t, restored.LimitReached(), "the parked wall itself is durable")
		// The restart recovery re-arms the swap fence before the retry; the
		// in-memory op is what the park guard consults, so the fixture arms it
		// the way recovery does.
		restored.inFlightOp = OpRespawning
		require.NoError(t, restored.ParkManualAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, restored, parked)
		observations := restored.AccountLimitObservations()
		require.Len(t, observations, 1)
		require.True(t, observations[0].ResetAt.Equal(parked),
			"a restarted same-pane re-read must keep the first parse: got %v", observations[0].ResetAt)
	})

	t.Run("automatic swap: persisted anchor recognizes the same pane", func(t *testing.T) {
		inst := automaticAccountSwapLimitInstance("personal")
		inst.Path = initTempGitRepo(t)
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, inst, parked)

		restored := restore(t, inst)
		restored.inFlightOp = OpRespawning
		require.NoError(t, restored.ParkAutomaticAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, restored, parked)
	})

	t.Run("pre-anchor record: the durable phrase still recognizes its wall", func(t *testing.T) {
		// A row parked before the banner anchor became durable restores with
		// no anchor at all — a missing anchor is not evidence of a new banner,
		// so the reset phrase is what still proves the re-read.
		inst := automaticAccountSwapLimitInstance("personal")
		inst.Path = initTempGitRepo(t)
		require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
		inst.limitBannerPrefix = "" // simulate the row an older binary wrote
		inst.limitBannerCount = 0
		inst.lastLimitWall = limitWallClaim{} // and no serialized shadow either
		data := inst.ToInstanceData()
		require.Empty(t, data.LimitBannerPrefix, "pre-anchor rows serialize no anchor")
		var decoded InstanceData
		encoded, err := json.Marshal(data)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		decoded.StartupStateUnknown = true
		restored, err := FromInstanceData(decoded)
		require.NoError(t, err)

		restored.inFlightOp = OpRespawning
		require.NoError(t, restored.ParkAutomaticAccountSwapAtLimit(rolled, clockResetEvidence, staleBanner, false))
		requireParkedReset(t, restored, parked)
	})
}

// TestLimitPark_LiveClaimOrdering is the #6959 review's live-claim arm: a
// committed copy whose source sighted the wall AFTER the parked row recorded
// its claim is fresher evidence and must stand over the row's own parse — the
// same provenance rule the durable ledger arm already applies.
func TestLimitPark_LiveClaimOrdering(t *testing.T) {
	sightedAt := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)     // the row's own 11-07 record
	later := parked.Add(24 * time.Hour) // the sibling's 11-08 sighting

	inst := automaticAccountSwapLimitInstance("personal")
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(parked, clockResetEvidence, staleBanner, false))
	requireParkedReset(t, inst, parked)

	// A committed copy carrying the source's newer sighting instant: the
	// row's live claim is the OLDER record of this wall, so the copy's own
	// reset applies rather than the stale parked one.
	require.NoError(t, inst.ParkCommittedAccountSwapAtLimit(tmux.ProgramClaude, later, clockResetEvidence, instanceNow().Add(2*time.Hour)))
	requireParkedReset(t, inst, later)
}

// TestLimitPark_RecordedWallExpired is the readiness arm of #6959: at
// reset+grace the still-visible parked banner must read as residue so the
// retry's send proceeds past it. A banner the row never recorded — or one
// printed below the recorded one — remains a live wall.
func TestLimitPark_RecordedWallExpired(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	defer freezeInstanceNow(reReadAt)()

	inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
	requireParkedReset(t, inst, parked)

	require.True(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
		"the row's own parked banner past its reset is residue — readiness must send past it")

	// A banner printed BELOW the recorded one is a genuinely new wall.
	require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix+"banner line\n", 2, clockResetEvidence, false),
		"a newer emission is a live wall, not residue")
	// A differently-worded claim at the same position is a new wall.
	require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, "try again in 2 hours", false),
		"a re-worded clause is a live wall, not residue")

	// Before the reset passes the same banner is still the claimed wall —
	// the #6956 wait must keep holding it.
	defer freezeInstanceNow(parked.Add(-time.Minute))()
	require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
		"a banner whose claimed reset has not passed is still a wall")
}

// TestLimitPark_AnchorlessCountGrowthIsANewWall is the count arm of the
// residue gate: a recorded banner that started at byte zero legitimately
// carries an EMPTY prefix — its count is the only positional identity the
// claim has — so the anchorless phrase fallback must not waive the growth
// evidence a second emission leaves in the capture (#6959 review).
func TestLimitPark_AnchorlessCountGrowthIsANewWall(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	rolled := clockReset(reReadAt)
	defer freezeInstanceNow(reReadAt)()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, LimitBanner{Count: 1}, false)
	requireParkedReset(t, inst, parked)
	inst.ClearLimitReached()

	// A second emission re-using the same phrase below the first: the
	// capture's last match moved past the first banner's own line and the
	// count grew — a genuinely new wall, not the recorded one's re-read.
	second := LimitBanner{Prefix: "transcript\nClaude usage limit line\n", Count: 2}
	require.False(t, inst.RecordedLimitWallExpired(second.Prefix, second.Count, clockResetEvidence, false),
		"a grown match count is a new wall; the anchorless fallback cannot read it as residue")
	require.True(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, second, inst.stateEpoch),
		"the new emission must park with its own parse")
	requireParkedReset(t, inst, rolled)
}

// TestLimitPark_ResidueBindsToRecordedRuntime is the runtime-provenance arm
// of the residue gate: a claim pins the concrete pane-root process its wall
// was sighted on, so a REPLACEMENT runtime displaying a coincidentally
// identical banner is a new emission — the A→B→A handoff where the
// replacement re-meets the same provider wall. The binding travels with the
// claim: the respawn arm's repark restores it from the shadow, and the
// serialized row keeps it across a daemon restart (#6959 review).
func TestLimitPark_ResidueBindsToRecordedRuntime(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	rolled := clockReset(reReadAt)

	t.Run("same runtime keeps the residue read", func(t *testing.T) {
		defer freezeInstanceNow(reReadAt)()
		inst := &Instance{Program: tmux.ProgramClaude, runtimePID: 4242, runtimeStartID: 9}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
		require.True(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
			"the still-visible banner on the runtime that recorded it stays residue")
	})

	t.Run("replaced runtime's matching banner is a new wall", func(t *testing.T) {
		defer freezeInstanceNow(reReadAt)()
		inst := &Instance{Program: tmux.ProgramClaude, runtimePID: 4242, runtimeStartID: 9}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
		inst.ClearLimitReached()
		require.True(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
			"before replacement the residue read holds")

		// The process the wall was sighted on is gone: an A→B→A handoff or a
		// respawn put a fresh pane in its place. A coincidentally identical
		// banner on that pane is a different emission.
		inst.runtimePID = 5555
		require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
			"a replaced runtime's matching banner is a new wall, not residue")
		require.True(t, inst.SetLimitReachedAtEpoch(rolled, clockResetEvidence, staleBanner, inst.stateEpoch),
			"the fresh runtime's wall must park with its own parse")
		requireParkedReset(t, inst, rolled)
	})

	t.Run("respawn repark keeps the wall's recorded runtime", func(t *testing.T) {
		defer freezeInstanceNow(reReadAt)()
		inst := &Instance{Program: tmux.ProgramClaude, runtimePID: 4242, runtimeStartID: 9, inFlightOp: OpRespawning}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)

		// The respawn replaced the process and its ConfirmLive cleared the
		// live claim fields; the resume's repark restores the wall claim —
		// bound to the runtime the wall was sighted on, not the new one.
		inst.liveness = LiveRunning
		inst.limitResetAt = time.Time{}
		inst.limitResetEvidence = ""
		inst.limitBannerPrefix = ""
		inst.limitBannerCount = 0
		inst.limitWallRuntimePID = 0
		inst.limitWallRuntimeStartID = 0
		inst.runtimePID = 5555
		require.NoError(t, inst.ReparkLimitUnderResumeFence(parked))
		requireParkedReset(t, inst, parked)

		require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
			"the reparked claim stays pinned to the sighting runtime — the fresh pane's matching banner is a new wall")
	})

	t.Run("the runtime binding is durable", func(t *testing.T) {
		defer freezeInstanceNow(reReadAt)()
		inst := &Instance{Program: tmux.ProgramClaude, runtimePID: 4242, runtimeStartID: 9}
		inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
		data := inst.ToInstanceData()
		require.Equal(t, 4242, data.LimitWallRuntimePID)
		require.Equal(t, uint64(9), data.LimitWallRuntimeStartID)
		require.NotNil(t, data.LastLimitWall)
		require.Equal(t, 4242, data.LastLimitWall.RuntimePID)
		require.Equal(t, uint64(9), data.LastLimitWall.RuntimeStartID)
	})
}

// TestLimitWallPrefixTail_PreservesUTF8Boundary: the persisted anchor tail
// must cut on a rune boundary — a mid-rune slice marshals through JSON as
// U+FFFD, and the repaired bytes never suffix-match the unchanged pane
// again, so a restarted row reads its recorded wall's re-parse as fresh
// evidence and rolls the reset forward (#6959 review).
func TestLimitWallPrefixTail_PreservesUTF8Boundary(t *testing.T) {
	// 700 three-byte runes (2100 bytes) push the cut inside a multibyte
	// character on any boundary alignment.
	prefix := strings.Repeat("界", 700) + "\n"
	require.Greater(t, len(prefix), limitWallPrefixPersistMax)

	tail := limitWallPrefixTail(prefix)
	require.True(t, utf8.ValidString(tail), "the persisted tail must be valid UTF-8")
	require.LessOrEqual(t, len(tail), limitWallPrefixPersistMax)
	require.True(t, strings.HasSuffix(prefix, tail), "the tail is a clean suffix of the capture")

	// The round trip that used to corrupt the anchor: JSON serialization
	// preserves the tail verbatim, and the position compare the residue
	// and stale-read decisions run still recognizes the unchanged pane.
	encoded, err := json.Marshal(tail)
	require.NoError(t, err)
	var restored string
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.Equal(t, tail, restored)
	require.True(t, sameLimitBannerPosition(restored, prefix),
		"the restored anchor must still prove the unchanged pane's position")
}

// TestLimitWallResidue_ReplacedGenerationIsNewEmission is the #6972 item-1
// wedge: on an unpinned runtime (a remote session can only read the backend —
// runtimePID is 0) an identical banner on a REPLACED runtime has nothing
// distinguishing it from the retired emission, and the residue rule used to
// take the lookalike as proof. The runtime generation binds the claim to the
// pane that sighted it: a just-launched pane must never prove residue.
func TestLimitWallResidue_ReplacedGenerationIsNewEmission(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	reReadAt := parked.Add(3 * time.Minute)
	defer freezeInstanceNow(reReadAt)()

	// An unpinned runtime (remote/adopted — neither side can resolve a
	// concrete pid) parks on the wall, then the clear retires the claim.
	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
	inst.ClearLimitReached()

	// Same generation, same pane: the unchanged expired emission is residue.
	require.True(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
		"the same runtime's unchanged expired emission is residue")

	// The runtime was replaced (remote rebind, respawn, handoff): with no PID
	// pin available the claim's runtime generation is the only proof — a
	// lookalike banner on the new pane is a different emission.
	inst.noteAgentRuntimeReplaced()
	require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
		"a just-launched pane must never prove residue: PID pin 0 cannot stand in for 'same runtime'")
	require.True(t, inst.SetLimitReachedAtEpoch(clockReset(reReadAt), clockResetEvidence, staleBanner, inst.stateEpoch),
		"the fresh runtime's identical banner is a new wall with its own parse")
	requireParkedReset(t, inst, clockReset(reReadAt))
}

// TestLimitWallResidue_ResetlessShadowIsResidue is #6972 item 3: a wall the
// detector could not parse a reset from parks with resetAt=0. After the
// interval retry's send lands and clears it, the unchanged emission is
// residue — a re-park there only re-arms limit_retry_interval, which is
// exactly the replay this issue retires.
func TestLimitWallResidue_ResetlessShadowIsResidue(t *testing.T) {
	defer freezeInstanceNow(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC))()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(time.Time{}, "", staleBanner, false)
	require.True(t, inst.LimitReached(), "the reset-less sighting parks as a live wall")
	_, ok := inst.LimitResetAt()
	require.False(t, ok, "a reset-less park carries no reset — the retry falls back to the interval")
	inst.ClearLimitReached()

	require.True(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, "", false),
		"a cleared reset-less wall's unchanged emission is residue — the row already moved past it")
	require.False(t, inst.SetLimitReachedAtEpoch(time.Time{}, "", staleBanner, inst.stateEpoch),
		"residue must not re-park: each re-park is one more interval replay (#6972)")
	require.False(t, inst.LimitReached())

	// A PARKED reset-less wall is still a live wall — only a retired
	// emission is residue.
	parked := &Instance{Program: tmux.ProgramClaude}
	parked.SetLimitReachedAtEvidence(time.Time{}, "", staleBanner, false)
	require.False(t, parked.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, "", false),
		"the live zero-reset claim is the wall itself, not residue")
}

// TestLimitPark_NewEmissionDropsPromptSent is the #6972 Codex wedge: an
// early retry lands its send and the unchanged banner re-parks the row at
// the recorded reset with the send tombstone. A banner printed BELOW the
// recorded one — same reset phrase, grown match count — is a NEW wall
// whose prompt never went out: keeping the claim's tombstone there would
// suppress the new wall's resume forever. The re-read must resolve a fresh
// claim with a fresh tombstone.
func TestLimitPark_NewEmissionDropsPromptSent(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(-time.Hour))()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)

	// The retry's send lands: the clear retires the claim tombstoned.
	inst.ClearLimitReachedSent()
	require.False(t, inst.LimitReached())

	// The unchanged banner re-parks the row at the recorded reset — and the
	// tombstone rides with the borrowed claim.
	require.True(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch))
	require.True(t, inst.LimitWallPromptSent(),
		"the re-park of the sent wall keeps its tombstone so the scheduler skips it")

	// A second emission below it — same phrase, count grown — is a different
	// wall: the tombstone must not follow.
	second := LimitBanner{Prefix: staleBanner.Prefix + "Claude usage limit reached.\nmore transcript\n", Count: 2}
	require.True(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, second, inst.stateEpoch),
		"the new emission still parks the row at the wall")
	require.False(t, inst.LimitWallPromptSent(),
		"a provably new emission re-using the reset phrase must not inherit the sent tombstone")
}

// TestLimitPark_TopOfCaptureShadowKeepsTombstone is the #6972 Codex
// anchorless arm: a tombstoned claim recorded at the capture's start has no
// prefix bytes to prove position — the anchorless fallback must still carry
// the send tombstone through a same-phrase re-park, or the scheduler replays
// the prompt at reset+grace.
func TestLimitPark_TopOfCaptureShadowKeepsTombstone(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(-time.Hour))()

	top := LimitBanner{Prefix: "", Count: 1}
	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, top, false)
	inst.ClearLimitReachedSent()

	require.True(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, top, inst.stateEpoch),
		"the anchorless same-phrase re-read re-parks the recorded claim")
	require.True(t, inst.LimitWallPromptSent(),
		"the tombstone survives an anchorless shadow re-park — its send already went out")
}

// TestLimitPark_ReplacedRuntimeDropsTombstone is the #6972 Codex runtime
// arm: a tombstoned parked row whose runtime was replaced (a relaunch that
// preserves LiveLimitReached) must not keep the dead runtime's tombstone on
// a lookalike banner — the replacement's wall was never answered.
func TestLimitPark_ReplacedRuntimeDropsTombstone(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(-time.Hour))()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
	inst.ClearLimitReachedSent()
	require.True(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch))
	require.True(t, inst.LimitWallPromptSent())

	inst.MarkAgentRuntimeReplacedForTest(true)
	require.True(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch),
		"the replacement's identical banner re-parks as its own emission")
	require.False(t, inst.LimitWallPromptSent(),
		"the replaced runtime's wall never received a prompt — no tombstone may carry")
}

// TestLimitWallResidue_ChurnedClaimIsNotRetired is the #6972 Codex churn arm:
// ordinary pane churn settles a parked row to Running WITHOUT delivering its
// pending prompt and WITHOUT clearing the claim fields — so the shadow it
// mirrors is a standing claim, not a retired one. Judging it residue would
// ignore the unchanged banner forever and strand the send; the sighting must
// re-park the row and keep it in the retry loop.
func TestLimitWallResidue_ChurnedClaimIsNotRetired(t *testing.T) {
	inst := &Instance{Program: tmux.ProgramClaude}
	require.True(t, inst.SetLimitReachedAtEpoch(time.Time{}, "", staleBanner, inst.stateEpoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveRunning).ChurnOnly()),
		"pane churn settles liveness without retiring the claim")

	require.True(t, inst.SetLimitReachedAtEpoch(time.Time{}, "", staleBanner, inst.stateEpoch),
		"the unchanged banner is still the standing wall — it re-parks, it is not residue")
	require.True(t, inst.LimitReached())
	require.False(t, inst.LimitWallPromptSent(),
		"no send landed for this wall — the row must stay in the retry loop")
}

// TestLimitWallResidue_FailedHandoffKeepsSentClaim is the #6972 Codex rollback
// arm: a failed SwapAgentProgram's revert leaves the outgoing runtime running,
// so its retired claims stay provable. Without the restore, the speculative
// shadow drop plus generation advance turned an expired, already-answered wall
// into a fresh park with no tombstone — the scheduler replayed its prompt.
func TestLimitWallResidue_FailedHandoffKeepsSentClaim(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(2 * time.Hour))()

	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
	inst.ClearLimitReachedSent()

	entry, err := inst.SwapAgentProgram(tmux.ProgramGemini, HandoffReasonUsageLimit, "sha", false)
	require.NoError(t, err)
	require.NoError(t, inst.RevertHandoff(entry))

	require.False(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch),
		"the reverted swap's runtime still shows its own retired wall — residue, not a fresh park")
	require.False(t, inst.LimitReached())
}

// TestLimitWallResidue_VerifiedReattachKeepsShadow is the #6972 Codex
// reattach arm: a recovery that verified the surviving pane's (pid, start)
// identity is NOT a replacement — the post hook's broker reset must not
// advance the runtime generation or drop the retired wall, or the same
// already-answered banner stops reading as residue and re-arms retries.
func TestLimitWallResidue_VerifiedReattachKeepsShadow(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(2 * time.Hour))()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch)
	inst.ClearLimitReachedSent()

	inst.markAgentRuntimeReplaced(false) // the verified verdict
	resetAgentBrokerCaptures(inst, false)

	require.False(t, inst.SetLimitReachedAtEpoch(parked, clockResetEvidence, staleBanner, inst.stateEpoch),
		"the reattached pane's own retired wall is still residue — provenance survived the hook")
	require.False(t, inst.LimitReached())
}

// TestLimitWallResidue_RemoteReplaceIsNewEmission is the #6972 Codex remote
// arm of item 1: a docker/ssh/hook session's in-place relaunch marks the
// runtime replaced without ever resolving a concrete PID — runtimePID stays
// 0, so only the generation advance distinguishes the new pane. The mark is
// what wires remote replacements into that invalidation.
func TestLimitWallResidue_RemoteReplaceIsNewEmission(t *testing.T) {
	sightedAt := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	parked := clockReset(sightedAt)
	defer freezeInstanceNow(parked.Add(3 * time.Minute))()

	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(parked, clockResetEvidence, staleBanner, false)
	inst.ClearLimitReached()

	// An in-place remote relaunch marks the runtime replaced — no PID pin
	// exists either side, so the generation is the only binding.
	inst.MarkAgentRuntimeReplacedForTest(true)
	require.False(t, inst.RecordedLimitWallExpired(staleBanner.Prefix, staleBanner.Count, clockResetEvidence, false),
		"a relaunched remote runtime's lookalike banner is a new emission, never residue")
}

// TestLimitWallShadow_DropsOnRuntimeReplace is the #6972 item-5 lifetime
// bound: the shadow claim survives ClearLimitReached so a same-emission
// re-read stays residue, but once the runtime it names is gone for good its
// pane can never show that emission again — the shadow is dead weight in
// instances.json and must drop. (The respawn fence is the one consumer that
// still reads it; ReparkLimitUnderResumeFence re-mints it afterward.)
func TestLimitWallShadow_DropsOnRuntimeReplace(t *testing.T) {
	inst := &Instance{Program: tmux.ProgramClaude}
	inst.SetLimitReachedAtEvidence(clockReset(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)),
		clockResetEvidence, staleBanner, false)
	inst.ClearLimitReached()
	require.NotNil(t, inst.ToInstanceData().LastLimitWall, "the shadow outlives the clear for residue")

	inst.noteAgentRuntimeReplaced()
	require.Nil(t, inst.ToInstanceData().LastLimitWall,
		"a replaced runtime's pane can never show the recorded emission again — drop the shadow (#6972)")
}

// TestLimitWallForClientsStripsPaneMatchingData is the #6972 item-5 payload
// boundary: the serialized record doubles as the session API row, but the
// daemon-only wall machinery — raw pane text in the banner anchors, the
// runtime pins, the retired shadow, the send tombstone — is matching data no
// client renders or decides from. ForClients withholds it while the visible
// badge facts survive.
func TestLimitWallForClientsStripsPaneMatchingData(t *testing.T) {
	inst := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	inst.SetLimitReachedAtEvidence(clockReset(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)),
		clockResetEvidence, staleBanner, false)

	raw := inst.ToInstanceData()
	require.NotEmpty(t, raw.LimitBannerPrefix, "the record carries the anchor tail for its own matching")
	require.NotNil(t, raw.LastLimitWall)

	projected := raw.ForClients()
	require.Empty(t, projected.LimitBannerPrefix, "raw pane text is daemon-only matching data")
	require.Zero(t, projected.LimitBannerCount)
	require.Zero(t, projected.LimitWallRuntimePID)
	require.Zero(t, projected.LimitWallRuntimeStartID)
	require.Zero(t, projected.LimitWallRuntimeGen)
	require.False(t, projected.LimitPromptSent)
	require.Zero(t, projected.AgentRuntimeGeneration)
	require.Nil(t, projected.LastLimitWall, "the retired shadow — and its raw anchor — stays daemon-side")

	// The client-visible wall facts are unaffected by the projection.
	require.Equal(t, raw.LimitResetAt, projected.LimitResetAt)
	require.Equal(t, raw.LimitResetEvidence, projected.LimitResetEvidence)
	require.Equal(t, raw.LimitAgent, projected.LimitAgent)
	require.Equal(t, raw.LimitAccount, projected.LimitAccount)

	// The anchor tail itself is bounded: a long capture never lands whole in
	// the record, and the cut lands on a rune boundary so a suffix compare
	// still matches after the JSON round-trip.
	var long string
	for len(long) < limitWallPrefixPersistMax+64 {
		long += "transcript line — a multibyte é char\n"
	}
	tail := limitWallPrefixTail(long)
	require.LessOrEqual(t, len(tail), limitWallPrefixPersistMax)
	require.True(t, utf8.ValidString(tail), "a mid-rune cut would JSON-decode to U+FFFD and never match again")
	require.True(t, strings.HasSuffix(long, tail))
}
