package daemon

import (
	"context"
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

// A turn can end before its delivery's bookkeeping is written (orchDelivered
// retrying a busy backlog): the turn end sees the events still being sent and
// keeps the session live. Once the delivery is recorded, the decision is made
// again, so the orchestrator is not left live with nothing pending (Codex P2
// on #6614).
func TestOrchestratorTurnEndedBeforeItsDeliveryIsRecordedArchivesAfter(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	_, d, ok := m.beginOrchDelivery(repoID, 2, time.Now())
	require.True(t, ok)
	require.True(t, inst.ArmOrchestratorTurn(time.Now()))
	gen := inst.BacklogRunGeneration()
	idleItemSession(t, inst)

	m.orchestratorTurnEnded(repoID, inst, gen, nil)
	require.Empty(t, *archived, "the delivery is still being sent")

	m.orchDelivered(nil, repoID, d)
	require.Equal(t, []string{"o1"}, *archived)
}

// The revisit is fenced like the turn end: a newer turn that a delivery opened
// before the first one was recorded ends on its own edge.
func TestOrchestratorTurnRevisitFencedByANewerTurn(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst := liveOrchestrator(t, m, repoID)
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	_, d, ok := m.beginOrchDelivery(repoID, 2, time.Now())
	require.True(t, ok)
	require.True(t, inst.ArmOrchestratorTurn(time.Now()))
	gen := inst.BacklogRunGeneration()
	idleItemSession(t, inst)
	m.orchestratorTurnEnded(repoID, inst, gen, nil)

	require.True(t, inst.ArmOrchestratorTurn(time.Now()))
	m.orchDelivered(nil, repoID, d)

	require.Empty(t, *archived, "the revisit archived a session in a newer turn")
}

// A replacement's first turn (opened at its creation) can end before af binds
// it, while it orchestrates no group yet: the turn end has nothing to decide.
// Once it is bound, with the events its first prompt carried delivered, the
// decision is made again and the idle replacement is archived (Codex P2 on
// #6614).
func TestOrchestratorReplacementWhoseFirstTurnEndedBeforeTheBindArchivesAfter(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	stubExactResume(t, true, "")
	archived := stubBacklogArchive(t)
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, _ context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		require.True(t, req.OrchestratorTurn, "a replacement starts with its first turn open")
		inst, err := session.NewInstance(session.InstanceOptions{ID: "n1", Title: "effort B-2 Effort", Path: t.TempDir(),
			Program: "claude", OrchestratorTurn: true})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStartedForTest(true)
		m.mu.Lock()
		m.instances[daemonInstanceKey(repoID, "effort B-2 Effort")] = inst
		m.mu.Unlock()
		// The first turn ends now, before the bind.
		gen := inst.BacklogRunGeneration()
		idleItemSession(t, inst)
		m.orchestratorTurnEnded(repoID, inst, gen, nil)
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort", CreatedAt: time.Now()}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())

	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Empty(t, o.Events, "the first prompt carried the events")
	require.Equal(t, []string{"n1"}, *archived)
}
