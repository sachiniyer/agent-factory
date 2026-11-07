package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestCommittedSwapAccountWall_IgnoresTheSubjectRowsInheritedWall is the
// #6915-review wedge: a committed replacement parked at the incoming wall by
// an earlier refusal must not count its own live wall on the next
// re-admission. The live marker has no ordering clock — a no-reset window
// never expires and no retraction defeats it — so counting it makes the
// refusal permanent even after another runtime proves the account usable and
// retracts the stored observations. The retained observation is the fact the
// proof orders against; the live wall is only its projection.
func TestCommittedSwapAccountWall_IgnoresTheSubjectRowsInheritedWall(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "limited", session.NewFakeBackend(), true, session.Running)
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	inst.ReconcileAccountHandoffSnapshot("work", "", true, &session.AccountSwapData{To: "work"})
	inst.SetInFlightOpForTest(session.OpRespawning)
	// Park at the incoming wall the way an earlier refusal did: no parsed
	// reset — the window that can never expire on its own.
	require.NoError(t, inst.ParkAutomaticAccountSwapAtLimit(time.Time{}, ""))
	swap := committedAccountSwap(inst)
	require.NotNil(t, swap, "the parked row carries a committed pending swap")

	// Sanity: the row really is parked at the incoming identity's wall.
	agent, account, limited := inst.LimitIdentity()
	require.True(t, limited)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", account)

	// A later runtime proved the account usable: the retraction defeats every
	// stored observation of this wall, including the park's own durable
	// record. ProvenAt must postdate the observation's ObservedAt — and must
	// be a timestamp production's usageReadingProof accepts, i.e. a record
	// time not ahead of the read: one nanosecond after the sighting is both
	// strictly newer than the wall and already in the past.
	var observedAt time.Time
	for _, observation := range inst.AccountLimitObservations() {
		if observation.Agent == tmux.ProgramClaude && observation.Account == "work" && observation.ResetAt.IsZero() {
			observedAt = observation.ObservedAt
		}
	}
	require.False(t, observedAt.IsZero(), "the park must have recorded the incoming wall's sighting")
	loadProof := func() (accountLimitEvidence, error) {
		return accountLimitEvidence{Retractions: []session.AccountLimitRetractionData{
			{Agent: tmux.ProgramClaude, Account: "work", ProvenAt: observedAt.Add(time.Nanosecond)},
		}}, nil
	}
	_, _, _, walled, err := manager.committedSwapAccountWall(swap, loadProof)
	require.NoError(t, err)
	require.False(t, walled,
		"a committed replacement must not be walled by its own inherited live wall once the evidence is retracted")

	// Without the proof, the row's own stored observation — fail-closed
	// evidence a no-reset wall stands on — keeps it walled. The self-skip
	// removes the unorderable projection, not the durable fact.
	_, _, _, walled, err = manager.committedSwapAccountWall(swap, ledgerBackedEvidence())
	require.NoError(t, err)
	require.True(t, walled,
		"absent a retraction, the parked row's own durable observation still excludes the committed identity")
}

// TestParkCommittedSwapAtIncomingWall_FilesWallUnderCommittedNamespace is the
// #6915-review attribution fix at the daemon boundary: a cross-agent manual
// handoff parked BEFORE its replacement spawned must file the incoming
// account's wall under the committed namespace (codex), not the outgoing
// agent the pane still runs (claude). Pane-derived attribution would exclude
// an unrelated claude account and defeat incomingManualReset's namespace
// match.
func TestParkCommittedSwapAtIncomingWall_FilesWallUnderCommittedNamespace(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "limited", session.NewFakeBackend(), true, session.Running)
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	swap := &autoAccountSwap{manual: true, agent: tmux.ProgramClaude, accountAgent: tmux.ProgramCodex, to: "work", alreadySet: true}
	inst.ReconcileAccountHandoffSnapshot("work", swap.accountAgent, false, &session.AccountSwapData{
		Manual: true, To: "work", AccountAgent: tmux.ProgramCodex,
		MissionDeliveryStatus: session.PromptCouldNotConfirm,
	})
	inst.SetInFlightOpForTest(session.OpRespawning)

	reset := time.Now().Add(2 * time.Hour)
	err := manager.parkCommittedSwapAtIncomingWall(repoID, daemonInstanceKey(repoID, inst.Title), inst.Title, inst, swap, reset, time.Now().UTC(), "")
	require.Error(t, err, "the park reports the launch refusal")

	agent, account, limited := inst.LimitIdentity()
	require.True(t, limited)
	require.Equal(t, tmux.ProgramCodex, agent,
		"the incoming wall must file under the committed namespace, not the pane's outgoing agent")
	require.Equal(t, "work", account)
	requireAccountLimitObservation(t, inst.AccountLimitObservations(), tmux.ProgramCodex, "work", reset)

	// The durable row the park's settlement wrote carries the same
	// attribution a restart must reload.
	record := recordFor(t, repoID, inst.Title)
	require.NotNil(t, record)
	require.Equal(t, tmux.ProgramCodex, record.LimitAgent)
	require.Equal(t, "work", record.LimitAccount)
}

