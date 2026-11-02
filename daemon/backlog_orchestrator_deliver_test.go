package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// orchestratedGroup adds group B-2 with child B-3 to a registered project,
// binds session o1 ("planner") to it, and queues a review event on B-3.
func orchestratedGroup(t *testing.T) (*Manager, string) {
	t.Helper()
	m, repoID := runnerTestManager(t)
	addOrchGroup(t, m, repoID)
	m.backlog.markRecovered(repoID)
	return m, repoID
}

func addOrchGroup(t *testing.T, m *Manager, repoID string) {
	t.Helper()
	svc := m.backlogService()
	_, err := svc.Add(backlog.AddRequest{RepoID: repoID, Items: []backlog.ItemInput{
		{Key: "g", Title: "Effort"}, {Key: "c", Title: "Child", Parent: backlog.Ref{Key: "g"}}}})
	require.NoError(t, err)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "planner")] = &session.Instance{ID: "o1", Title: "planner", Program: "claude"}
	m.mu.Unlock()
	_, err = svc.SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, SessionID: "o1"})
	require.NoError(t, err)
	queueOrch(t, m, repoID, backlog.OrchEvent{Kind: backlog.EventReview, Item: 3})
}

func queueOrch(t *testing.T, m *Manager, repoID string, ev backlog.OrchEvent) {
	t.Helper()
	svc := m.backlogService()
	_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		return []int{b.QueueOrchEvent(ev, svc.Now())}, nil
	})
	require.NoError(t, err)
}

func runOrchDelivery(t *testing.T, m *Manager, repoID string, now time.Time) {
	t.Helper()
	b, err := backlog.Load(repoID)
	require.NoError(t, err)
	m.deliverOrchestrators(repoID, b, now)
	m.backgroundMutationWG.Wait()
}

func orchOf(t *testing.T, repoID string) *backlog.Orchestrator {
	t.Helper()
	return loadItem(t, repoID, 2).Orchestrator
}

func TestOrchestratorDeliveryToALiveSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	queueOrch(t, m, repoID, backlog.OrchEvent{Kind: backlog.EventStalled, Item: 3})

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, 1, sent.count(), "pending events go in one prompt")
	p := sent.reqs[0]
	require.Equal(t, "o1", p.ID)
	require.Contains(t, p.Prompt, "B-3 Child: ready for review.")
	require.Contains(t, p.Prompt, "B-3 Child: stalled.")
	o := orchOf(t, repoID)
	require.Empty(t, o.Events)
	require.Empty(t, o.Failures)

	runOrchDelivery(t, m, repoID, time.Now())
	require.Equal(t, 1, sent.count(), "nothing left to send")
}

func TestOrchestratorDeliveryWaitsForRecovery(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	m.backlog.mu.Lock()
	delete(m.backlog.recovered, repoID)
	m.backlog.mu.Unlock()
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	created := false
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		created = true
		return session.InstanceData{}, errors.New("no")
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())
	require.False(t, created, "replaced a session before recovery saw the project's sessions")
	require.Zero(t, sent.count())
	require.Len(t, orchOf(t, repoID).Events, 1)
}

func TestOrchestratorDeliveryRestoresAnArchivedSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	prevNeeds, prevRestore := backlogNeedsRestore, restoreBacklogSession
	backlogNeedsRestore = func(s itemSession) bool { return s.instance.ID == "o1" }
	var restored []string
	sentAtRestore := -1
	restoreBacklogSession = func(_ *Manager, req RestoreSessionRequest) (session.InstanceData, error) {
		restored = append(restored, req.ID)
		sentAtRestore = sent.count()
		return session.InstanceData{ID: req.ID}, nil
	}
	t.Cleanup(func() { backlogNeedsRestore, restoreBacklogSession = prevNeeds, prevRestore })

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, []string{"o1"}, restored)
	require.Zero(t, sentAtRestore, "restore comes before the send")
	require.Equal(t, 1, sent.count())
	require.Contains(t, sent.reqs[0].Prompt, "restarted without its earlier conversation",
		"a session back without its conversation gets the start prompt")
	require.Contains(t, sent.reqs[0].Prompt, "ready for review")
	require.Empty(t, orchOf(t, repoID).Events)
}

