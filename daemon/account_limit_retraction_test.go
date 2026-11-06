package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/rateindex"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// Refutable account-limit evidence (#6776 slice 1d, #4404). A wall observation
// is durable, but it is only a claim about the past: positive proof that the
// account served traffic later — a below-wall usage reading in its own rollout
// store — ends the observation without deleting it. The newer fact wins by
// when it happened, never by which write landed on disk last, and a
// currently-walled account is never made healthy by a retraction.

// requireAccountLimitObservation asserts an observation exists for the
// identity and reset — and that it carries its sighting instant, which every
// observation this binary records stamps. The fixture cannot predict that
// instant, so it is asserted present rather than equal.
func requireAccountLimitObservation(t *testing.T, observations []session.AccountLimitObservationData, agent, account string, resetAt time.Time) {
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

func stubAccountLimitEvidence(t *testing.T, evidence accountLimitEvidence, err error) {
	t.Helper()
	previous := loadAccountLimitEvidenceForSwap
	loadAccountLimitEvidenceForSwap = func() (accountLimitEvidence, error) {
		return evidence, err
	}
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = previous })
	bypassAccountLimitProofGate(t)
}

// bypassAccountLimitProofGate opens the reconcile's cheap pre-scan gate: a
// test stubbing the durable evidence bypasses the very sources the gate
// inspects, so the gate is opened explicitly to exercise the pass itself.
// The gate's own cases are covered separately below.
func bypassAccountLimitProofGate(t *testing.T) {
	t.Helper()
	previous := accountLimitProofWorthScanning
	accountLimitProofWorthScanning = func(*Manager) bool { return true }
	t.Cleanup(func() { accountLimitProofWorthScanning = previous })
}

// resetAccountLimitRetractionQuarantine empties the process-wide quarantine
// of once-future-dated retractions so one test's dropped entry cannot make a
// later test's identical entry inert.
func resetAccountLimitRetractionQuarantine(t *testing.T) {
	t.Helper()
	quarantinedAccountLimitRetractions = sync.Map{}
	quarantinedAccountLimitRetractionsCount.Store(0)
}

// requireAccountLimitRetraction asserts a retraction exists for the identity
// at the expected INSTANT. The ledger round-trips through JSON, which strips
// monotonic clock and zone — only instant equality is meaningful, so compare
// with Time.Equal, never require.Equal on the struct.
func requireAccountLimitRetraction(t *testing.T, retractions []session.AccountLimitRetractionData, agent, account string, provenAt time.Time) {
	t.Helper()
	for _, retraction := range retractions {
		if retraction.Agent == agent && retraction.Account == account && retraction.ProvenAt.Equal(provenAt) {
			return
		}
	}
	t.Fatalf("no %s/%s retraction proven at %v in %+v", agent, account, provenAt, retractions)
}

func ledgerBackedEvidence(observations ...session.AccountLimitObservationData) accountLimitEvidenceLoader {
	return func() (accountLimitEvidence, error) {
		evidence, err := loadAccountLimitLedger()
		if err != nil {
			return accountLimitEvidence{}, err
		}
		evidence.Observations = append(observations, evidence.Observations...)
		return evidence, nil
	}
}

func TestAccountLimitObservationExcludes(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	wall := session.AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "work",
		ObservedAt: now.Add(-2 * time.Hour), ResetAt: now.Add(time.Hour),
	}
	retract := func(provenAt time.Time) session.AccountLimitRetractionData {
		return session.AccountLimitRetractionData{Agent: tmux.ProgramClaude, Account: "work", ProvenAt: provenAt}
	}

	t.Run("an unexpired wall with no proof excludes", func(t *testing.T) {
		require.True(t, accountLimitObservationExcludes(wall, tmux.ProgramClaude, nil, now))
	})

	t.Run("an expired wall excludes nothing", func(t *testing.T) {
		expired := wall
		expired.ResetAt = now.Add(-limitResumeGrace - time.Second)
		require.False(t, accountLimitObservationExcludes(expired, tmux.ProgramClaude, nil, now))
	})

	t.Run("a wall with no reset claim excludes until proven", func(t *testing.T) {
		noReset := wall
		noReset.ResetAt = time.Time{}
		require.True(t, accountLimitObservationExcludes(noReset, tmux.ProgramClaude, nil, now))
		require.False(t, accountLimitObservationExcludes(noReset, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{retract(now)}, now))
	})

	t.Run("positive proof after the wall's reset frees the account", func(t *testing.T) {
		require.False(t, accountLimitObservationExcludes(wall, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{retract(wall.ResetAt.Add(time.Second))}, now))
	})

	t.Run("a proof inside the wall's window retracts nothing", func(t *testing.T) {
		require.True(t, accountLimitObservationExcludes(wall, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{retract(wall.ResetAt.Add(-time.Minute))}, now),
			"a reading before the claimed reset is not proof the wall ended")
	})

	t.Run("a wall sighted after the proof is the newer fact", func(t *testing.T) {
		fresh := wall
		fresh.ObservedAt = now.Add(-time.Minute)
		fresh.ResetAt = time.Time{}
		require.True(t, accountLimitObservationExcludes(fresh, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{retract(now.Add(-2 * time.Minute))}, now),
			"event time decides — never the order the facts were written")
	})

	t.Run("an untimed retraction defeats nothing", func(t *testing.T) {
		require.True(t, accountLimitObservationExcludes(wall, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{retract(time.Time{})}, now))
	})

	t.Run("evidence for another identity does not interfere", func(t *testing.T) {
		require.False(t, accountLimitObservationExcludes(wall, tmux.ProgramCodex, nil, now),
			"the agent is part of the identity")
		otherAccount := wall
		otherAccount.Account = ""
		require.False(t, accountLimitObservationExcludes(otherAccount, tmux.ProgramClaude, nil, now))
		require.True(t, accountLimitObservationExcludes(wall, tmux.ProgramClaude,
			[]session.AccountLimitRetractionData{{
				Agent: tmux.ProgramClaude, Account: "personal", ProvenAt: now,
			}}, now))
	})
}