// TestResumeFromLimit_CommittedSwapRetry_ProofBetweenParksFreesTheSession is
// the #6880 retained-observation wedge driven end-to-end through
// resumeFromLimitOutcome → resumeFromLimitLockedOutcome: a committed swap's
// refused retry re-parks on the incoming wall, and a usage proof recorded
// BETWEEN two refused parks must free the session. Master's park recorded the
// copy as a fresh sighting every refusal, so the second park re-stamped
// ObservedAt past the proof's ProvenAt and the copy could never be retracted —
// the session stayed walled by its own observation forever, and the committed
// identity stayed excluded from every admission.
//
// The evidence loader is stubbed the way the durable reconcile works in
// production: the proof lands between the two parks but only becomes visible
// to the re-admission when the next reconcile lands, one retry later.
func TestResumeFromLimit_CommittedSwapRetry_ProofBetweenParksFreesTheSession(t *testing.T) {
	manager, repoID, inst, backend := wedgedCommittedSwapFixture(t, false)

	// The committed transaction targets the claude "personal" identity. The
	// durable ledger's wall on it was sighted long before this retry — the
	// instant every copy of it must stay anchored to.
	wallSightedAt := time.Now().Add(-time.Hour)
	seeded := session.AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "personal", ObservedAt: wallSightedAt,
	}
	previous := loadAccountLimitEvidenceForSwap
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = previous })
	staleEvidence := func() (accountLimitEvidence, error) {
		return accountLimitEvidence{Observations: []session.AccountLimitObservationData{seeded}}, nil
	}
	loadAccountLimitEvidenceForSwap = staleEvidence
	req := ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}

	// First refused retry: the pre-spawn re-admission walls on the seeded
	// observation and parks a copy of it on the subject row.
	outcome, err := manager.resumeFromLimitOutcome(req)
	require.Error(t, err)
	require.Equal(t, resumeNotPerformed, outcome)
	require.True(t, inst.LimitReached(), "the refused retry must leave the session parked at the incoming wall")

	// A sibling runtime then proves the account usable. The proof is not
	// future-dated — production's usageReadingProof only accepts record times
	// at or before the read — it sits between the two parks.
	provenAt := time.Now()
	time.Sleep(5 * time.Millisecond)

	// Second refused retry on the same stale evidence: the reconcile has not
	// landed the retraction yet, so the wall stands and the row re-parks.
	outcome, err = manager.resumeFromLimitOutcome(req)
	require.Error(t, err)
	require.Equal(t, resumeNotPerformed, outcome)

	// The reconcile lands the retraction. The next retry's re-admission must
	// order the proof against the parked copy's SOURCE instant — the wall's
	// own event time — not the instant this session last refused on it, so
	// the proof defeats every observation and the launch goes through.
	loadAccountLimitEvidenceForSwap = func() (accountLimitEvidence, error) {
		return accountLimitEvidence{
			Observations: []session.AccountLimitObservationData{seeded},
			Retractions: []session.AccountLimitRetractionData{
				{Agent: tmux.ProgramClaude, Account: "personal", ProvenAt: provenAt},
			},
		}, nil
	}
	_, respawnBefore, _ := backend.snapshot()
	outcome, err = manager.resumeFromLimitOutcome(req)
	require.NoError(t, err,
		"a proof dated between two refused parks must free the session: %v", err)
	require.Equal(t, resumePerformed, outcome)

	_, respawnCalls, prompts := backend.snapshot()
	require.Greater(t, respawnCalls, respawnBefore, "the freed retry must launch the committed replacement")
	require.NotEmpty(t, prompts, "the recovered swap must deliver its committed mission")
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending, "the completed retry must retire the committed swap")

	// The parked copy never postdated its source observation — the property
	// that let the proof reach it.
	var copySightedAt time.Time
	for _, observation := range inst.AccountLimitObservations() {
		if observation.Agent == tmux.ProgramClaude && observation.Account == "personal" && observation.ResetAt.IsZero() {
			copySightedAt = observation.ObservedAt
		}
	}
	require.True(t, copySightedAt.Equal(wallSightedAt),
		"a park's copy inherits its source observation's instant: got %v, want %v", copySightedAt, wallSightedAt)
}