func TestOrchestratorDeliveryReplacesAGoneSession(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	_, err := m.backlogService().SetOrchestratorHandoff(backlog.SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Handoff: "Merge B-3 next."})
	require.NoError(t, err)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	sent := stubBacklogSend(t, m, repoID)
	var create CreateSessionRequest
	prev := createBacklogSession
	createBacklogSession = func(_ *Manager, _ context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		create = req
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort"}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })
	sub, ch, _ := m.events.subscribeWithOverflow()
	defer m.events.unsubscribe(sub)

	runOrchDelivery(t, m, repoID, time.Now())

	require.Zero(t, sent.count(), "the events ride the new session's first prompt")
	require.Equal(t, "effort B-2 Effort", create.TitleBase)
	require.Equal(t, "claude", create.Program)
	require.Zero(t, create.BacklogItem, "an orchestrator is not an item session")
	for _, want := range []string{"af started this session to replace it", "Merge B-3 next.", "B-3 Child: ready for review."} {
		require.Contains(t, create.Prompt, want)
	}
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Equal(t, "effort B-2 Effort", o.Title)
	require.Equal(t, "Merge B-3 next.", o.Handoff)
	require.Empty(t, o.Events)

	evs := orchestratorEvents(ch)
	require.Len(t, evs, 1)
	require.Equal(t, agentproto.EventBacklogOrchestratorReplaced, evs[0].Type)
	var p backlog.OrchestratorEvent
	require.NoError(t, json.Unmarshal(evs[0].Data, &p))
	require.Equal(t, backlog.OrchestratorEvent{RepoID: repoID, ID: 2, SessionID: "n1", Title: "effort B-2 Effort", Previous: "o1"}, p)
}

func TestOrchestratorDeliveryHeldWhileAttached(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	m.PauseStatusPollFor(repoID, "planner", "o1", "")

	runOrchDelivery(t, m, repoID, time.Now())

	require.Zero(t, sent.count())
	o := orchOf(t, repoID)
	require.Len(t, o.Events, 1)
	require.Empty(t, o.Failures, "a hold is not a failure")
}

func TestOrchestratorDeliveryBacksOffThenEscalates(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	sent.err = errors.New("pane gone")

	now := time.Now()
	runOrchDelivery(t, m, repoID, now)
	require.Len(t, orchOf(t, repoID).Failures, 1)

	runOrchDelivery(t, m, repoID, now)
	require.Equal(t, 1, sent.count(), "no retry inside the back-off")

	for i := 0; i < backlog.OrchMaxFailures-1; i++ {
		now = now.Add(time.Duration(backlog.OrchMaxFailures) * backlog.OrchRetryStep)
		runOrchDelivery(t, m, repoID, now)
	}
	require.Equal(t, backlog.OrchMaxFailures, sent.count())
	g := loadItem(t, repoID, 2)
	q := g.OpenQuestion()
	require.NotNil(t, q, "the third failure asks the user")
	require.Equal(t, backlog.AuthorAF, q.Author)
	require.True(t, strings.Contains(q.Text, "pane gone"), q.Text)
	require.Len(t, g.Orchestrator.Events, 1, "the events wait")

	runOrchDelivery(t, m, repoID, now.Add(time.Hour))
	require.Equal(t, backlog.OrchMaxFailures, sent.count(), "no delivery while escalated")

	sent.err = nil
	_, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 2, Kind: backlog.KindAnswer, Text: "fixed"})
	require.NoError(t, err)
	runOrchDelivery(t, m, repoID, now.Add(time.Hour))
	require.Equal(t, backlog.OrchMaxFailures+1, sent.count(), "the answer resumes delivery")
	require.Empty(t, orchOf(t, repoID).Events)
}

