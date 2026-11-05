package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestSetLimitReached sets the LimitReached liveness + reset time and reports
// them through the accessors (#1146). The composed Status stays Ready (there is
// no legacy limit value), so render sites must key off LimitReached, not Status.
func TestSetLimitReached(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 0, 0, 0, time.UTC)
	i := &Instance{}
	i.SetLimitReached(reset)

	require.True(t, i.LimitReached())
	require.Equal(t, LiveLimitReached, i.GetLiveness())
	require.Equal(t, Ready, i.GetStatus(), "LimitReached composes to Ready for legacy readers")

	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset))
}

func TestSetLimitReachedAttributesWallToAccountThatProducedIt(t *testing.T) {
	i := &Instance{Program: tmux.ProgramClaude, Account: "work", accountAutoSelected: true}
	i.SetLimitReached(time.Time{})

	account, ok := i.LimitAccount()
	require.True(t, ok)
	require.Equal(t, "work", account)
	agent, identityAccount, ok := i.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", identityAccount)

	data := i.ToInstanceData()
	require.Equal(t, tmux.ProgramClaude, data.LimitAgent)
	require.Equal(t, "work", data.LimitAccount)
	i.Account = "personal"
	account, _ = i.LimitAccount()
	require.Equal(t, "work", account)
}

func TestLimitAgentFromDataRejectsArbitraryText(t *testing.T) {
	data := InstanceData{
		ID: "limited", Program: tmux.ProgramClaude, Account: "work",
		Liveness: LiveLimitReached, LimitAgent: "client-chosen-text", LimitAccount: "work",
	}
	account, observations := AccountLimitEvidenceFromData(data)

	agent := limitAgentFromData(data, account, observations)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", account)
	require.NotEqual(t, "client-chosen-text", agent)
}

func TestAccountLimitObservationSurvivesClearAndStorage(t *testing.T) {
	reset := time.Date(2026, 8, 10, 17, 0, 0, 0, time.UTC)
	i := &Instance{
		Program: tmux.ProgramClaude, Account: "work", accountAutoSelected: true,
	}
	i.SetLimitReached(reset)
	i.ClearLimitReached()

	observations := i.AccountLimitObservations()
	require.Len(t, observations, 1,
		"clearing current liveness must not make an exhausted identity eligible")
	require.Equal(t, tmux.ProgramClaude, observations[0].Agent)
	require.Equal(t, "work", observations[0].Account)
	require.True(t, observations[0].ResetAt.Equal(reset))
	require.False(t, observations[0].ObservedAt.IsZero(),
		"the wall's sighting instant is part of the evidence (#6776 1d)")

	raw, err := json.Marshal(i.ToInstanceData().ForStorage())
	require.NoError(t, err)
	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, observations, stored.AccountLimitObservations,
		"account limit evidence must survive the daemon restart boundary")
}

func TestAccountLimitObservationPreservesSafestReset(t *testing.T) {
	earlier := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	later := earlier.Add(7 * 24 * time.Hour)

	t.Run("later reset is not shortened", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		i.SetLimitReached(later)
		i.ClearLimitReached()
		i.SetLimitReached(earlier)

		require.Equal(t, later, i.AccountLimitObservations()[0].ResetAt,
			"a short-window wall must not erase a still-active longer quota wall")
	})

	t.Run("distinct reset claims keep distinct rows", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		i.SetLimitReached(time.Time{})
		i.ClearLimitReached()
		i.SetLimitReached(later)

		observations := i.AccountLimitObservations()
		require.Len(t, observations, 2,
			"a known reset cannot merge into an unknown-reset wall without lowering what a proof must beat (#6802 review)")
		require.True(t, observations[0].ResetAt.IsZero(),
			"a later timestamp cannot make an indefinite limit observation safe to reuse")
		require.True(t, observations[1].ResetAt.Equal(later),
			"the known reset claim survives beside the unknown one — the strictest still excludes")
	})
}