// mergeAccountLimitObservations must keep the freshest sighting regardless of
// which copy arrives first: the merge, not the caller's ordering, owns event
// time.
func TestMergeAccountLimitObservationsKeepsFreshestSighting(t *testing.T) {
	earlier := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	later := earlier.Add(2 * time.Hour)
	stale := session.AccountLimitObservationData{Agent: "claude", Account: "work", ObservedAt: earlier}
	fresh := stale
	fresh.ObservedAt = later

	for _, order := range [][2]session.AccountLimitObservationData{{stale, fresh}, {fresh, stale}} {
		merged, err := mergeAccountLimitObservations(
			[]session.AccountLimitObservationData{order[0]},
			[]session.AccountLimitObservationData{order[1]})
		require.NoError(t, err)
		require.Len(t, merged, 1)
		require.True(t, merged[0].ObservedAt.Equal(later),
			"the merged wall's event time is its last sighting")
	}
}

// A sighting-less copy merged with a timed one must NOT inherit the other's
// instant: the untimed copy could be a wall sighted after the proof, and
// stamping it would invent an event time the fact never had — one an older
// retraction could then order itself against (#6802 review). Either copy
// arriving untimed keeps the merged fact untimed.
func TestMergeAccountLimitObservationsUntimedCopyKeepsTheFactUntimed(t *testing.T) {
	timed := session.AccountLimitObservationData{
		Agent: "claude", Account: "work",
		ObservedAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
	}
	untimed := session.AccountLimitObservationData{Agent: "claude", Account: "work"}

	for _, order := range [][2]session.AccountLimitObservationData{{timed, untimed}, {untimed, timed}} {
		merged, err := mergeAccountLimitObservations(
			[]session.AccountLimitObservationData{order[0]},
			[]session.AccountLimitObservationData{order[1]})
		require.NoError(t, err)
		require.Len(t, merged, 1)
		require.True(t, merged[0].ObservedAt.IsZero(),
			"an untimed copy proves the sighting time is unknown — inheriting one would let a stale retraction order against it")
	}
}

// A known-reset wall and an unknown-reset wall for the same identity are
// different claims about the account: the first expires and bounds what a
// proof must beat, the second ends only on proof. Folding them into one row
// loses whichever half the merge drops — an unknown-dominated merge would let
// a proof between the sightings and the known reset free an account the
// separate facts still exclude (#6802 review).
func TestMergeAccountLimitObservationsKeepsResetKindsSeparate(t *testing.T) {
	earlier := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	reset := earlier.Add(48 * time.Hour)
	unknown := session.AccountLimitObservationData{Agent: "claude", Account: "work", ObservedAt: earlier}
	known := session.AccountLimitObservationData{Agent: "claude", Account: "work", ObservedAt: earlier.Add(time.Hour), ResetAt: reset}

	for _, order := range [][2]session.AccountLimitObservationData{{unknown, known}, {known, unknown}} {
		merged, err := mergeAccountLimitObservations(
			[]session.AccountLimitObservationData{order[0]},
			[]session.AccountLimitObservationData{order[1]})
		require.NoError(t, err)
		require.Len(t, merged, 2,
			"distinct reset claims keep distinct rows — the strictest still excludes")
		require.True(t, merged[0].ResetAt.IsZero(), "the unknown-reset claim keeps its row")
		require.True(t, merged[1].ResetAt.Equal(reset), "the known reset claim keeps its row")
	}
}

// The scenario the merge must not open: two sessions' walls for one account —
// one with no reset claim, one with a future reset — transfer into the
// ledger. A proof after both sightings but before the known reset must not
// free the account the unmerged evidence kept excluded.
func TestLimitedAccountsForSwapKeepsTheKnownResetThroughAMixedMerge(t *testing.T) {
	now := nowFunc()
	reset := now.Add(48 * time.Hour)
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "work", ObservedAt: now.Add(-time.Hour)},
			{Agent: "claude", Account: "work", ObservedAt: now.Add(-30 * time.Minute), ResetAt: reset},
		},
		Retractions: []session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: now.Add(-time.Minute)},
		},
	}, nil)

	limited, err := (&Manager{}).limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"a proof before the known reset defeats the unknown claim but not the reset-bounded one")

	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "work", ObservedAt: now.Add(-time.Hour)},
			{Agent: "claude", Account: "work", ObservedAt: now.Add(-30 * time.Minute), ResetAt: reset},
		},
		Retractions: []session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: reset.Add(time.Second)},
		},
	}, nil)

	limited, err = (&Manager{}).limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotContains(t, limited, "work",
		"a proof past the known reset defeats both claims")
}

func TestMergeAccountLimitRetractionsKeepsNewestProof(t *testing.T) {
	earlier := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	merged, err := mergeAccountLimitRetractions(
		[]session.AccountLimitRetractionData{{Agent: "claude", Account: "work", ProvenAt: earlier}},
		[]session.AccountLimitRetractionData{{Agent: "claude", Account: "work", ProvenAt: later}})
	require.NoError(t, err)
	require.Equal(t, []session.AccountLimitRetractionData{
		{Agent: "claude", Account: "work", ProvenAt: later},
	}, merged, "an older proof adds nothing the newer one does not already order")

	_, err = mergeAccountLimitRetractions(nil,
		[]session.AccountLimitRetractionData{{Agent: "claude", Account: "work"}})
	require.Error(t, err, "a retraction with no proof time cannot be merged")

	_, err = mergeAccountLimitRetractions(nil,
		[]session.AccountLimitRetractionData{{Agent: "claude", ProvenAt: later}})
	require.Error(t, err, "a retraction with no account cannot be scoped")
}