func TestOrchestratorDeliveryForABacklogMadeAfterRecovery(t *testing.T) {
	m, repoID := runnerTestManager(t)
	path, err := backlog.BacklogPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".aside"))
	m.recoverBacklogStarts(nil)
	require.NoError(t, os.Rename(path+".aside", path))
	addOrchGroup(t, m, repoID)
	sent := stubBacklogSend(t, m, repoID)

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, 1, sent.count(), "a backlog recovery never saw has nothing to recover")
	require.Empty(t, orchOf(t, repoID).Events)
}

func TestOrchestratorReplacementRefusedIsKilled(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.instances[daemonInstanceKey(repoID, "other")] = &session.Instance{ID: "o2", Title: "other"}
	m.mu.Unlock()
	prevCreate, prevKill := createBacklogSession, killOrchestratorSession
	createBacklogSession = func(m *Manager, _ context.Context, _ CreateSessionRequest) (session.InstanceData, error) {
		// The user takes the group over while the replacement starts.
		if _, err := m.backlogService().SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, SessionID: "o2", Replace: true}); err != nil {
			return session.InstanceData{}, err
		}
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort"}, nil
	}
	var killed []KillSessionRequest
	killOrchestratorSession = func(_ *Manager, req KillSessionRequest) (session.InstanceData, error) {
		killed = append(killed, req)
		return session.InstanceData{ID: req.ID, Title: req.Title}, nil
	}
	t.Cleanup(func() { createBacklogSession, killOrchestratorSession = prevCreate, prevKill })

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, []KillSessionRequest{{ID: "n1", Title: "effort B-2 Effort", RepoID: repoID}}, killed)
	o := orchOf(t, repoID)
	require.Equal(t, "o2", o.SessionID, "the user's bind stands")
	require.Len(t, o.Events, 1, "the events wait for the session bound now")
}

func TestOrchestratorSendThatRacesAnArchiveIsHeld(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	inst, ok := m.projectSession(repoID, "o1")
	require.True(t, ok)
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error {
		inst.instance.SetArchived()
		return errors.New("pane closed")
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })

	runOrchDelivery(t, m, repoID, time.Now())

	o := orchOf(t, repoID)
	require.Len(t, o.Events, 1)
	require.Empty(t, o.Failures, "an archive that raced the send is not a failure")
}

func TestOrchestratorFailureDuringShutdownIsNotCounted(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	stop := make(chan struct{})
	prev := createBacklogSession
	createBacklogSession = func(_ *Manager, ctx context.Context, _ CreateSessionRequest) (session.InstanceData, error) {
		close(stop)
		<-ctx.Done()
		return session.InstanceData{}, ctx.Err()
	}
	t.Cleanup(func() { createBacklogSession = prev })

	m.deliverOrchestrator(stop, repoID, 2, time.Now())

	o := orchOf(t, repoID)
	require.Empty(t, o.Failures, "a shutdown is not the session's fault")
	require.Len(t, o.Events, 1)
}

// TestOrchestratorReplacementWhosePromptWasNotDeliveredKeepsTheEvents: the
// replacement's first prompt carries the events. When the create reports it
// proven not delivered, the session is bound but the events stay, for the next
// pass to send to the now-live session.
func TestOrchestratorReplacementWhosePromptWasNotDeliveredKeepsTheEvents(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort", LastPromptDeliveryStatus: session.PromptNotDelivered}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())

	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID, "the replacement is bound")
	require.Len(t, o.Events, 1, "events its first prompt never delivered are kept")
}