// TestSetLimitReachedAtEpoch_UnchangedEvidenceKeepsReset is the session-level
// half of #6765: the poll re-parses an unchanged banner against a later clock
// every tick, and the time-only and relative forms resolve to a LATER instant
// each time. The same reset phrase is the same observation — the stored
// instant is the first parse's, the one nearest the wall the provider printed.
func TestSetLimitReachedAtEpoch_UnchangedEvidenceKeepsReset(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex}
	first := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	phrase := "try again in 4 days 2 hours 46 minutes."

	require.True(t, i.SetLimitReachedAtEpoch(first, phrase, i.stateEpoch))
	require.True(t, i.LimitReached())
	reset, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, reset.Equal(first))

	// A re-read of the same banner re-parses to a later instant but must not
	// move the stored reset — the phrase already produced this row's reset.
	require.True(t, i.SetLimitReachedAtEpoch(first.Add(2*time.Minute), phrase, i.stateEpoch))
	reset, _ = i.LimitResetAt()
	require.True(t, reset.Equal(first), "an unchanged banner's re-read must not move the stored reset")

	// The phrase rides the durable row beside the reset it produced, so a
	// daemon restart keeps the pairing (#6765).
	require.Equal(t, phrase, i.ToInstanceData().LimitResetEvidence)

	// A different phrase is NEW evidence and applies normally.
	require.True(t, i.SetLimitReachedAtEpoch(first.Add(time.Hour), "try again at 9:00 AM", i.stateEpoch))
	reset, _ = i.LimitResetAt()
	require.True(t, reset.Equal(first.Add(time.Hour)))

	// A reset that arrives without a phrase always applies — and clears the
	// slot so it cannot keep describing this reset.
	require.True(t, i.SetLimitReachedAtEpoch(first.Add(2*time.Hour), "", i.stateEpoch))
	reset, _ = i.LimitResetAt()
	require.True(t, reset.Equal(first.Add(2*time.Hour)))
	require.Empty(t, i.ToInstanceData().LimitResetEvidence)
}

// TestSetLimitReachedAtEpoch_DegradedRereadKeepsStoredReset is the session-level
// half of the parse-failure re-read fix: the poll captures only the pane's
// visible viewport (no scrollback), so a parked session whose banner is still
// detected but whose reset clause has scrolled or wrapped below the viewport
// fold re-reads as hit=true with resetAt=zero and evidence="". That degraded
// re-read must be a no-op for stored facts — the wall is exactly as parseable
// as at park time, so downgrading it from known-reset to unknown wipes the
// stored reset, wipes the stored evidence phrase, and appends a second
// never-expiring zero-reset observation row that durably excludes the account
// from automatic replacement (#6802 review).
func TestSetLimitReachedAtEpoch_DegradedRereadKeepsStoredReset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
		phrase  string
	}{
		{"codex", tmux.ProgramCodex, "try again at Sep 20th, 2026 9:16 AM"},
		{"claude", tmux.ProgramClaude, "reset at 2pm (America/New_York)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instance{Program: tc.program, Account: "work", accountAutoSelected: true}
			reset0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

			// Park exactly as the poll does: a parsed reset time and its phrase.
			require.True(t, i.SetLimitReachedAtEpoch(reset0, tc.phrase, i.stateEpoch))
			require.True(t, i.LimitReached())
			storedReset, ok := i.LimitResetAt()
			require.True(t, ok)
			require.True(t, storedReset.Equal(reset0))
			require.Equal(t, tc.phrase, i.LimitResetEvidence())
			require.Len(t, i.AccountLimitObservations(), 1)
			// The first park flagged the observation dirty; simulate the
			// persist that landed it on disk so the re-read's dirty flag is
			// observable in isolation (matching the poll's persist sequence).
			i.MarkAccountLimitObservationsPersisted(i.AccountLimitObservations())
			require.False(t, i.AccountLimitObservationsUnpersisted())

			// Degraded re-read: hit=true but the reset clause is no longer
			// parseable from the visible pane — the exact shape the poll
			// sends (zero reset, empty evidence).
			applied := i.SetLimitReachedAtEpoch(time.Time{}, "", i.stateEpoch)
			require.True(t, applied, "the degraded re-read still parks (detection stands)")

			// The stored reset and evidence phrase are preserved — the first
			// parse is nearest the real window (#6765).
			gotReset, ok := i.LimitResetAt()
			require.True(t, ok, "the stored known reset must not be wiped by a parse-failure re-read")
			require.True(t, gotReset.Equal(reset0), "the stored reset must not move")
			require.Equal(t, tc.phrase, i.LimitResetEvidence(),
				"the stored evidence phrase must not be wiped by a parse-failure re-read")

			// No second observation row is appended — the degraded re-read
			// observed no new fact, so the durable ledger is unchanged.
			observations := i.AccountLimitObservations()
			require.Len(t, observations, 1,
				"a parse-failure re-read must not append a second zero-reset observation row")
			require.True(t, observations[0].ResetAt.Equal(reset0),
				"the stored observation's reset must not be downgraded to zero")
			require.False(t, i.AccountLimitObservationsUnpersisted(),
				"the degraded re-read must not flag the observation dirty")
		})
	}
}

