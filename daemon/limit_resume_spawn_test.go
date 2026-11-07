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
	// record. ProvenAt must postdate the observation's ObservedAt, which the
	// park stamped with the sighting instant.
	loadProof := func() (accountLimitEvidence, error) {
		return accountLimitEvidence{Retractions: []session.AccountLimitRetractionData{
			{Agent: tmux.ProgramClaude, Account: "work", ProvenAt: time.Now().Add(time.Hour)},
		}}, nil
	}
	_, _, walled, err := manager.committedSwapAccountWall(swap, loadProof)
	require.NoError(t, err)
	require.False(t, walled,
		"a committed replacement must not be walled by its own inherited live wall once the evidence is retracted")

	// Without the proof, the row's own stored observation — fail-closed
	// evidence a no-reset wall stands on — keeps it walled. The self-skip
	// removes the unorderable projection, not the durable fact.
	_, _, walled, err = manager.committedSwapAccountWall(swap, ledgerBackedEvidence())
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
	err := manager.parkCommittedSwapAtIncomingWall(repoID, daemonInstanceKey(repoID, inst.Title), inst.Title, inst, swap, reset, "")
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
