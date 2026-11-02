package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func openOrch(m *Manager, repoID string, id int) (backlog.OpenOrchestratorResponse, error) {
	return m.OpenBacklogOrchestrator(backlog.OpenOrchestratorRequest{RepoID: repoID, ID: id})
}

func stubOrchCreate(t *testing.T) *[]CreateSessionRequest {
	t.Helper()
	var creates []CreateSessionRequest
	prev := createBacklogSession
	createBacklogSession = func(_ *Manager, _ context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		creates = append(creates, req)
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort"}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })
	return &creates
}

// stubOrchRestore stands in for an archived o1 that af restores.
func stubOrchRestore(t *testing.T) *[]string {
	t.Helper()
	prevNeeds, prevRestore := backlogNeedsRestore, restoreBacklogSession
	backlogNeedsRestore = func(s itemSession) bool { return s.instance.ID == "o1" }
	var restored []string
	restoreBacklogSession = func(_ *Manager, req RestoreSessionRequest) (session.InstanceData, error) {
		restored = append(restored, req.ID)
		return session.InstanceData{ID: req.ID}, nil
	}
	t.Cleanup(func() { backlogNeedsRestore, restoreBacklogSession = prevNeeds, prevRestore })
	return &restored
}

func dropPlanner(m *Manager, repoID string) {
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
}

func TestOpenOrchestratorNamesALiveSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	creates := stubOrchCreate(t)

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OpenOrchestratorResponse{SessionID: "o1", Title: "planner"}, out)
	require.Empty(t, *creates)
	require.Len(t, orchOf(t, repoID).Events, 1, "opening sends no events")
}

// TestOpenOrchestratorRestoresAnArchivedSession: a session that comes back
// without its conversation gets the start prompt, without the events, and no
// turn, so its end does not archive the session the user is opening. The
// confirmed send clears the unstarted mark.
func TestOpenOrchestratorRestoresAnArchivedSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	restored := stubOrchRestore(t)
	stubExactResume(t, false, "no conversation id")

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OrchOpenRestored, out.Change)
	require.Equal(t, "o1", out.SessionID)
	require.Equal(t, []string{"o1"}, *restored)
	require.Equal(t, 1, sent.count())
	require.Contains(t, sent.reqs[0].Prompt, "restarted without its earlier conversation")
	require.NotContains(t, sent.reqs[0].Prompt, "Events in", "events wait for the next delivery")
	o := orchOf(t, repoID)
	require.Empty(t, o.Unstarted, "the start prompt was confirmed")
	require.Len(t, o.Events, 1)
	m.mu.Lock()
	inst := m.instances[daemonInstanceKey(repoID, "planner")]
	m.mu.Unlock()
	require.False(t, inst.BacklogRunActive(), "no turn: the user opens it")
}

// TestOpenOrchestratorHeldStartPromptStaysPending: a start prompt held by a
// usage limit leaves the session marked unstarted, so the next delivery sends
// the start prompt in full.
func TestOpenOrchestratorHeldStartPromptStaysPending(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	sent.err = errBacklogTargetLimited
	stubOrchRestore(t)
	stubExactResume(t, false, "no conversation id")

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OrchOpenRestored, out.Change)
	require.Equal(t, backlog.OrchUnstartedRestarted, orchOf(t, repoID).Unstarted)
}

func TestOpenOrchestratorRestoresAResumableSessionWithoutAPrompt(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	restored := stubOrchRestore(t)
	stubExactResume(t, true, "")

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OrchOpenRestored, out.Change)
	require.Equal(t, []string{"o1"}, *restored)
	require.Zero(t, sent.count(), "its conversation resumes")
	require.Empty(t, orchOf(t, repoID).Unstarted)
}

func TestOpenOrchestratorReplacesAGoneSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	dropPlanner(m, repoID)
	var creates []CreateSessionRequest
	var replacing *backlog.OrchReplacing
	prev := createBacklogSession
	createBacklogSession = func(_ *Manager, _ context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		creates = append(creates, req)
		// Read without require: the create runs off the test goroutine.
		if b, err := backlog.Load(repoID); err == nil && b.Item(2) != nil && b.Item(2).Orchestrator != nil {
			replacing = b.Item(2).Orchestrator.Replacing
		}
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort"}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })
	sub, ch, _ := m.events.subscribeWithOverflow()
	defer m.events.unsubscribe(sub)

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OpenOrchestratorResponse{SessionID: "n1", Title: "effort B-2 Effort", Change: backlog.OrchOpenReplaced}, out)
	require.Len(t, creates, 1)
	c := creates[0]
	require.Equal(t, "claude", c.Program)
	require.False(t, c.OrchestratorTurn, "the user opens it, so its turn's end does not archive it")
	require.Contains(t, c.Prompt, "af started this session to replace it")
	require.NotNil(t, replacing, "the replacement is recorded before the create")
	require.Equal(t, "effort B-2 Effort", replacing.TitleBase)
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Nil(t, o.Replacing)
	require.Empty(t, o.Unstarted)
	require.Len(t, o.Events, 1, "pending events stay for the next delivery")
	evs := orchestratorEvents(ch)
	require.Len(t, evs, 1)
	require.Equal(t, agentproto.EventBacklogOrchestratorReplaced, evs[0].Type)
}