// TestOrchestratorReplacementRefusedByThePreflightIsAFailure: a replacement
// goes through the host checks an item start runs. One that cannot launch
// starts no session and counts toward escalation.
func TestOrchestratorReplacementRefusedByThePreflightIsAFailure(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	prevValidate := validateBacklogCreate
	validateBacklogCreate = func(CreateSessionRequest) error { return errors.New(`agent "claude" is not installed`) }
	t.Cleanup(func() { validateBacklogCreate = prevValidate })
	created := false
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		created = true
		return session.InstanceData{ID: "n1"}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())

	require.False(t, created, "a replacement the preflight refuses is not created")
	o := orchOf(t, repoID)
	require.Equal(t, "o1", o.SessionID)
	require.Len(t, o.Failures, 1)
	require.Len(t, o.Events, 1)
}

// TestOrchestratorUnstartedReplacementGetsTheStartPromptNext: a replacement
// whose first prompt never arrived is bound unstarted, and the next delivery
// sends it the start prompt with the handoff and the events, not bare events
// (Codex P2 on #6571).
func TestOrchestratorUnstartedReplacementGetsTheStartPromptNext(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	_, err := m.backlogService().SetOrchestratorHandoff(backlog.SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Handoff: "Merge B-3 next."})
	require.NoError(t, err)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.mu.Unlock()
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, _ context.Context, _ CreateSessionRequest) (session.InstanceData, error) {
		m.mu.Lock()
		m.instances[daemonInstanceKey(repoID, "effort B-2 Effort")] = &session.Instance{ID: "n1", Title: "effort B-2 Effort", Program: "claude", CreatedAt: time.Now()}
		m.mu.Unlock()
		return session.InstanceData{ID: "n1", Title: "effort B-2 Effort", LastPromptDeliveryStatus: session.PromptNotDelivered}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())
	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID)
	require.Equal(t, backlog.OrchUnstartedReplaced, o.Unstarted)
	require.Len(t, o.Events, 1)

	sent := stubBacklogSend(t, m, repoID)
	runOrchDelivery(t, m, repoID, time.Now())
	require.Equal(t, 1, sent.count())
	for _, want := range []string{"af started this session to replace it", "Merge B-3 next.", "B-3 Child: ready for review."} {
		require.Contains(t, sent.reqs[0].Prompt, want)
	}
	o = orchOf(t, repoID)
	require.Empty(t, o.Unstarted, "a confirmed start prompt clears it")
	require.Empty(t, o.Events)
}

// TestOrchestratorDeliveryAckRetriesABusyBacklog: the acknowledgment of a sent
// prompt goes through the retrying write, so a busy backlog does not leave the
// delivered events to be sent again (Codex P2 on #6571).
func TestOrchestratorDeliveryAckRetriesABusyBacklog(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	prevApply, prevBackoff := applyBacklog, finishStartBackoff
	calls := 0
	applyBacklog = func(svc *backlog.Service, ref backlog.ProjectRef, fn func(*backlog.Backlog) ([]int, error)) (*backlog.Backlog, error) {
		calls++
		if calls == 1 {
			return nil, backlog.ErrBusy
		}
		return prevApply(svc, ref, fn)
	}
	finishStartBackoff = time.Millisecond
	t.Cleanup(func() { applyBacklog, finishStartBackoff = prevApply, prevBackoff })

	runOrchDelivery(t, m, repoID, time.Now())

	require.Equal(t, 1, sent.count())
	require.GreaterOrEqual(t, calls, 2, "the busy acknowledgment was retried")
	require.Empty(t, orchOf(t, repoID).Events, "the delivered events are dropped, not left to be sent again")
}