func writeRawAccountLimitLedger(t *testing.T, body string) string {
	t.Helper()
	path, err := accountLimitLedgerPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// A malformed wall is integrity damage: the file's exclusions cannot be
// trusted, so the read fails closed. A malformed or untrustworthy RETRACTION
// is different — it can only ever free an account wrongly — so it loses its
// power without breaking the read: the entry is dropped and every wall still
// loads (#6802 review). "The ledger could not be read" collapsing into
// "nothing is walled" is how a limited account becomes eligible, and failing
// the whole file on one questionable proof is how an ordinary clock
// correction wedges kills, handoffs, and rotation.
func TestReadAccountLimitLedgerDropsUntrustworthyRetractions(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetAccountLimitRetractionQuarantine(t)
	wallJSON := `"observations":[{"agent":"claude","account":"work","observed_at":"2025-11-10T09:00:00Z"}]`

	for name, retractionJSON := range map[string]string{
		"retraction with an empty identity": `{"agent":"claude","account":"","proven_at":"2025-11-10T12:00:00Z"}`,
		"retraction with no proof time":     `{"agent":"claude","account":"work"}`,
		"retraction dated in the future":    `{"agent":"claude","account":"work","proven_at":"2999-11-10T12:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeRawAccountLimitLedger(t,
				fmt.Sprintf(`{"schema_version":1,%s,"retractions":[%s]}`, wallJSON, retractionJSON))

			evidence, err := readAccountLimitLedger(path)
			require.NoError(t, err,
				"an untrustworthy retraction must not make the whole ledger unreadable")
			require.Len(t, evidence.Observations, 1, "the wall still loads")
			require.Empty(t, evidence.Retractions,
				"the untrustworthy entry simply has no power to free anything")
		})
	}
}

// The drop must hold at admission too: a stored future-dated retraction — the
// file a forward-jumped clock or an older binary left behind — frees nothing
// while the real wall still excludes.
func TestLimitedAccountsForSwapIgnoresAStoredFutureRetraction(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetAccountLimitRetractionQuarantine(t)
	writeRawAccountLimitLedger(t,
		`{"schema_version":1,
		  "observations":[{"agent":"claude","account":"work","observed_at":"2025-11-10T09:00:00Z"}],
		  "retractions":[{"agent":"claude","account":"work","proven_at":"2999-11-10T12:00:00Z"}]}`)

	limited, err := (&Manager{}).limitedAccountsForSwap("claude", ledgerBackedEvidence())
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"an untrustworthy retraction defeats no wall — and never blocks reading the walls it sits beside")
}

// The additive read: a reader that knows the new field folds both halves; a
// well-formed retraction travels with its walls untouched.
func TestReadAccountLimitLedgerReadsRetractionsWithWalls(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path := writeRawAccountLimitLedger(t,
		`{"schema_version":1,
		  "observations":[{"agent":"claude","account":"work","observed_at":"2025-11-10T09:00:00Z"}],
		  "retractions":[{"agent":"claude","account":"work","proven_at":"2025-11-10T12:00:00Z"}]}`)

	evidence, err := readAccountLimitLedger(path)
	require.NoError(t, err)
	require.Len(t, evidence.Observations, 1)
	require.Equal(t, "work", evidence.Observations[0].Account)
	require.Equal(t, []session.AccountLimitRetractionData{{
		Agent: "claude", Account: "work",
		ProvenAt: time.Date(2025, 11, 10, 12, 0, 0, 0, time.UTC),
	}}, evidence.Retractions)
}

func TestRetractAccountLimitObservationsWritesTombstones(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	proven := time.Date(2025, 11, 10, 12, 0, 0, 0, time.UTC)
	reset := proven.Add(-time.Hour)
	require.NoError(t, retainAccountLimitObservations([]session.AccountLimitObservationData{{
		Agent: "claude", Account: "work", ObservedAt: proven.Add(-2 * time.Hour), ResetAt: reset,
	}}))

	require.NoError(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
		{Agent: "claude", Account: "work", ProvenAt: proven},
	}))

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Observations, 1, "the wall is kept — the defeat is computed at read, not by deletion")
	require.Equal(t, []session.AccountLimitRetractionData{
		{Agent: "claude", Account: "work", ProvenAt: proven},
	}, evidence.Retractions)
}

func TestRetractAccountLimitObservationsRefusesUnscopedOrUntimed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	proven := time.Date(2025, 11, 10, 12, 0, 0, 0, time.UTC)

	require.Error(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
		{Agent: "claude", Account: "work"},
	}), "an untimed proof orders against nothing")
	require.Error(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
		{Agent: "claude", ProvenAt: proven},
	}), "a proof with no account cannot be scoped")

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions, "refused retractions must not reach the ledger")
}

// A proof cannot carry an instant that has not happened: the write refuses
// future times (#6802 review) — otherwise a forward clock jump mints a
// tombstone that defeats every wall sighted between now and then.
func TestRetractAccountLimitObservationsRefusesAFutureProof(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.Error(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
		{Agent: "claude", Account: "work", ProvenAt: nowFunc().Add(time.Hour)},
	}), "a proof dated in the future cannot be ordered against real walls")

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions, "a refused retraction must not reach the ledger")
}

// The race that decides everything: a retraction and a fresh wall for the same
// account can land in either order. Event time — not file-write order — picks
// the winner, in BOTH directions.
func TestAccountLimitLedgerEventTimeBeatsWriteOrder(t *testing.T) {
	observed := time.Date(2025, 11, 10, 9, 0, 0, 0, time.UTC)
	proven := observed.Add(30 * time.Minute)

	t.Run("retraction written first, wall retained second, wall is newer", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		require.NoError(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: proven},
		}))
		require.NoError(t, retainAccountLimitObservations([]session.AccountLimitObservationData{{
			Agent: "claude", Account: "work", ObservedAt: proven.Add(time.Minute),
		}}))

		limited, err := (&Manager{}).limitedAccountsForSwap("claude", ledgerBackedEvidence())
		require.NoError(t, err)
		require.Contains(t, limited, "work",
			"a wall sighted after the proof stays exclusionary however it reached the disk")
	})

	t.Run("wall retained first, retraction written second, proof is newer", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		require.NoError(t, retainAccountLimitObservations([]session.AccountLimitObservationData{{
			Agent: "claude", Account: "work", ObservedAt: observed,
		}}))
		require.NoError(t, retractAccountLimitObservations([]session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: proven},
		}))

		limited, err := (&Manager{}).limitedAccountsForSwap("claude", ledgerBackedEvidence())
		require.NoError(t, err)
		require.NotContains(t, limited, "work",
			"a proof after the wall's last sighting frees the account however it reached the disk")
	})
}

func TestLoadAccountLimitEvidenceSnapshotCarriesRowsAndRetractions(t *testing.T) {
	observed := time.Date(2025, 11, 10, 9, 0, 0, 0, time.UTC)
	rowObs := session.AccountLimitObservationData{Agent: "claude", Account: "work", ObservedAt: observed}
	ledgerObs := session.AccountLimitObservationData{Agent: "codex", Account: "play", ObservedAt: observed}
	retraction := session.AccountLimitRetractionData{Agent: "claude", Account: "work", ProvenAt: observed.Add(time.Hour)}

	evidence, err := loadAccountLimitEvidenceSnapshot(
		func() ([]session.AccountLimitObservationData, error) {
			return []session.AccountLimitObservationData{rowObs}, nil
		},
		func() (accountLimitEvidence, error) {
			return accountLimitEvidence{
				Observations: []session.AccountLimitObservationData{ledgerObs},
				Retractions:  []session.AccountLimitRetractionData{retraction},
			}, nil
		})
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]session.AccountLimitObservationData{rowObs, ledgerObs}, evidence.Observations)
	require.Equal(t, []session.AccountLimitRetractionData{retraction}, evidence.Retractions,
		"proofs live only in the ledger and survive the row merge untouched")
}

func TestLoadAccountLimitEvidenceSnapshotFailsClosedOnUnreadableEvidence(t *testing.T) {
	okRows := func() ([]session.AccountLimitObservationData, error) { return nil, nil }
	okLedger := func() (accountLimitEvidence, error) { return accountLimitEvidence{}, nil }
	boom := errors.New("unreadable")

	if _, err := loadAccountLimitEvidenceSnapshot(
		func() ([]session.AccountLimitObservationData, error) { return nil, boom }, okLedger); err == nil {
		t.Fatal("unreadable session rows must fail the snapshot, not read as no walls")
	}
	if _, err := loadAccountLimitEvidenceSnapshot(
		okRows, func() (accountLimitEvidence, error) { return accountLimitEvidence{}, boom }); err == nil {
		t.Fatal("an unreadable ledger must fail the snapshot, not read as no walls")
	}
}

func TestLimitedAccountsForSwapFoldsStoredWallsThroughRetractions(t *testing.T) {
	now := nowFunc()
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "work", ObservedAt: now.Add(-time.Hour)},
			{Agent: "claude", Account: "personal", ObservedAt: now.Add(-time.Hour)},
		},
		Retractions: []session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: now.Add(-time.Minute)},
		},
	}, nil)

	limited, err := (&Manager{}).limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.NotContains(t, limited, "work", "the proven account is back in the candidate pool")
	require.Contains(t, limited, "personal", "unproven walls are untouched by another account's retraction")
}

// A parked session IS current evidence: a proof about the account elsewhere
// must never un-park it or read its live wall as stale. Retractions fold only
// the stored record — the live LimitIdentity arm is independent of them.
func TestLimitedAccountsForSwapRetractionNeverUnparksALiveWall(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "parked",
		session.NewFakeBackend(), true, session.Running)
	inst.Account = "work"
	resetAt := nowFunc().Add(24 * time.Hour)
	inst.SetLimitReached(resetAt)

	// The stored record for the same identity is defeated by a later proof —
	// admissible retraction territory — yet the live wall still stands.
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{{
			Agent: "claude", Account: "work", ObservedAt: nowFunc().Add(-time.Hour),
		}},
		Retractions: []session.AccountLimitRetractionData{{
			Agent: "claude", Account: "work", ProvenAt: nowFunc().Add(-time.Minute),
		}},
	}, nil)

	limited, err := manager.limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"a session parked at its wall is current evidence a retraction cannot retire")
	require.True(t, inst.LimitReached(),
		"the parked session keeps its own parked state when its stored observation is defeated")
}

// --- usage-reading evidence ----------------------------------------------------

func registerAccountInstance(t *testing.T, m *Manager, repoID, repoPath, title, program, account string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: program})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.Account = account
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// codexUsageClaim renders one rate_limits rollout line — the only record the
// usage indexer reads — timestamped ts with BOTH windows at pct percent: a
// proof needs a value for every window the account is subject to, so the
// default claim reports the primary and the weekly secondary alike. The
// shape is handwritten from the pinned Codex record; the day directory is
// derived from the record date so the fixture lands inside the enumeration
// window it models.
func codexUsageClaim(ts time.Time, pct float64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":1761902400},"secondary":{"used_percent":%v,"window_minutes":10080,"resets_at":1762502400}}}}`,
		ts.UTC().Format(time.RFC3339Nano), pct, pct)
}

// codexUsageClaimMissingSecondary renders a claim with "secondary": null —
// the shape that says nothing about the weekly window's wall.
func codexUsageClaimMissingSecondary(ts time.Time, pct float64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":1761902400},"secondary":null}}}`,
		ts.UTC().Format(time.RFC3339Nano), pct)
}