// TestOpenOrchestratorAdoptsAReplacementStartedBeforeARestart: a replacement
// the deliverer started before a restart could bind it is bound, not joined by
// a second.
func TestOpenOrchestratorAdoptsAReplacementStartedBeforeARestart(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	svc := m.backlogService()
	since := time.Now().Add(-time.Minute)
	_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		return []int{2}, b.BeginOrchReplace(2, "o1", "effort B-2 Effort", since)
	})
	require.NoError(t, err)
	dropPlanner(m, repoID)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "effort B-2 Effort")] = &session.Instance{ID: "n1", Title: "effort B-2 Effort", Program: "claude", CreatedAt: since.Add(time.Second)}
	m.mu.Unlock()
	creates := stubOrchCreate(t)

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OpenOrchestratorResponse{SessionID: "n1", Title: "effort B-2 Effort", Change: backlog.OrchOpenReplaced}, out)
	require.Empty(t, *creates, "no second replacement")
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Nil(t, o.Replacing)
	require.Equal(t, backlog.OrchUnstartedReplaced, o.Unstarted, "it may not have had its start prompt")
}

func TestOpenOrchestratorStartsOneForAnUnboundGroup(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	_, err := m.backlogService().SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, Clear: true})
	require.NoError(t, err)
	creates := stubOrchCreate(t)
	sub, ch, _ := m.events.subscribeWithOverflow()
	defer m.events.unsubscribe(sub)

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OrchOpenStarted, out.Change)
	require.Len(t, *creates, 1)
	require.Contains(t, (*creates)[0].Prompt, "started this session from the web")
	require.False(t, (*creates)[0].OrchestratorTurn)
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Empty(t, o.Unstarted)
	evs := orchestratorEvents(ch)
	require.Len(t, evs, 1)
	require.Equal(t, agentproto.EventBacklogOrchestratorBound, evs[0].Type)
}

// TestOpenOrchestratorBindsANewSessionWhoseFirstPromptFailedUnstarted: a new
// session whose first prompt was proven not delivered is bound, marked
// unstarted with its own kind, so a delivery sends it the start prompt.
func TestOpenOrchestratorBindsANewSessionWhoseFirstPromptFailedUnstarted(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	_, err := m.backlogService().SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, Clear: true})
	require.NoError(t, err)
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort", LastPromptDeliveryStatus: session.PromptNotDelivered}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	out, err := openOrch(m, repoID, 2)
	require.NoError(t, err)
	require.Equal(t, backlog.OrchOpenStarted, out.Change)
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Equal(t, backlog.OrchUnstartedNew, o.Unstarted)
}

func TestOpenOrchestratorRefusesAnItemThatIsNotAGroup(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	creates := stubOrchCreate(t)

	_, err := openOrch(m, repoID, 3)
	require.ErrorContains(t, err, "not a top-level parent")
	require.Empty(t, *creates)
}

func TestOpenOrchestratorRefusedDuringADelivery(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	require.True(t, m.backlog.beginOrchestrate(repoID, 2))
	defer m.backlog.endOrchestrate(repoID, 2)

	_, err := openOrch(m, repoID, 2)
	require.EqualError(t, err, "af is already working on the effort session of B-2; try again in a moment")
}

func TestOpenOrchestratorKillsASessionItCouldNotBind(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	svc := m.backlogService()
	_, err := svc.SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, Clear: true})
	require.NoError(t, err)
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
			return []int{2}, b.BindOrchestrator(2, backlog.Orchestrator{SessionID: "other", Title: "other"}, svc.Now())
		})
		require.NoError(t, err)
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort"}, nil
	}
	prevKill := killOrchestratorSession
	var killed []KillSessionRequest
	killOrchestratorSession = func(_ *Manager, req KillSessionRequest) (session.InstanceData, error) {
		killed = append(killed, req)
		return session.InstanceData{ID: req.ID}, nil
	}
	t.Cleanup(func() { createBacklogSession, killOrchestratorSession = prev, prevKill })

	_, err = openOrch(m, repoID, 2)
	require.ErrorContains(t, err, `"other" started orchestrating B-2 meanwhile`)
	require.Equal(t, []KillSessionRequest{{ID: "n1", Title: "effort B-2 Effort", RepoID: repoID}}, killed)
	require.Equal(t, "other", orchOf(t, repoID).SessionID)
}
