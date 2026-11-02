package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// liveOrchestrator puts a started session o1 ("planner") in place of the
// bare one orchestratedGroup binds.
func liveOrchestrator(t *testing.T, m *Manager, repoID string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{ID: "o1", Title: "planner", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "planner")] = inst
	m.mu.Unlock()
	return inst
}

func TestOrchestratorDeliveryOpensATurn(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	sent := stubBacklogSend(t, m, repoID)

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, 1, sent.count())
	require.True(t, inst.BacklogRunActive(), "the delivery opens a turn whose end can archive the session")
	require.Zero(t, inst.LifecycleView().BacklogItem)
	require.Zero(t, m.backlogSlotsUsed(repoID), "an orchestrator takes no max_running slot")
}

// An idle edge observed before a newer turn was armed does not end that turn:
// the end is fenced by the generation the edge saw, as settleRunEnd is.
func TestOrchestratorTurnEndFencedByANewerTurn(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubBacklogSend(t, m, repoID)
	archived := stubBacklogArchive(t)
	runOrchDelivery(t, m, repoID, time.Now())
	idleItemSession(t, inst)
	edge := inst.BacklogRunGeneration()

	require.True(t, inst.ArmOrchestratorTurn(time.Now()))
	m.orchestratorTurnEnded(repoID, inst, edge, nil)

	require.Empty(t, *archived, "the old edge archived a session in a newer turn")
}

func TestOrchestratorTurnEndArchivesWhenIdle(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubBacklogSend(t, m, repoID)
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	runOrchDelivery(t, m, repoID, time.Now())
	require.Empty(t, orchOf(t, repoID).Events)

	idleItemSession(t, inst)
	m.orchestratorTurnEnded(repoID, inst, inst.BacklogRunGeneration(), nil)

	require.Equal(t, []string{"o1"}, *archived)
	require.Nil(t, inst.OwedArchive())
}

// The next event restores an archived orchestrator, so af archives only one it
// can restore into its own conversation, as it does for an ask archive; any
// other would come back a fresh agent every turn. It stays live, with no note.
func TestOrchestratorTurnEndKeepsLiveWithoutItsConversation(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubBacklogSend(t, m, repoID)
	stubExactResume(t, false, "the session has no recorded conversation id")
	archived := stubBacklogArchive(t)
	runOrchDelivery(t, m, repoID, time.Now())
	threads := func() int {
		b, err := backlog.Load(repoID)
		require.NoError(t, err)
		n := 0
		for _, it := range b.Items {
			n += len(it.Thread)
		}
		return n
	}
	before := threads()

	idleItemSession(t, inst)
	m.orchestratorTurnEnded(repoID, inst, inst.BacklogRunGeneration(), nil)

	require.Empty(t, *archived)
	require.Nil(t, inst.OwedArchive())
	require.Equal(t, before, threads(), "a kept orchestrator gets no note")
}

func TestOrchestratorTurnEndKeepsLiveWithPendingEvents(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	archived := stubBacklogArchive(t)
	idleItemSession(t, inst)

	m.orchestratorTurnEnded(repoID, inst, inst.BacklogRunGeneration(), nil)

	require.Empty(t, *archived, "the review event still waits for delivery")
}

func TestOrchestratorStayKeepsLiveOnce(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubBacklogSend(t, m, repoID)
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	runOrchDelivery(t, m, repoID, time.Now())
	_, err := m.backlogService().SetOrchestratorHandoff(backlog.SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Stay: true})
	require.NoError(t, err)

	idleItemSession(t, inst)
	m.orchestratorTurnEnded(repoID, inst, inst.BacklogRunGeneration(), nil)
	require.Empty(t, *archived)
	require.False(t, orchOf(t, repoID).Stay, "the turn end uses the stay up")

	m.orchestratorTurnEnded(repoID, inst, inst.BacklogRunGeneration(), nil)
	require.Equal(t, []string{"o1"}, *archived)
}

func TestStayRefusedOutsideAnAfTurn(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	liveOrchestrator(t, m, repoID)
	_, err := m.backlogService().SetOrchestratorHandoff(backlog.SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Stay: true})
	require.ErrorIs(t, err, backlog.ErrStayNoTurn)
}

// session keeps its own copy of the idle archive reason (it does not import
// backlog), so a restart restores an orchestrator's owed archive.
func TestOrchIdleArchiveReasonMatches(t *testing.T) {
	require.Equal(t, backlog.ArchiveOrchIdle, session.OrchIdleArchive)
}

// An idle archive an earlier daemon filed and did not finish is finished by
// restart recovery.
func TestRecoveryFinishesAnOrchestratorsOwedArchive(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubBacklogSend(t, m, repoID)
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	runOrchDelivery(t, m, repoID, time.Now())
	idleItemSession(t, inst)
	inst.SetOwedArchive(backlog.ArchiveOrchIdle, time.Now())
	b, err := backlog.Load(repoID)
	require.NoError(t, err)

	m.redriveOrchestratorArchives(repoID, b)

	require.Equal(t, []string{"o1"}, *archived)
	require.Nil(t, inst.OwedArchive())
}