// TestSetLimitReachedAtEpoch_DegradedRereadDoesNotMaskNewWall verifies the fix
// does not suppress a genuinely new wall event: after the parked state is
// cleared (the account recovered), a fresh limit hit — even one with no
// parseable reset — must still apply normally.
func TestSetLimitReachedAtEpoch_DegradedRereadDoesNotMaskNewWall(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex, Account: "work", accountAutoSelected: true}
	reset0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

	require.True(t, i.SetLimitReachedAtEpoch(reset0, "try again at 9:00 AM", i.stateEpoch))
	require.Len(t, i.AccountLimitObservations(), 1)

	// The account recovers and hits the wall again with no parseable reset.
	i.ClearLimitReached()
	require.True(t, i.SetLimitReachedAtEpoch(time.Time{}, "", i.stateEpoch),
		"a fresh wall hit with no parseable reset must still apply when not parked")
	require.True(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a zero reset must report as unknown")
	require.Empty(t, i.LimitResetEvidence())
	require.Len(t, i.AccountLimitObservations(), 2,
		"a wall sighted after recovery is a new fact — a second row lands")
}

// TestSetLimitReachedAtEpoch_NonZeroResetWithEmptyEvidenceStillApplies pins the
// #6788 shape: a reset that arrives without a phrase always applies. The fix
// only short-circuits the zero-reset+empty-evidence parse-failure shape, so a
// non-zero reset paired with empty evidence — which no production caller of
// SetLimitReachedAtEpoch can produce, but the #6788 design pinned — must still
// fall through and apply.
func TestSetLimitReachedAtEpoch_NonZeroResetWithEmptyEvidenceStillApplies(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex, Account: "work", accountAutoSelected: true}
	first := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

	require.True(t, i.SetLimitReachedAtEpoch(first, "try again at 9:00 AM", i.stateEpoch))

	// A non-zero reset with empty evidence is NOT the parse-failure shape and
	// must still apply, clearing the evidence slot.
	later := first.Add(2 * time.Hour)
	require.True(t, i.SetLimitReachedAtEpoch(later, "", i.stateEpoch))
	gotReset, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, gotReset.Equal(later))
	require.Empty(t, i.LimitResetEvidence())
}

// TestSetLimitReached_NoResetTime: a banner with no parseable reset time still
// blocks the session, but LimitResetAt reports no known time.
func TestSetLimitReached_NoResetTime(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Time{})
	require.True(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a zero reset time must report as unknown")
}