// TestOrchestratorAdoptsAReplacementStartedBeforeARestart: af records a
// replacement before it creates it. A pass that finds that record and the
// session it started, unbound, binds it instead of starting a second
// (Codex P2 on #6571).
func TestOrchestratorAdoptsAReplacementStartedBeforeARestart(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	svc := m.backlogService()
	since := time.Now().Add(-time.Minute)
	_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		return []int{2}, b.BeginOrchReplace(2, "o1", "effort B-2 Effort", since)
	})
	require.NoError(t, err)
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, "planner"))
	m.instances[daemonInstanceKey(repoID, "effort B-2 Effort")] = &session.Instance{ID: "n1", Title: "effort B-2 Effort", Program: "claude", CreatedAt: since.Add(time.Second)}
	m.mu.Unlock()
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		t.Error("a second replacement was started")
		return session.InstanceData{ID: "n2"}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	runOrchDelivery(t, m, repoID, time.Now())

	o := orchOf(t, repoID)
	require.Equal(t, "n1", o.SessionID, "the replacement started before the restart is bound")
	require.Nil(t, o.Replacing)
	require.Equal(t, backlog.OrchUnstartedReplaced, o.Unstarted, "it may not have had its start prompt")
	require.Len(t, o.Events, 1)
}

// TestOrchestratorDeliveryAbandonedWhenTheGroupIsReboundMidDelivery: a rebind
// while af restores the old session abandons the send. The old session is not
// woken, the events stay for the new orchestrator, and nothing is counted
// (Codex P2 on #6571).
func TestOrchestratorDeliveryAbandonedWhenTheGroupIsReboundMidDelivery(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, "other")] = &session.Instance{ID: "o2", Title: "other", Program: "claude"}
	m.mu.Unlock()
	prevNeeds, prevRestore := backlogNeedsRestore, restoreBacklogSession
	backlogNeedsRestore = func(s itemSession) bool { return s.instance.ID == "o1" }
	restoreBacklogSession = func(m *Manager, req RestoreSessionRequest) (session.InstanceData, error) {
		_, err := m.backlogService().SetOrchestrator(backlog.SetOrchestratorRequest{RepoID: repoID, ID: 2, SessionID: "o2", Replace: true})
		require.NoError(t, err)
		return session.InstanceData{ID: req.ID}, nil
	}
	t.Cleanup(func() { backlogNeedsRestore, restoreBacklogSession = prevNeeds, prevRestore })

	runOrchDelivery(t, m, repoID, time.Now())

	require.Zero(t, sent.count(), "the session the group was rebound away from is not woken")
	o := orchOf(t, repoID)
	require.Equal(t, "o2", o.SessionID)
	require.Len(t, o.Events, 1, "the events stay for the new orchestrator")
	require.Empty(t, o.Failures)
}

// TestOrchestratorRestoredWithoutItsConversationKeepsTheStartPromptPending: a
// session restored without its conversation is marked unstarted before the
// restore, so when its start prompt is held, the next pass sends the start
// prompt again in full, not bare events (Codex P2 on #6571).
func TestOrchestratorRestoredWithoutItsConversationKeepsTheStartPromptPending(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	sent := stubBacklogSend(t, m, repoID)
	sent.err = errBacklogTargetLimited // the first send meets a usage limit: held
	prevNeeds, prevRestore := backlogNeedsRestore, restoreBacklogSession
	needs := 1
	backlogNeedsRestore = func(s itemSession) bool {
		if s.instance.ID == "o1" && needs > 0 {
			needs--
			return true
		}
		return false
	}
	restoreBacklogSession = func(_ *Manager, req RestoreSessionRequest) (session.InstanceData, error) {
		return session.InstanceData{ID: req.ID}, nil
	}
	t.Cleanup(func() { backlogNeedsRestore, restoreBacklogSession = prevNeeds, prevRestore })

	runOrchDelivery(t, m, repoID, time.Now())
	o := orchOf(t, repoID)
	require.Equal(t, backlog.OrchUnstartedRestarted, o.Unstarted, "marked before the restore")
	require.Len(t, o.Events, 1, "a held send keeps the events")

	sent.err = nil
	runOrchDelivery(t, m, repoID, time.Now())
	require.Equal(t, 2, sent.count())
	require.Contains(t, sent.reqs[1].Prompt, "restarted without its earlier conversation",
		"the live session still gets the start prompt, not bare events")
	o = orchOf(t, repoID)
	require.Empty(t, o.Unstarted)
	require.Empty(t, o.Events)
}