// seedCodexRollout drops a rollout file holding lines into the named codex
// account's own store — <af-home>/accounts/codex/<account>/sessions/<day>/ —
// the store the reconcile's indexer refresh scans. Day dirs are
// sessions/YYYY/MM/DD; derive one from the wall clock the claim claims.
func seedCodexRollout(t *testing.T, home, account, day, name string, lines ...string) string {
	t.Helper()
	accountDir, err := agentaccount.Dir(home, tmux.ProgramCodex, account)
	require.NoError(t, err)
	dir := filepath.Join(accountDir, "sessions", day)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	return path
}

// rolloutName yields a filename matching the Codex rollout shape.
func rolloutName(suffix string) string {
	return fmt.Sprintf("rollout-2025-11-10T09-00-00-00000000-0000-4000-8000-%012s.jsonl", suffix)
}

// usageReadingProof is the proof gate: only an OBSERVED, past-dated claim
// that reports every window the account is subject to, each below the
// configured wall, proves the account served at the record's own instant.
func TestUsageReadingProof(t *testing.T) {
	now := nowFunc()
	at := now.Add(-time.Minute)
	observed := func(recorded time.Time, windows ...quota.UsageWindow) rateindex.AccountReading {
		return rateindex.AccountReading{Status: rateindex.StatusObserved,
			Reading: quota.UsageReading{RecordedAt: recorded, Windows: windows}}
	}
	window := func(name string, pct float64) quota.UsageWindow {
		return quota.UsageWindow{Name: name, UsedPercent: pct}
	}
	// A healthy snapshot reports every window the account is subject to:
	// primary and secondary alike below the configured wall.
	healthy := func() []quota.UsageWindow {
		return []quota.UsageWindow{window("primary", 40), window("secondary", 40)}
	}

	t.Run("a below-wall reading proves at its record timestamp", func(t *testing.T) {
		proven, ok := usageReadingProof(observed(at, healthy()...), 97, now)
		require.True(t, ok)
		require.True(t, proven.Equal(at),
			"the proof instant is the provider's timestamp, never when af happened to read it")
	})

	t.Run("a missing claim is no evidence", func(t *testing.T) {
		_, ok := usageReadingProof(rateindex.AccountReading{Status: rateindex.StatusNone}, 97, now)
		require.False(t, ok)
	})

	t.Run("an untrusted claim proves nothing", func(t *testing.T) {
		_, ok := usageReadingProof(rateindex.AccountReading{
			Status: rateindex.StatusUnknown, Reason: "scan-bounded",
		}, 97, now)
		require.False(t, ok, "a bounded or malformed scan cannot free a wall")
	})

	t.Run("a future-dated record is untrustworthy", func(t *testing.T) {
		_, ok := usageReadingProof(observed(now.Add(time.Minute), healthy()...), 97, now)
		require.False(t, ok)
	})

	t.Run("an untimed record proves nothing", func(t *testing.T) {
		_, ok := usageReadingProof(observed(time.Time{}, healthy()...), 97, now)
		require.False(t, ok)
	})

	t.Run("a windowless snapshot is not a zero reading", func(t *testing.T) {
		_, ok := usageReadingProof(observed(at), 97, now)
		require.False(t, ok, "no windows is no data, not headroom")
	})

	t.Run("a record missing a window is not proof", func(t *testing.T) {
		_, ok := usageReadingProof(observed(at, window("primary", 10)), 97, now)
		require.False(t, ok,
			"a calm primary beside \"secondary\": null is silent on the weekly wall, not below it")
	})

	t.Run("a window at the wall is still the wall", func(t *testing.T) {
		_, ok := usageReadingProof(observed(at, window("primary", 97), window("secondary", 40)), 97, now)
		require.False(t, ok, "below means below — at the wall keeps excluding")
	})

	t.Run("one hot window keeps the wall", func(t *testing.T) {
		_, ok := usageReadingProof(observed(at, window("primary", 10), window("secondary", 99)), 97, now)
		require.False(t, ok,
			"the account's own newest word still shows a window at the wall")
	})
}