// TestSetLimitReachedAtEvidence stores the reset phrase alongside its reset so
// the daemon's first poll recognizes a re-read of an unchanged banner as the
// same observation and keeps the first parse's reset (#6765). The send-path park
// (limitParkError → LimitReachedError.Evidence → SetLimitReachedAtEvidence) uses
// this to record the phrase that produced its reset; without it the evidence slot
// is empty and the poll re-parses against a later clock, moving the stored reset.
func TestSetLimitReachedAtEvidence(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex}
	reset := time.Date(2026, 11, 4, 6, 34, 0, 0, time.UTC)
	phrase := "try again at 6:34 AM"

	i.SetLimitReachedAtEvidence(reset, phrase)
	require.True(t, i.LimitReached())
	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset))
	require.Equal(t, phrase, i.LimitResetEvidence(), "the evidence must be stored alongside its reset")
	require.Equal(t, phrase, i.ToInstanceData().LimitResetEvidence, "the evidence must serialize onto the durable row")

	// SetLimitReached (no evidence) clears the slot — callers that resolve a
	// reset without a phrase must not leave a stale one describing it.
	i.SetLimitReached(reset.Add(time.Hour))
	require.Empty(t, i.LimitResetEvidence(), "SetLimitReached must clear the evidence slot")
}

// TestSetLimitReached_SkipsTransient: a row mid create/kill teardown must not be
// clobbered into LimitReached (mirrors SetStatusIfNotDeleting).
func TestSetLimitReached_SkipsTransient(t *testing.T) {
	for _, s := range []Status{Loading, Deleting} {
		i := &Instance{}
		i.SetStatusForTest(s)
		i.SetLimitReached(time.Now())
		require.False(t, i.LimitReached(), "%v must not be overwritten by SetLimitReached", s)
		require.Equal(t, s, i.GetStatus())
	}
}

// TestSetLimitReached_SkipsRestore: a usage-limit observation must not destroy
// the OpRestoring fence or its LiveLost liveness while a restore is in flight.
func TestSetLimitReached_SkipsRestore(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "restoring", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.NoError(t, i.Transition(BeginArchive()))
	require.NoError(t, i.Transition(CommitArchive()))
	require.NoError(t, i.Transition(BeginRestore()))
	require.Equal(t, LiveLost, i.GetLiveness())
	require.Equal(t, OpRestoring, i.GetInFlightOp())

	i.SetLimitReached(time.Now().Add(time.Hour))

	require.Equal(t, OpRestoring, i.GetInFlightOp(), "SetLimitReached must preserve an active restore fence")
	require.Equal(t, LiveLost, i.GetLiveness(), "SetLimitReached must preserve restore liveness")
	require.False(t, i.LimitReached(), "an in-flight restore must not be marked limit-reached")
}

// TestClearLimitReached moves a limit-blocked instance back to Running and drops
// the reset time; it is a no-op on a non-limit instance.
func TestClearLimitReached(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.ClearLimitReached()
	require.False(t, i.LimitReached())
	require.Equal(t, LiveRunning, i.GetLiveness())
	_, ok := i.LimitResetAt()
	require.False(t, ok)
	_, ok = i.LimitAccount()
	require.False(t, ok)

	// No-op on a Ready instance.
	r := &Instance{}
	r.SetStatusForTest(Ready)
	r.ClearLimitReached()
	require.Equal(t, Ready, r.GetStatus())
}

// TestLimitResetAt_ClearedWhenNotLimit: once the liveness moves off LimitReached
// the reset time never surfaces, even if the field lingers in memory.
func TestLimitResetAt_ClearedWhenNotLimit(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.SetStatusForTest(Ready) // shim moves liveness to LiveReady, limitResetAt lingers
	require.False(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a lingering reset time must not surface once off LimitReached")
}

// TestLimitReached_PersistRoundTrip: a limit-blocked instance serializes its
// liveness + reset time, and the InstanceData survives the JSON round-trip the
// daemon writes to disk / carries in the snapshot — so the badge survives a
// restart (#1146).
func TestLimitReached_PersistRoundTrip(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 30, 0, 0, time.UTC)
	i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetLimitReached(reset)
	i.Prompt = "run the nightly report"

	data := i.ToInstanceData()
	require.Equal(t, LiveLimitReached, data.Liveness)
	require.True(t, data.LimitResetAt.Equal(reset))
	require.Equal(t, "run the nightly report", data.Prompt)

	// JSON round-trip (the on-disk + snapshot format).
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, LiveLimitReached, back.Liveness)
	require.True(t, back.LimitResetAt.Equal(reset))
	require.Equal(t, "run the nightly report", back.Prompt)

	// FromInstanceData maps the reset time onto the rebuilt instance's field.
	require.Equal(t, reset, back.LimitResetAt)
}

