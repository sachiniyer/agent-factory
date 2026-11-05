package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// Refutable account-limit evidence (#6776 slice 1d, #4404). A wall observation
// is durable, but it is only a claim about the past: positive proof that the
// account served traffic later — a confirmed turn under it — ends the
// observation without deleting it. The newer fact wins by when it happened,
// never by which write landed on disk last, and a currently-walled account is
// never made healthy by a retraction.

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
			"a turn before the claimed reset is not proof the wall ended")
	})

	t.Run("a wall sighted after the proof is the newer fact", func(t *testing.T) {
		fresh := wall
		fresh.ObservedAt = now.Add(-time.Minute)
		fresh.ResetAt = now.Add(-time.Hour)
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

// A malformed retraction is integrity damage like a malformed wall: the read
// must fail closed, because "the ledger could not be read" collapsing into
// "nothing is walled" is how a limited account becomes eligible (#6776 1d).
func TestReadAccountLimitLedgerFailsClosedOnMalformedRetraction(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for name, body := range map[string]string{
		"retraction with an empty identity": `{"schema_version":1,"retractions":[{"agent":"claude","account":"","proven_at":"2026-11-10T09:00:00Z"}]}`,
		"retraction with no proof time":     `{"schema_version":1,"retractions":[{"agent":"claude","account":"work"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeRawAccountLimitLedger(t, body)
			_, err := readAccountLimitLedger(path)
			require.Error(t, err)
		})
	}
}

// The additive read: a reader that knows the new field folds both halves; a
// well-formed retraction travels with its walls untouched.
func TestReadAccountLimitLedgerReadsRetractionsWithWalls(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path := writeRawAccountLimitLedger(t,
		`{"schema_version":1,
		  "observations":[{"agent":"claude","account":"work","observed_at":"2026-11-10T09:00:00Z"}],
		  "retractions":[{"agent":"claude","account":"work","proven_at":"2026-11-10T12:00:00Z"}]}`)

	evidence, err := readAccountLimitLedger(path)
	require.NoError(t, err)
	require.Len(t, evidence.Observations, 1)
	require.Equal(t, "work", evidence.Observations[0].Account)
	require.Equal(t, []session.AccountLimitRetractionData{{
		Agent: "claude", Account: "work",
		ProvenAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	}}, evidence.Retractions)
}

func TestRetractAccountLimitObservationsWritesTombstones(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	proven := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
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
	proven := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

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

// The race that decides everything: a retraction and a fresh wall for the same
// account can land in either order. Event time — not file-write order — picks
// the winner, in BOTH directions.
func TestAccountLimitLedgerEventTimeBeatsWriteOrder(t *testing.T) {
	observed := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
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
	observed := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
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

// --- turn-evidence collection ------------------------------------------------

// codexArmFrame is a captured codex pane with no in-turn chrome: it arms the
// scoped indicator watch so a later chrome-bearing frame can prove a turn.
const codexArmFrame = "working\n› \n"

// codexTurnFrame carries codex's in-turn chrome — the composer glyph row
// above the bare "esc to interrupt" hint.
const codexTurnFrame = "working\n› \nesc to interrupt\n"

// codexWallFrame carries codex's stall banner on its own line, the anchored
// shape the limit detector accepts.
const codexWallFrame = "■ You've hit your usage limit\ntry again later\n"

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

func (m *Manager) pendingAccountLimitProofs() map[accountLimitProofKey]time.Time {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	proofs := make(map[accountLimitProofKey]time.Time, len(m.accountLimitProofs))
	for key, provenAt := range m.accountLimitProofs {
		proofs[key] = provenAt
	}
	return proofs
}

func accountTurnWatchKeys(m *Manager) map[string]struct{} {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	watched := make(map[string]struct{}, len(m.accountTurnWatches))
	for key := range m.accountTurnWatches {
		watched[key] = struct{}{}
	}
	return watched
}

func TestNoteAccountTurnEvidenceRecordsProofAtCaptureTime(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)
	capturedAt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	// The scoped indicator needs its absent→present edge: an armed watch, then
	// the chrome frame, proves a turn began.
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, capturedAt)
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(time.Second))

	proofs := manager.pendingAccountLimitProofs()
	require.Equal(t, map[accountLimitProofKey]time.Time{
		{agent: tmux.ProgramCodex, account: "work"}: capturedAt.Add(time.Second),
	}, proofs, "a confirmed turn is positive proof for the (agent, account) it ran under")
}

func TestNoteAccountTurnEvidenceIgnoresNonPositiveObservations(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)
	capturedAt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	t.Run("an uncaptured frame proves nothing", func(t *testing.T) {
		manager.noteAccountTurnEvidence(key, inst,
			session.Observation{Captured: false, Content: codexTurnFrame}, capturedAt)
		require.Empty(t, manager.pendingAccountLimitProofs(),
			"a failed or unknown capture must never produce proof")
		require.Empty(t, accountTurnWatchKeys(manager),
			"an uncaptured frame must not even arm the watch")
	})

	t.Run("the ambient identity is not account evidence", func(t *testing.T) {
		ambient := registerAccountInstance(t, manager, repoID, repoPath, "ambient", tmux.ProgramCodex, "")
		ambientKey := stableSessionKey(repoID, ambient)
		manager.noteAccountTurnEvidence(ambientKey, ambient,
			session.Observation{Captured: true, Content: codexArmFrame}, capturedAt)
		manager.noteAccountTurnEvidence(ambientKey, ambient,
			session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(time.Second))
		require.Empty(t, manager.pendingAccountLimitProofs(),
			"a turn under the ambient identity proves nothing the ledger keys on")
	})
}

// A frame that reads as a wall is ambiguous — in-turn chrome and a live banner
// can share a capture — so it neither proves a turn nor arms the watch that
// would let the NEXT chrome frame count as one.
func TestNoteAccountTurnEvidenceWallFrameCannotArm(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)
	capturedAt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexWallFrame}, capturedAt)
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(time.Second))
	require.Empty(t, manager.pendingAccountLimitProofs(),
		"a chrome frame after a wall frame has no armed edge to fire on — the wall frame was skipped")

	// And once the pane is unambiguous, the normal edge still works.
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, capturedAt.Add(2*time.Second))
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(3*time.Second))
	require.Contains(t, manager.pendingAccountLimitProofs(),
		accountLimitProofKey{agent: tmux.ProgramCodex, account: "work"})
}

// The watch answers for one (agent, account) signature: a handoff that changes
// the pane's account mid-run must rebuild the watch, or a turn begun under the
// old identity could be credited to the new one.
func TestNoteAccountTurnEvidenceRebuildsOnIdentityChange(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)
	capturedAt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, capturedAt)

	inst.Account = "play"
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(time.Second))
	require.Empty(t, manager.pendingAccountLimitProofs(),
		"the first chrome frame under the new identity has no armed edge — the watch was rebuilt")

	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, capturedAt.Add(2*time.Second))
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, capturedAt.Add(3*time.Second))
	require.Equal(t, map[accountLimitProofKey]time.Time{
		{agent: tmux.ProgramCodex, account: "play"}: capturedAt.Add(3 * time.Second),
	}, manager.pendingAccountLimitProofs(),
		"the turn is credited to the account the pane ran under when chrome proved it")
}

func TestSweepAccountTurnWatchesDropsGoneSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)
	capturedAt := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, capturedAt)
	require.Contains(t, accountTurnWatchKeys(manager), key)

	manager.sweepAccountTurnWatches()
	require.Contains(t, accountTurnWatchKeys(manager), key,
		"a live session's watch is kept")

	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, inst.Title))
	manager.mu.Unlock()
	manager.sweepAccountTurnWatches()
	require.Empty(t, accountTurnWatchKeys(manager),
		"a gone session's armed state must never be inherited by a successor")
}

// --- reconcile ---------------------------------------------------------------

// The reconcile folds collected proofs against durable evidence and writes a
// retraction for each account the stored record still excludes. It owns the
// expensive durable scan, so it is interval-bounded and proof-gated.
func TestReconcileAccountLimitProofsRetractsOnlyExcludedEvidence(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, repoPath := newStatusTestManager(t)
	registerAccountInstance(t, manager, repoID, repoPath, "idle-session", tmux.ProgramCodex, "other")

	// Stored walls: "work" still excluded (no reset claim), "play" walled but
	// by an observation NEWER than the proof that will arrive for it.
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-time.Hour)},
			{Agent: tmux.ProgramCodex, Account: "play", ObservedAt: base.Add(-time.Minute)},
		},
	}, nil)

	manager.pausedMu.Lock()
	manager.accountLimitProofs[accountLimitProofKey{agent: tmux.ProgramCodex, account: "work"}] = base.Add(-time.Minute)
	manager.accountLimitProofs[accountLimitProofKey{agent: tmux.ProgramCodex, account: "play"}] = base.Add(-time.Hour)
	manager.accountLimitProofs[accountLimitProofKey{agent: tmux.ProgramCodex, account: "unwalled"}] = base.Add(-time.Minute)
	manager.pausedMu.Unlock()

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Equal(t, []session.AccountLimitRetractionData{{
		Agent: tmux.ProgramCodex, Account: "work", ProvenAt: base.Add(-time.Minute),
	}}, evidence.Retractions,
		"only the account the stored record still excluded earns a tombstone: "+
			"a fresh wall outranks the older proof, and a proof with no wall writes nothing")
	require.Empty(t, manager.pendingAccountLimitProofs(), "consumed proofs do not linger")

	// The bounded cadence: a second pass inside the interval does not rescan.
	advance(time.Second)
	manager.pausedMu.Lock()
	manager.accountLimitProofs[accountLimitProofKey{agent: tmux.ProgramCodex, account: "play"}] = base
	manager.pausedMu.Unlock()
	loads := 0
	previous := loadAccountLimitEvidenceForSwap
	loadAccountLimitEvidenceForSwap = func() (accountLimitEvidence, error) {
		loads++
		return previous()
	}
	manager.reconcileAccountLimitProofs()
	require.Zero(t, loads, "the durable scan is bounded — never once per poll")
	require.NotEmpty(t, manager.pendingAccountLimitProofs(), "the pending proof waits for the interval")

	advance(accountLimitProofReconcileInterval)
	manager.reconcileAccountLimitProofs()
	require.Equal(t, 1, loads, "the scan resumes once the interval elapses")
}

// The stored record a proof answers for is not only the ledger: a live
// session's own observation list is folded in too, so a fresh wall sighted by
// a live sibling outranks an older proof just as durably.
func TestReconcileAccountLimitProofsRespectsLiveInstanceWalls(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	base := nowFunc()
	inst := registerAccountInstance(t, manager, repoID, repoPath, "parked", tmux.ProgramClaude, "work")
	// The instance's own observation is recorded at the real wall clock —
	// guaranteed after the fixed proof instant below.
	inst.SetLimitReached(nowFunc().Add(24 * time.Hour))

	stubAccountLimitEvidence(t, accountLimitEvidence{}, nil)
	key := accountLimitProofKey{agent: tmux.ProgramClaude, account: "work"}
	manager.pausedMu.Lock()
	manager.accountLimitProofs[key] = base.Add(-time.Hour)
	manager.pausedMu.Unlock()

	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Empty(t, evidence.Retractions,
		"a live session's fresh wall is the newer fact — the older proof earns no tombstone")
	require.Empty(t, manager.pendingAccountLimitProofs(),
		"the proof was evaluated against complete evidence; nothing remains to retry")
}

// An unreadable evidence record must never be mistaken for "nothing is
// walled": the reconcile retains its proofs for the next pass rather than
// acting on a partial picture.
func TestReconcileAccountLimitProofsRequeuesOnUnreadableEvidence(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	base := nowFunc()
	stubAccountLimitEvidence(t, accountLimitEvidence{}, errors.New("ledger unreadable"))

	key := accountLimitProofKey{agent: tmux.ProgramCodex, account: "work"}
	manager.pausedMu.Lock()
	manager.accountLimitProofs[key] = base
	manager.pausedMu.Unlock()

	manager.reconcileAccountLimitProofs()

	require.Equal(t, map[accountLimitProofKey]time.Time{key: base}, manager.pendingAccountLimitProofs(),
		"proofs survive a failed read — they are facts, retried against a whole evidence record")
}

// The end-to-end shape: a pane proves a turn under a named account whose
// stored wall is older than the proof; the reconcile retracts it; admission
// folds the pair and the account is a candidate again.
func TestAccountTurnProofEndToEndRestoresTheAccount(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerAccountInstance(t, manager, repoID, repoPath, "codex-session", tmux.ProgramCodex, "work")
	key := stableSessionKey(repoID, inst)

	storedWall := session.AccountLimitObservationData{
		Agent: tmux.ProgramCodex, Account: "work", ObservedAt: base.Add(-2 * time.Hour),
	}
	stubAccountLimitEvidence(t, accountLimitEvidence{
		Observations: []session.AccountLimitObservationData{storedWall},
	}, nil)

	limited, err := manager.limitedAccountsForSwap(tmux.ProgramCodex, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work", "precondition: the stored wall excludes the account")

	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexArmFrame}, base.Add(-time.Minute))
	manager.noteAccountTurnEvidence(key, inst, session.Observation{Captured: true, Content: codexTurnFrame}, base)
	advance(accountLimitProofReconcileInterval)
	manager.reconcileAccountLimitProofs()

	evidence, err := loadAccountLimitLedger()
	require.NoError(t, err)
	require.Equal(t, []session.AccountLimitRetractionData{{
		Agent: tmux.ProgramCodex, Account: "work", ProvenAt: base,
	}}, evidence.Retractions, "the turn's capture instant is the durable proof")

	// The loader used by admission sees the ledger's retraction fold the wall.
	limited, err = manager.limitedAccountsForSwap(tmux.ProgramCodex, ledgerBackedEvidence(storedWall))
	require.NoError(t, err)
	require.NotContains(t, limited, "work",
		"admission folds the pair: the proven account returns to the candidate pool")
}