// The happy path: a stored codex wall whose account's own rollout claims a
// below-wall reading recorded after the sighting earns a tombstone — and the
// tombstone's instant is the record's timestamp.
func TestReconcileAccountLimitProofsRetractsOnABelowWallReading(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Retractions, 1)
	requireAccountLimitRetraction(t, evidence.Retractions, tmux.ProgramCodex, "work", base.Add(-time.Minute))
}

// The newest claim's own word decides: a reading still at or above the
// configured wall retracts nothing.
func TestReconcileAccountLimitProofsKeepsAWallItsNewestReadingStillShows(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 99))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a reading still above the wall is the wall's own word — no retraction")
}

// A reading recorded before the wall's sighting is the OLDER fact: it proves
// the account served then, not after — no tombstone is written (#6802 review:
// reconcile writes only what admission would honor).
func TestReconcileAccountLimitProofsIgnoresAReadingOlderThanTheWall(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-2*time.Hour), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a proof that predates the wall litters the ledger with a tombstone admission never honors")
}

// The stored record a reading answers for is not only the ledger: a live
// session's own observation list is folded too, so a fresh wall sighted by a
// live sibling outranks an older reading just as durably.
func TestReconcileAccountLimitProofsRespectsLiveInstanceWalls(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	// A live session parked at its wall moments ago contributes a fresh live
	// observation; the account's newest reading predates it.
	inst := registerAccountInstance(t, manager, repoID, repoPath, "parked", tmux.ProgramCodex, "work")
	inst.SetLimitReached(time.Time{})
	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Hour), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a live session's fresh wall is the newer fact — the older reading earns no tombstone")
	require.True(t, inst.LimitReached(), "retraction never un-parks a live wall")
}

// An unreadable evidence record must never be mistaken for "nothing is
// walled": the pass aborts and writes nothing rather than acting on a partial
// picture.
func TestReconcileAccountLimitProofsAbortsOnUnreadableEvidence(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{}, errors.New("ledger unreadable"))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a failed evidence read means no pass at all — the next tick retries the whole picture")
}

// Retraction is identity-scoped: a codex reading proves nothing about a claude
// account wearing the same label — and an agent with no usage record never
// earns a read of any store.
func TestReconcileAccountLimitProofsSkipsAgentsWithoutReadingSources(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	// The codex rollout for the same account LABEL is a healthy reading — but
	// the wall is filed under claude, whose agent has no usage record.
	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramClaude, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a codex reading is not evidence for a claude wall — identity is (agent, account)")
}

// Claims inside a carried prefix hold the SOURCE account's numbers wearing
// the target's name (#6794): the watermark quarantines them, so a wall keeps
// excluding until a claim lands past the copied bytes.
func TestReconcileAccountLimitProofsHonorsTheCarriedPrefix(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	rollout := seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	// Every byte of the file is carried content: the claim below it is the
	// source account's reading, not this account's.
	info, err := os.Stat(rollout)
	require.NoError(t, err)
	store := handoff.NewStore(filepath.Join(home, "instances", handoff.StateFileName))
	require.NoError(t, store.RecordCarriedPrefix(rollout, info.Size()))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a claim inside the carried prefix is the SOURCE account's word — it proves nothing here")

	// The target account's own claim lands past the watermark: now it proves.
	fresh := codexUsageClaim(base.Add(-time.Second), 30)
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(fresh + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	manager.reconcileAccountLimitProofs()

	evidence, err = loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Retractions, 1)
	requireAccountLimitRetraction(t, evidence.Retractions, tmux.ProgramCodex, "work", base.Add(-time.Second))
}

