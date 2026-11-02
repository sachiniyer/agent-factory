package daemon

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func TestBacklogSessionState(t *testing.T) {
	for _, c := range []struct {
		v    session.LifecycleView
		want string
	}{
		{session.LifecycleView{Liveness: session.LiveRunning}, backlog.SessionLive},
		{session.LifecycleView{Liveness: session.LiveReady}, backlog.SessionLive},
		{session.LifecycleView{Liveness: session.LiveArchived}, backlog.SessionArchived},
		{session.LifecycleView{Liveness: session.LiveDead}, backlog.SessionDead},
		{session.LifecycleView{Liveness: session.LiveLost}, backlog.SessionLost},
		{session.LifecycleView{Liveness: session.LiveLimitReached}, backlog.SessionLimited},
		{session.LifecycleView{Liveness: session.LiveRunning, UserKilled: true}, backlog.SessionGone},
		{session.LifecycleView{Liveness: session.LiveRunning, InFlightOp: session.OpKilling}, backlog.SessionGone},
	} {
		if got := backlogSessionState(c.v); got != c.want {
			t.Errorf("%+v: %q, want %q", c.v, got, c.want)
		}
	}
}

func orchestratorEvents(ch <-chan agentproto.Event) []agentproto.Event {
	var out []agentproto.Event
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventBacklogOrchestratorBound || ev.Type == agentproto.EventBacklogOrchestratorReplaced {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func TestBacklogHostSessionIsScopedToTheProject(t *testing.T) {
	cs, repoPath := newBacklogTestServer(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	m := cs.manager
	m.instances = map[string]*session.Instance{
		daemonInstanceKey(repo.ID, "planner"): {ID: "o1", Title: "planner", Program: "claude"},
		daemonInstanceKey(repo.ID, "worker"):  {ID: "w1", Title: "worker", BacklogItem: 2},
		daemonInstanceKey("other-repo", "x"):  {ID: "elsewhere", Title: "x"},
	}
	h := backlogHost{m: m}

	info, ok := h.Session(repo.ID, "o1")
	require.True(t, ok)
	require.Equal(t, backlog.SessionInfo{Title: "planner", Program: "claude", State: backlog.SessionLive}, info)
	info, ok = h.Session(repo.ID, "w1")
	require.True(t, ok)
	require.Equal(t, 2, info.Item)
	_, ok = h.Session(repo.ID, "elsewhere")
	require.False(t, ok, "a session of another project is not this project's")
	_, ok = h.Session("other-repo", "o1")
	require.False(t, ok)
}

func TestSetBacklogOrchestratorPublishesBoundAndReplaced(t *testing.T) {
	cs, repoPath := newBacklogTestServer(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	m := cs.manager
	m.instances = map[string]*session.Instance{
		daemonInstanceKey(repo.ID, "planner"): {ID: "o1", Title: "planner"},
		daemonInstanceKey(repo.ID, "next"):    {ID: "o2", Title: "next"},
		daemonInstanceKey("other-repo", "x"):  {ID: "elsewhere", Title: "x"},
	}
	sub, ch, _ := m.events.subscribeWithOverflow()
	defer m.events.unsubscribe(sub)

	code, env := postProposalRoute(t, cs, "AddBacklogItems", AddBacklogItemsRequest{RepoPath: repoPath,
		Items: []backlog.ItemInput{{Key: "g", Title: "G"}, {Key: "a", Title: "A", Parent: backlog.Ref{Key: "g"}}}})
	require.Equal(t, http.StatusOK, code, env)

	code, env = postProposalRoute(t, cs, "SetBacklogOrchestrator", SetBacklogOrchestratorRequest{RepoPath: repoPath, ID: 1, SessionID: "elsewhere"})
	require.NotEqual(t, http.StatusOK, code, env)
	require.Empty(t, orchestratorEvents(ch))

	code, env = postProposalRoute(t, cs, "SetBacklogOrchestrator", SetBacklogOrchestratorRequest{RepoPath: repoPath, ID: 1, SessionID: "o1"})
	require.Equal(t, http.StatusOK, code, env)
	evs := orchestratorEvents(ch)
	require.Len(t, evs, 1)
	require.Equal(t, agentproto.EventBacklogOrchestratorBound, evs[0].Type)
	var p backlog.OrchestratorEvent
	require.NoError(t, json.Unmarshal(evs[0].Data, &p))
	require.Equal(t, backlog.OrchestratorEvent{RepoID: repo.ID, ID: 1, SessionID: "o1", Title: "planner"}, p)

	code, env = postProposalRoute(t, cs, "SetBacklogOrchestrator", SetBacklogOrchestratorRequest{RepoPath: repoPath, ID: 1, SessionID: "o1"})
	require.Equal(t, http.StatusOK, code, env)
	require.Empty(t, orchestratorEvents(ch), "a repeat bind publishes nothing")

	code, env = postProposalRoute(t, cs, "SetBacklogOrchestrator", SetBacklogOrchestratorRequest{RepoPath: repoPath, ID: 1, SessionID: "o2"})
	require.NotEqual(t, http.StatusOK, code, env)

	code, env = postProposalRoute(t, cs, "SetBacklogOrchestrator", SetBacklogOrchestratorRequest{RepoPath: repoPath, ID: 1, SessionID: "o2", Replace: true})
	require.Equal(t, http.StatusOK, code, env)
	evs = orchestratorEvents(ch)
	require.Len(t, evs, 1)
	require.Equal(t, agentproto.EventBacklogOrchestratorReplaced, evs[0].Type)
	require.NoError(t, json.Unmarshal(evs[0].Data, &p))
	require.Equal(t, backlog.OrchestratorEvent{RepoID: repo.ID, ID: 1, SessionID: "o2", Title: "next", Previous: "o1"}, p)
}
