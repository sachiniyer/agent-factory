package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// requireAccountLimitObservation asserts an observation exists for the
// identity and reset — and that it carries its sighting instant, which every
// observation this binary records stamps. The fixture cannot predict that
// instant, so it is asserted present rather than equal.
func requireAccountLimitObservation(t *testing.T, observations []AccountLimitObservationData, agent, account string, resetAt time.Time) {
	t.Helper()
	for _, observation := range observations {
		if observation.Agent == agent && observation.Account == account && observation.ResetAt.Equal(resetAt) {
			require.False(t, observation.ObservedAt.IsZero(),
				"the wall's sighting instant is its own event time (#6776 1d)")
			return
		}
	}
	t.Fatalf("no %s/%s observation with reset %v in %+v", agent, account, resetAt, observations)
}

// AccountLimitObservationRetracted is the ordering rule that makes refutable
// limit evidence safe (#6776 1d): a proof defeats a wall only when it is the
// NEWER fact by when each happened — strictly after every instant the wall
// claims. Strictness is the load-bearing property: equal times cannot prove
// the account served AFTER the wall it claims to retire.
func TestAccountLimitObservationRetracted(t *testing.T) {
	observedAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	resetAt := observedAt.Add(5 * time.Hour)
	observation := AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "work",
		ObservedAt: observedAt, ResetAt: resetAt,
	}
	retract := func(agent, account string, provenAt time.Time) AccountLimitRetractionData {
		return AccountLimitRetractionData{Agent: agent, Account: account, ProvenAt: provenAt}
	}

	t.Run("proof after every claimed instant defeats the wall", func(t *testing.T) {
		require.True(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", resetAt.Add(time.Second))))
	})

	t.Run("identity must match on both axes", func(t *testing.T) {
		proven := resetAt.Add(time.Second)
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramCodex, "work", proven)),
			"an equal account label under another provider proves nothing")
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "personal", proven)))
	})

	t.Run("an untimed proof defeats nothing", func(t *testing.T) {
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", time.Time{})))
	})

	t.Run("a wall sighted at or after the proof is the newer fact", func(t *testing.T) {
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", observedAt)),
			"a proof simultaneous with the sighting did not happen after it")
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", observedAt.Add(-time.Second))),
			"a fresh wall outranks an older proof regardless of write order")
	})

	t.Run("a proof at or before the claimed reset proves nothing", func(t *testing.T) {
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", resetAt)),
			"the wall's reset must already have passed when the proof lands (#4404)")
		require.False(t, AccountLimitObservationRetracted(observation,
			retract(tmux.ProgramClaude, "work", resetAt.Add(-time.Minute))))
	})

	t.Run("a missing sighting instant orders against nothing", func(t *testing.T) {
		legacy := observation
		legacy.ObservedAt = time.Time{}
		require.False(t, AccountLimitObservationRetracted(legacy,
			retract(tmux.ProgramClaude, "work", resetAt.Add(time.Second))),
			"a row that lost its timestamp could have been sighted after the proof — it cannot be ordered (#6802 review)")
		require.False(t, AccountLimitObservationRetracted(legacy,
			retract(tmux.ProgramClaude, "work", resetAt.Add(-time.Second))))
	})

	t.Run("a missing reset orders only through the sighting", func(t *testing.T) {
		noReset := observation
		noReset.ResetAt = time.Time{}
		require.True(t, AccountLimitObservationRetracted(noReset,
			retract(tmux.ProgramClaude, "work", observedAt.Add(time.Second))))
		require.False(t, AccountLimitObservationRetracted(noReset,
			retract(tmux.ProgramClaude, "work", observedAt.Add(-time.Second))))
	})

	t.Run("a wall with neither instant is never defeated", func(t *testing.T) {
		untimed := observation
		untimed.ObservedAt = time.Time{}
		untimed.ResetAt = time.Time{}
		require.False(t, AccountLimitObservationRetracted(untimed,
			retract(tmux.ProgramClaude, "work", observedAt.Add(time.Hour))),
			"an untimed wall's true sighting is unknowable — it cannot lose to any retraction (#6802 review)")
		require.False(t, AccountLimitObservationRetracted(untimed,
			retract(tmux.ProgramClaude, "work", time.Time{})))
	})
}

// The observation's event time is its last SIGHTING, not its first: a wall
// re-observed later must force any retraction to postdate the freshest
// evidence, or an old proof could retire a wall that is still being seen.
func TestAccountLimitObservationStampsEachSighting(t *testing.T) {
	first := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Hour)
	clock := first
	previous := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = previous })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(first.Add(5 * time.Hour))
	observations := i.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(first),
		"the wall's event time is the instant it was observed, not the provider's reset claim")

	clock = second
	i.ClearLimitReached()
	i.SetLimitReached(first.Add(5 * time.Hour))
	observations = i.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(second),
		"re-sighting the same wall advances its event time so an older proof cannot defeat it")
}

// The durable-observation dirty flag is what carries a re-sighting to disk
// when nothing else on the poll's change axes moved: the stored record must
// carry the same fact the reconcile decides against, and a mark only clears
// when the write carried the list the instance still holds (#6802 review).
func TestAccountLimitObservationUnpersistedTracksTheWrite(t *testing.T) {
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	require.False(t, i.AccountLimitObservationsUnpersisted(),
		"observations loaded from a row are already durable")

	i.SetLimitReached(time.Time{})
	require.True(t, i.AccountLimitObservationsUnpersisted(),
		"a fresh sighting is memory-only until a write carries it")

	i.MarkAccountLimitObservationsPersisted(nil)
	require.True(t, i.AccountLimitObservationsUnpersisted(),
		"a write that did not carry the observation list clears nothing")

	i.MarkAccountLimitObservationsPersisted(i.AccountLimitObservations())
	require.False(t, i.AccountLimitObservationsUnpersisted(),
		"a write carrying the current list clears the flag")

	i.SetLimitReached(time.Time{})
	require.True(t, i.AccountLimitObservationsUnpersisted(),
		"a re-sighting after the write is pending again")
}

// A known-reset wall and a no-reset wall are different claims: merging them
// into one row would either let the indefinite claim expire on the other's
// clock or lower the instant a retraction must postdate. The recorder keeps
// one row per reset-claim kind and folds only same-kind re-sightings (#6802
// review).
func TestAccountLimitObservationKeepsResetClaimsSeparate(t *testing.T) {
	later := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(later)
	i.ClearLimitReached()
	i.SetLimitReached(time.Time{})
	observations := i.AccountLimitObservations()
	require.Len(t, observations, 2,
		"an unknown-reset wall cannot merge into the known one — the strictest claim still excludes")
	require.True(t, observations[0].ResetAt.Equal(later))
	require.True(t, observations[1].ResetAt.IsZero())

	// And a same-kind re-sighting still folds: the list stays bounded at one
	// row per claim kind.
	i.ClearLimitReached()
	i.SetLimitReached(time.Time{})
	require.Len(t, i.AccountLimitObservations(), 2,
		"re-sighting the no-reset wall advances its row rather than growing the list")
}