// A corrupt watermark file fails the whole pass closed: a reading inside a
// prefix that cannot be ruled out could be the other account's evidence.
func TestReconcileAccountLimitProofsFailsClosedOnUnreadableWatermarks(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	statePath := filepath.Join(home, "instances", handoff.StateFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("not json"), 0o600))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"an unknown watermark means an observed claim cannot be trusted — the pass writes nothing")
}

// A corrupt RAILS file fails the pass closed the same way the watermark
// file does: the unrecorded-carry quarantine is the record that some
// rollout's copied bytes cannot be trusted, and a file that will not
// decode hides that record — a reading inside an unrecorded prefix could
// be the other account's evidence (#6852 round-2 review).
func TestReconcileAccountLimitProofsFailsClosedOnUnreadableRails(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	railsPath := filepath.Join(home, "instances", handoff.RailsFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(railsPath), 0o700))
	require.NoError(t, os.WriteFile(railsPath, []byte("not json"), 0o600))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a rails file that will not decode hides the quarantine record — the pass writes nothing")
}

// An account whose carry quarantine overflowed the marker cap is flagged
// untrusted in the rails file wholesale: the reconcile skips its store
// entirely — no claim in it may free a wall, however healthy it reads
// (#6852 review). The flag stands until a covering watermark lands or the
// rails are repaired.
func TestReconcileAccountLimitProofsSkipsAQuarantinedAccount(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	dir, err := agentaccount.Dir(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	railsPath := filepath.Join(home, "instances", handoff.RailsFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(railsPath), 0o700))
	require.NoError(t, os.WriteFile(railsPath, []byte(
		fmt.Sprintf(`{"schema_version":1,"quarantined_accounts":{%q:{"at":%q}}}`,
			dir, base.Add(-time.Hour).UTC().Format(time.RFC3339Nano))), 0o600))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a quarantined account's claims are never retraction evidence — the pass skips it")
}

// A quarantine marker always wins over an older recorded watermark: the
// marker may describe a carry whose bytes landed past the recorded
// boundary, so honoring the stale watermark would free a wall on the
// source account's claims (#6852 round-2 review).
func TestReconcileAccountLimitProofsAMarkerBeatsARecordedWatermark(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	rollout := seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	// The recorded watermark covers only the first bytes — taken alone the
	// healthy claim sits past it and would read as the target's own.
	store := handoff.NewStore(filepath.Join(home, "instances", handoff.StateFileName))
	require.NoError(t, store.RecordCarriedPrefix(rollout, 10))
	// …but a later carry quarantined the file end to end and its marker
	// stands: the whole file's claims are carried poison until a watermark
	// covering the marked span lands.
	dir, err := agentaccount.Dir(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	rails := handoff.NewRailsStore(
		filepath.Join(home, "instances", handoff.RailsFileName),
		filepath.Join(home, "instances", handoff.StateFileName))
	info, err := os.Stat(rollout)
	require.NoError(t, err)
	_, err = rails.NoteUnrecordedCarry(rollout, dir, info.Size(), base.Add(-2*time.Hour))
	require.NoError(t, err)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"the marker's covered span beats the stale watermark — no claim inside it is the target's")
}

// The wall line is the RESOLVED config leaf, not a hardcoded 97: an operator
// who raised it keeps walls that a default threshold would retract.
func TestReconcileAccountLimitProofsRespectsConfiguredWallPercent(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	// wall_percent must satisfy prefer_below < move_idle < wall against the
	// defaults (85 < 95), so the raised line is 99.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[account_handoff]\nwall_percent = 99\n"), 0o600))

	// 98 is below the configured 99 but above the default 97: only the
	// configured line says this is a healthy account.
	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 98))
	// 99.5 is at the configured wall: the account's own word still excludes.
	seedCodexRollout(t, home, "play", base.UTC().Format("2006/01/02"), rolloutName("000000000002"),
		codexUsageClaim(base.Add(-time.Minute), 99.5))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
			{Agent: tmux.ProgramCodex, Account: "play", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Retractions, 1)
	requireAccountLimitRetraction(t, evidence.Retractions, tmux.ProgramCodex, "work", base.Add(-time.Minute))
}

// A wall that lost its sighting time — a session row rewritten by an older
// binary that dropped observed_at — can never be ordered against a retraction,
// so it must never lose to one: its true sighting could postdate the proof
// (#6802 review). The untimed wall stands until expiry or a fresher sighting.
func TestLimitedAccountsForSwapUntimedWallNeverDefeated(t *testing.T) {
	now := nowFunc()
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: "claude", Account: "work", ObservedAt: time.Time{}},
		},
		Retractions: []session.AccountLimitRetractionData{
			{Agent: "claude", Account: "work", ProvenAt: now.Add(-time.Minute)},
		},
	}, nil)

	limited, err := (&Manager{}).limitedAccountsForSwap("claude", loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"a wall with no sighting time cannot be ordered — it keeps excluding however new the proof")
}

// The reconcile owns a durable scan far too heavy for the 1 Hz poll goroutine;
// it runs on its own ticker so the poll never waits on the disk read or the
// rollout scans (#6802 review). The loop runs passes on its interval and
// joins shutdown.
func TestAccountLimitReconcileLoopRunsOffThePoll(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	bypassAccountLimitProofGate(t)
	loads := make(chan struct{}, 16)
	previous := loadAccountLimitEvidenceForSwap
	loadAccountLimitEvidenceForSwap = func() (accountLimitEvidence, error) {
		loads <- struct{}{}
		return accountLimitEvidence{}, nil
	}
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = previous })

	prevInterval := accountLimitProofReconcileInterval
	accountLimitProofReconcileInterval = 10 * time.Millisecond
	t.Cleanup(func() { accountLimitProofReconcileInterval = prevInterval })

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	startAccountLimitReconcileLoop(manager, stopCh, &wg)

	select {
	case <-loads:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile loop never ran a pass on its own ticker")
	}
	close(stopCh)
	wg.Wait()
}