// TestToInstanceData_NoLimitResetForNormalSession: a Ready session never persists
// a reset time, so omitzero drops the field for every normal row.
func TestToInstanceData_NoLimitResetForNormalSession(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "normal", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStatusForTest(Ready)
	data := i.ToInstanceData()
	require.True(t, data.LimitResetAt.IsZero(), "a non-limit session must not carry a reset time")
}

// TestLimitResetAt_OmittedFromJSONWhenZero pins the wire contract documented in
// both the field comment and web/src/types.ts: the "limit_reset_at" key MUST be
// absent from marshaled JSON whenever LimitResetAt is the zero time.Time. Go's
// encoding/json treats a struct as non-empty for omitempty, so the field must
// use omitzero (the TabExitData.At precedent, #4506); a regression to omitempty
// would re-leak "0001-01-01T00:00:00Z" onto every session row, snapshot, and
// /v1/events payload, and the web would render a fabricated "resets Jan 1 12am"
// badge for a limit row with no parseable reset time.
func TestLimitResetAt_OmittedFromJSONWhenZero(t *testing.T) {
	t.Run("non-limit session omits the key", func(t *testing.T) {
		i, err := NewInstance(InstanceOptions{Title: "normal", Path: t.TempDir(), Program: "claude"})
		require.NoError(t, err)
		i.SetStatusForTest(Ready)
		raw, err := json.Marshal(i.ToInstanceData())
		require.NoError(t, err)
		require.NotContains(t, string(raw), "limit_reset_at",
			"a non-limit session must not emit limit_reset_at (omitzero on a zero time.Time)")
		require.NotContains(t, string(raw), "0001-01-01",
			"the invented zero timestamp must never reach the wire")
	})
	t.Run("limit session with unknown reset time omits the key", func(t *testing.T) {
		i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
		require.NoError(t, err)
		i.SetLimitReached(time.Time{}) // banner matched, reset time did not parse
		require.True(t, i.LimitReached())
		raw, err := json.Marshal(i.ToInstanceData())
		require.NoError(t, err)
		require.NotContains(t, string(raw), "limit_reset_at",
			"a limit row with an unknown reset window must not emit limit_reset_at")
		require.NotContains(t, string(raw), "0001-01-01",
			"the invented zero timestamp must never reach the wire")
	})
	t.Run("ForStorage projection omits the key for a normal session", func(t *testing.T) {
		i, err := NewInstance(InstanceOptions{Title: "normal", Path: t.TempDir(), Program: "claude"})
		require.NoError(t, err)
		i.SetStatusForTest(Ready)
		raw, err := json.Marshal(i.ToInstanceData().ForStorage())
		require.NoError(t, err)
		require.NotContains(t, string(raw), "limit_reset_at",
			"instances.json must not carry limit_reset_at for a non-limit row")
	})
	t.Run("limit session with a reset time still emits the key", func(t *testing.T) {
		reset := time.Date(2026, 7, 5, 14, 30, 0, 0, time.UTC)
		i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
		require.NoError(t, err)
		i.SetLimitReached(reset)
		raw, err := json.Marshal(i.ToInstanceData())
		require.NoError(t, err)
		require.Contains(t, string(raw), `"limit_reset_at":"2026-07-05T14:30:00Z"`,
			"a limit row with a known reset time must still emit limit_reset_at")
	})
}