// Re-seeing an unchanged wall is not a fact change: it must never flag the
// observation dirty, rewrite the row, or publish a session update — the poll
// was once persisting the same fact once a second for as long as the session
// stayed parked (#6802 review). A wall sighted after the parked state left is
// a new event and DOES land.
func TestPollWritesNothingForAnUnchangedWallResighting(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "walled", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.Account = "work"
	seedDiskInstance(t, repoID, "walled", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "walled")] = inst
	manager.mu.Unlock()
	_, events := manager.events.subscribe()

	persistUnchangedAxes := func() {
		manager.persistPollChangeWithIdleEvidence(repoID, inst,
			inst.GetLiveness(), inst.ToInstanceData().LimitResetAt, inst.LimitResetEvidence(), false, false)
	}
	storedRow := func() []byte {
		raw, err := config.LoadRepoInstances(repoID)
		require.NoError(t, err)
		return raw
	}
	drainEvents := func() {
		for {
			select {
			case <-events:
			default:
				return
			}
		}
	}

	inst.SetLimitReached(time.Time{})
	require.True(t, inst.AccountLimitObservationsUnpersisted())
	persistUnchangedAxes()
	require.False(t, inst.AccountLimitObservationsUnpersisted(), "the first sighting reached disk")
	first := storedRow()
	drainEvents()

	// The unchanged banner re-parks on every idle tick through the epoch
	// path: it must be a complete no-op.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.SetLimitReachedAtEpoch(time.Time{}, "", session.LimitBanner{}, epoch))
	require.False(t, inst.AccountLimitObservationsUnpersisted(),
		"re-seeing the unchanged banner flags nothing")
	persistUnchangedAxes()
	require.Equal(t, first, storedRow(),
		"an unchanged re-sight writes nothing")
	select {
	case ev := <-events:
		t.Fatalf("an unchanged re-sight published %s — the parked session must not re-announce", ev.Type)
	default:
	}

	// The recorder holds the same line on the non-epoch path: a parked
	// re-sight of the same claim is still no durable fact.
	inst.SetLimitReached(time.Time{})
	require.False(t, inst.AccountLimitObservationsUnpersisted(),
		"a parked re-sight of the same claim is not a fact change")
	persistUnchangedAxes()
	require.Equal(t, first, storedRow())

	// And a genuinely new wall event — the account recovered and hit the
	// limit again — still lands durably.
	inst.ClearLimitReached()
	inst.SetLimitReached(time.Time{})
	require.True(t, inst.AccountLimitObservationsUnpersisted(),
		"a wall sighted after recovery is a new fact")
	persistUnchangedAxes()
	require.False(t, inst.AccountLimitObservationsUnpersisted(), "the new wall reached disk")
}

// The end-to-end shape: a usage reading proves the account served after the
// stored wall; the reconcile retracts it; admission folds the pair and the
// account is a candidate again.
func TestUsageReadingEndToEndRestoresTheAccount(t *testing.T) {
	base := nowFunc()
	manager, _, _ := newStatusTestManager(t)
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	storedWall := session.AccountLimitObservationData{
		Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-2 * time.Hour),
	}
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{storedWall},
	}, nil)

	limited, err := manager.limitedAccountsForSwap(tmux.ProgramCodex, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work", "precondition: the stored wall excludes the account")

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-time.Minute), 40))
	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Retractions, 1)
	requireAccountLimitRetraction(t, evidence.Retractions, tmux.ProgramCodex, "work", base.Add(-time.Minute))

	// The loader used by admission sees the ledger's retraction fold the wall.
	limited, err = manager.limitedAccountsForSwap(tmux.ProgramCodex, ledgerBackedEvidence(storedWall))
	require.NoError(t, err)
	require.NotContains(t, limited, "work",
		"admission folds the pair: the proven account returns to the candidate pool")
}

// A once-future-dated retraction must never count: the read drops it while
// the file still loads, the quarantine keeps it inert after the clock
// catches up, and the load's purge removes it from the file so no later
// process can resurrect it (#6802 review).
func TestReadAccountLimitLedgerAFutureRetractionStaysInert(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetAccountLimitRetractionQuarantine(t)
	prevNow := nowFunc
	t.Cleanup(func() { nowFunc = prevNow })
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }

	proven := now.Add(time.Hour)
	path := writeRawAccountLimitLedger(t, fmt.Sprintf(
		`{"schema_version":1,
		  "observations":[{"agent":"claude","account":"work","observed_at":"2025-11-10T09:00:00Z"}],
		  "retractions":[{"agent":"claude","account":"work","proven_at":%q}]}`,
		proven.UTC().Format(time.RFC3339Nano)))

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a proof dated in the future is dropped while the walls still load")

	// The clock catches up — the entry must still not count, and the purge
	// the load triggered has written it out of the file for good.
	now = proven.Add(time.Hour)
	evidence, err = loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a once-future-dated retraction never counts, even after its instant passes")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "retractions",
		"the purge removes the poisoned entry durably, not just for this read")
}

// The record's timestamp is when codex wrote the line, not when the request
// started: a response completing inside the write margin of the sighting
// could belong to a request the wall already blocked, so it proves nothing.
// Only a record that clears the margin counts (#6802 review).
func TestReconcileAccountLimitProofsRequiresTheWriteMargin(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	bypassAccountLimitProofGate(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	day := base.UTC().Format("2006/01/02")

	// "work"'s newest claim lands 15 minutes after the wall's sighting —
	// inside the write margin: it may be the answer to a pre-wall request.
	seedCodexRollout(t, home, "work", day, rolloutName("000000000001"),
		codexUsageClaim(base.Add(-45*time.Minute), 40))
	// "play"'s claim clears the margin — 45 minutes past the same sighting.
	seedCodexRollout(t, home, "play", day, rolloutName("000000000002"),
		codexUsageClaim(base.Add(-15*time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
			{Agent: tmux.ProgramCodex, Account: "play", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Len(t, evidence.Retractions, 1)
	requireAccountLimitRetraction(t, evidence.Retractions, tmux.ProgramCodex, "play", base.Add(-15*time.Minute))
}

// The cheap gate must let every real codex wall through while sparing users
// without one the durable scan entirely.
func TestAccountLimitProofWorthScanning(t *testing.T) {
	codexAccountDir := func(t *testing.T) {
		t.Helper()
		home, err := config.GetConfigDir()
		require.NoError(t, err)
		dir, err := agentaccount.Dir(home, tmux.ProgramCodex, "work")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	t.Run("no codex accounts means nothing could be proven", func(t *testing.T) {
		manager, _, _ := newStatusTestManager(t)
		require.False(t, accountLimitProofWorthScanning(manager))
	})

	t.Run("codex accounts without any codex wall", func(t *testing.T) {
		manager, _, _ := newStatusTestManager(t)
		codexAccountDir(t)
		require.False(t, accountLimitProofWorthScanning(manager),
			"registered accounts alone are not a wall to free")
	})

	t.Run("a codex wall in the ledger runs the pass", func(t *testing.T) {
		manager, _, _ := newStatusTestManager(t)
		codexAccountDir(t)
		writeRawAccountLimitLedger(t,
			`{"schema_version":1,"observations":[{"agent":"codex","account":"work","observed_at":"2025-11-10T09:00:00Z"}]}`)
		require.True(t, accountLimitProofWorthScanning(manager))
	})

	t.Run("a codex wall on a live instance runs the pass", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		codexAccountDir(t)
		inst := registerAccountInstance(t, manager, repoID, repoPath, "parked", tmux.ProgramCodex, "work")
		inst.SetLimitReached(time.Time{})
		require.True(t, accountLimitProofWorthScanning(manager))
	})

	t.Run("a codex wall in a stored row runs the pass", func(t *testing.T) {
		manager, repoID, _ := newStatusTestManager(t)
		codexAccountDir(t)
		require.NoError(t, config.LoadState().SaveInstances(repoID, json.RawMessage(
			`[{"title":"walled","status":0,
			   "account_limit_observations":[{"agent":"codex","account":"work","observed_at":"2025-11-10T09:00:00Z"}]}]`)))
		require.True(t, accountLimitProofWorthScanning(manager))
	})

	t.Run("a codex session without a codex wall stays skipped", func(t *testing.T) {
		manager, repoID, _ := newStatusTestManager(t)
		codexAccountDir(t)
		// The row IS a codex session and even carries a wall — a claude
		// one. The bytes "codex" everywhere must not run the pass: only an
		// actual codex wall observation can (#6802 review).
		require.NoError(t, config.LoadState().SaveInstances(repoID, json.RawMessage(
			`[{"title":"walled","status":0,"program":"codex",
			   "account_limit_observations":[{"agent":"claude","account":"work","observed_at":"2025-11-10T09:00:00Z"}]}]`)))
		require.False(t, accountLimitProofWorthScanning(manager),
			"a claude wall on a codex session row is not a codex wall observation")
	})

	t.Run("an expired codex wall does not run the pass", func(t *testing.T) {
		manager, repoID, _ := newStatusTestManager(t)
		codexAccountDir(t)
		require.NoError(t, config.LoadState().SaveInstances(repoID, json.RawMessage(
			`[{"title":"walled","status":0,
			   "account_limit_observations":[{"agent":"codex","account":"work","observed_at":"2025-11-10T09:00:00Z","reset_at":"2025-11-10T10:00:00Z"}]}]`)))
		require.False(t, accountLimitProofWorthScanning(manager),
			"a wall whose reset already passed is not a wall — nothing left to free")
	})

	t.Run("a retracted codex wall does not run the pass", func(t *testing.T) {
		manager, _, _ := newStatusTestManager(t)
		resetAccountLimitRetractionQuarantine(t)
		codexAccountDir(t)
		writeRawAccountLimitLedger(t,
			`{"schema_version":1,
			  "observations":[{"agent":"codex","account":"work","observed_at":"2025-11-10T09:00:00Z"}],
			  "retractions":[{"agent":"codex","account":"work","proven_at":"2025-11-10T12:00:00Z"}]}`)
		require.False(t, accountLimitProofWorthScanning(manager),
			"a wall already retracted is already free — nothing left to free")
	})

	t.Run("a legacy parked row's synthesized codex wall runs the pass", func(t *testing.T) {
		manager, repoID, _ := newStatusTestManager(t)
		codexAccountDir(t)
		require.NoError(t, config.LoadState().SaveInstances(repoID, json.RawMessage(
			`[{"title":"walled","status":0,"program":"codex","account":"work",
			   "liveness":6,"limit_account":"work"}]`)))
		require.True(t, accountLimitProofWorthScanning(manager),
			"a parked row with a named codex account synthesizes a real wall observation")
	})
}

// The carry-commit race the watermark exists for: the wall is sighted, THEN
// the carry lands a rollout whose copied bytes hold a healthy claim that
// postdates the sighting by enough to clear the write margin. Read naively
// that claim is fresh proof for the target — only the watermark the commit
// recorded keeps it the source account's word (#6802 review).
func TestReconcileAccountLimitProofsACarryLandingAfterTheSightingCannotRetractTheWall(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	bypassAccountLimitProofGate(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	// The claim is dated 40 minutes after the sighting — past the write
	// margin, so it would free the account if the copied bytes counted.
	rollout := seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaim(base.Add(-20*time.Minute), 40))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	// What the carry commit wrote (session/carry.copy → RecordCarriedPrefix):
	// every copied byte is the source account's and must be quarantined.
	info, err := os.Stat(rollout)
	require.NoError(t, err)
	store := handoff.NewStore(filepath.Join(home, "instances", handoff.StateFileName))
	require.NoError(t, store.RecordCarriedPrefix(rollout, info.Size()))

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a post-sighting claim inside the carried prefix stays the source account's — the wall stands")
}

// A record that reports only some of the windows the account is subject to
// is silent about the rest: "secondary": null beside a calm primary says
// nothing about the weekly wall, so it frees nothing even when it postdates
// the sighting and clears the write margin (#6802 review).
func TestReconcileAccountLimitProofsAMissingWindowIsNotProof(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	bypassAccountLimitProofGate(t)
	base := nowFunc()
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	seedCodexRollout(t, home, "work", base.UTC().Format("2006/01/02"), rolloutName("000000000001"),
		codexUsageClaimMissingSecondary(base.Add(-time.Minute), 10))
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
		},
	}, nil)

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a snapshot silent on the weekly window is not proof of a healthy account")
}
