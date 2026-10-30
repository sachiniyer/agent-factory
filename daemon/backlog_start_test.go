package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func backlogTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	// These tests stand in for the session create; no agent is installed on the
	// runner, so the host preflight is exercised on its own below.
	prevValidate := validateBacklogCreate
	validateBacklogCreate = func(CreateSessionRequest) error { return nil }
	t.Cleanup(func() { validateBacklogCreate = prevValidate })
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.backlogService().Add(backlog.AddRequest{RepoID: repo.ID,
		Items: []backlog.ItemInput{{Key: "a", Title: "web/view", Body: "Build it."}}}); err != nil {
		t.Fatal(err)
	}
	return m, repo.ID, repoPath
}

func loadItem(t *testing.T, repoID string, id int) backlog.Item {
	t.Helper()
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	return *b.Item(id)
}

func TestStartBacklogItemCreatesItemSession(t *testing.T) {
	m, repoID, repoPath := backlogTestManager(t)
	var prompt string
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		prompt = req.Prompt
		return prev(m, ctx, req)
	}
	t.Cleanup(func() { createBacklogSession = prev })
	run, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != backlog.RunStarting || run.PrevStatus != backlog.StatusDraft {
		t.Fatalf("run %+v", run)
	}
	m.backgroundMutationWG.Wait()

	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning || len(it.Runs) != 1 || it.Runs[0].State != backlog.RunStarted {
		t.Fatalf("item %+v", it)
	}
	sessions := m.itemSessions(repoID)
	s, ok := sessions[it.Runs[0].SessionID]
	if !ok {
		t.Fatalf("no item session %q in %v", it.Runs[0].SessionID, sessions)
	}
	if s.instance.Title != "B-1 web view" || s.instance.BacklogItem != 1 || !s.instance.BacklogRunActive() {
		t.Fatalf("session %q item %d active %v", s.instance.Title, s.instance.BacklogItem, s.instance.BacklogRunActive())
	}
	if !strings.Contains(prompt, "Build it.") || !strings.Contains(prompt, "af backlog done --self") {
		t.Fatalf("prompt %q", prompt)
	}
	if got := m.backlogSlotsUsed(repoID); got != 1 {
		t.Fatalf("slots used %d, want 1", got)
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if st := v.States[1].State; st != backlog.StateWorking {
		t.Fatalf("state %s", st)
	}
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err == nil ||
		!strings.Contains(err.Error(), "already working") {
		t.Fatalf("second start: %v", err)
	}
}

func TestStartBacklogItemFailedCreate(t *testing.T) {
	m, repoID, repoPath := backlogTestManager(t)
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		return session.InstanceData{}, errors.New("no worktree")
	}
	t.Cleanup(func() { createBacklogSession = prev })

	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusDraft || it.Runs[0].State != backlog.RunFailed {
		t.Fatalf("item %+v", it)
	}
	if !strings.Contains(it.Thread[0].Text, "no worktree") {
		t.Fatalf("thread %+v", it.Thread)
	}
	if got := m.backlogSlotsUsed(repoID); got != 0 {
		t.Fatalf("slot leaked: %d", got)
	}
}

// A start whose agent cannot launch fails at once with the preflight's message
// and records no run, instead of creating a session that dies while waiting for
// the agent (found in the slice 4 play-test).
func TestStartBacklogItemFailsFastWhenTheHostCheckRefuses(t *testing.T) {
	m, repoID, repoPath := backlogTestManager(t)
	validateBacklogCreate = func(CreateSessionRequest) error {
		return errors.New("Claude Code is not installed or not on PATH")
	}
	created := false
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		created = true
		return session.InstanceData{}, nil
	}
	t.Cleanup(func() { createBacklogSession = prev })

	_, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want the preflight message", err)
	}
	m.backgroundMutationWG.Wait()
	if created {
		t.Fatal("a session was created for a start the host check refused")
	}
	it := loadItem(t, repoID, 1)
	if len(it.Runs) != 0 || it.Status != backlog.StatusDraft {
		t.Fatalf("a refused start left state behind: %+v", it)
	}
	if got := m.backlogSlotsUsed(repoID); got != 0 {
		t.Fatalf("slot leaked: %d", got)
	}
}

func startedItem(t *testing.T) (*Manager, string, *session.Instance) {
	t.Helper()
	m, repoID, repoPath := backlogTestManager(t)
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	return m, repoID, m.itemSessions(repoID)[it.Runs[0].SessionID].instance
}

func TestBacklogRunEndedMovesToReview(t *testing.T) {
	m, repoID, inst := startedItem(t)
	m.backlogRunEnded(repoID, inst.ID)
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusReview || it.Thread[len(it.Thread)-1].Text != backlog.RunEndNote {
		t.Fatalf("item %+v", it)
	}
}

func TestContinueBacklogItem(t *testing.T) {
	m, repoID, inst := startedItem(t)
	if !inst.DisarmBacklogRun() {
		t.Fatal("precondition: run active")
	}
	m.backlogRunEnded(repoID, inst.ID)

	var sent []SendPromptRequest
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(_ *Manager, req SendPromptRequest) error { sent = append(sent, req); return nil }
	t.Cleanup(func() { sendBacklogPrompt = prev })

	run, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser)
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != inst.ID || len(sent) != 1 || sent[0].ID != inst.ID || !strings.Contains(sent[0].Prompt, "Continue") {
		t.Fatalf("run %+v sent %+v", run, sent)
	}
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning || len(it.Runs) != 1 || !inst.BacklogRunActive() {
		t.Fatalf("item %+v active %v", it, inst.BacklogRunActive())
	}
}

func TestContinueBacklogItemSendFails(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	m.backlogRunEnded(repoID, inst.ID)
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error { return errors.New("pane gone") }
	t.Cleanup(func() { sendBacklogPrompt = prev })

	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err == nil {
		t.Fatal("start succeeded with a failed send")
	}
	it := loadItem(t, repoID, 1)
	if inst.BacklogRunActive() || it.Status != backlog.StatusReview {
		t.Fatal("a failed send must leave the item and session as they were")
	}
	if !strings.Contains(it.Thread[len(it.Thread)-1].Text, "pane gone") {
		t.Fatalf("thread %+v", it.Thread)
	}
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error { return nil }
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatalf("retry after a failed send: %v", err)
	}
}

func TestRecoverBacklogMatchesStartingRun(t *testing.T) {
	m, repoID, inst := startedItem(t)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		r.State, r.SessionID, r.Title = backlog.RunStarting, "", ""
		r.StartedAt = inst.CreatedAt.Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prevStart := daemonProcessStart
	t.Cleanup(func() { daemonProcessStart = prevStart })
	daemonProcessStart = time.Now()
	m.recoverBacklog(repoID, nil)
	it := loadItem(t, repoID, 1)
	if it.Runs[0].State != backlog.RunStarted || it.Runs[0].SessionID != inst.ID {
		t.Fatalf("run %+v", it.Runs[0])
	}
}

func TestBacklogSessionFacts(t *testing.T) {
	cases := []struct {
		name string
		view session.LifecycleView
		want backlog.SessionFacts
	}{
		{"working", session.LifecycleView{Liveness: session.LiveRunning, BacklogRunActive: true}, backlog.SessionFacts{Live: true, RunActive: true}},
		{"idle", session.LifecycleView{Liveness: session.LiveReady}, backlog.SessionFacts{Live: true}},
		{"archived", session.LifecycleView{Liveness: session.LiveArchived, BacklogRunActive: true}, backlog.SessionFacts{Reason: "session archived"}},
		{"dead", session.LifecycleView{Liveness: session.LiveDead}, backlog.SessionFacts{Reason: "session dead"}},
		{"limit", session.LifecycleView{Liveness: session.LiveLimitReached, BacklogRunActive: true}, backlog.SessionFacts{Limited: true, Reason: "usage limit"}},
		{"killing", session.LifecycleView{Liveness: session.LiveReady, InFlightOp: session.OpKilling}, backlog.SessionFacts{Reason: "session killed"}},
		{"lost finished", session.LifecycleView{Liveness: session.LiveLost}, backlog.SessionFacts{Lost: true, Reason: "session lost"}},
	}
	for _, c := range cases {
		if got := backlogSessionFacts(itemSession{view: c.view}, true); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	if got := backlogSessionFacts(itemSession{}, false); got.Reason != "session killed" {
		t.Errorf("missing: %+v", got)
	}
	if !holdsBacklogSlot(itemSession{view: cases[0].view}) || holdsBacklogSlot(itemSession{view: cases[1].view}) {
		t.Error("slot follows the active run")
	}
}

func TestBacklogSlotReservations(t *testing.T) {
	var s backlogState
	s.reserve("r")
	s.reserve("r")
	s.release("r")
	if s.reserved("r") != 1 {
		t.Fatal(s.reserved("r"))
	}
	s.release("r")
	s.release("r")
	if s.reserved("r") != 0 {
		t.Fatal(s.reserved("r"))
	}
}

// A continue whose prompt the runtime observed absent is a failed continue
// (Codex P2 on #6294): the default sender turns PromptNotDelivered into an error.
func TestSendBacklogPromptReportsObservedAbsenceAsAnError(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := observedPromptBackend{
		readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()},
		status:           session.PromptNotDelivered,
	}
	registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)

	err := sendBacklogPrompt(manager, SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "continue"})
	if !errors.Is(err, errBacklogPromptNotDelivered) {
		t.Fatalf("err = %v, want errBacklogPromptNotDelivered", err)
	}
}

// A create that succeeds with the initial prompt proven undelivered is a failed
// start, not a started run (Codex P2 on #6294): the run is disarmed so its first
// idle edge cannot end it as a completion, and the item goes back.
func TestStartBacklogItemFailsWhenTheInitialPromptWasNotDelivered(t *testing.T) {
	m, repoID, repoPath := backlogTestManager(t)
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		data, err := prev(m, ctx, req)
		data.LastPromptDeliveryStatus = session.PromptNotDelivered
		return data, err
	}
	t.Cleanup(func() { createBacklogSession = prev })
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusDraft || len(it.Runs) != 1 || it.Runs[0].State != backlog.RunFailed {
		t.Fatalf("a start whose prompt never reached the agent was recorded as started: %+v", it)
	}
	for _, s := range m.itemSessions(repoID) {
		if s.instance.BacklogRunActive() {
			t.Fatal("the run marker stayed armed")
		}
	}
	if got := m.backlogSlotsUsed(repoID); got != 0 {
		t.Fatalf("slot leaked: %d", got)
	}
}

// A start named by "~/project" creates its session in the expanded path (Codex
// P2 on #6294): the raw value used to reach the host preflight and CreateSession.
func TestStartBacklogItemExpandsATildeRepoPath(t *testing.T) {
	m, _, repoPath := backlogTestManager(t)
	t.Setenv("HOME", filepath.Dir(repoPath))
	var got string
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		got = req.RepoPath
		return prev(m, ctx, req)
	}
	t.Cleanup(func() { createBacklogSession = prev })
	var validated string
	prevValidate := validateBacklogCreate
	validateBacklogCreate = func(req CreateSessionRequest) error { validated = req.RepoPath; return nil }
	t.Cleanup(func() { validateBacklogCreate = prevValidate })
	tilde := "~/" + filepath.Base(repoPath)
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: tilde, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	if got != repoPath || validated != repoPath {
		t.Fatalf("create got %q, preflight got %q, want the expanded %q", got, validated, repoPath)
	}
}

// A continue whose delivery cannot be confirmed (a timeout after Enter reached
// the pane) keeps the run marker and the running item (Codex P2 on #6294,
// invariant 1 of the run lifecycle model): unknown is never "not delivered".
func TestContinueWithAnUncertainDeliveryKeepsTheRun(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	m.backlogRunEnded(repoID, inst.ID) // the previous run ended; the item is in review
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error {
		return fmt.Errorf("%w: tmux send timed out", errBacklogDeliveryUncertain)
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatalf("an unconfirmed delivery was reported as a failed start: %v", err)
	}
	if !inst.BacklogRunActive() {
		t.Fatal("the run marker was dropped for a send that may have landed")
	}
	m.backgroundMutationWG.Wait() // the advisory note is written by a tracked worker
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning {
		t.Fatalf("status = %s, want running", it.Status)
	}
	if n := it.Thread[len(it.Thread)-1]; !strings.Contains(n.Text, "could not confirm") {
		t.Fatalf("no note about the unconfirmed delivery: %+v", n)
	}
}

// The default sender separates a proven refusal from an uncertain send.
func TestSendBacklogPromptClassifiesDeliveryErrors(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &failingPromptBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)
	err := sendBacklogPrompt(manager, SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "continue"})
	if !errors.Is(err, errBacklogDeliveryUncertain) || errors.Is(err, errBacklogPromptNotDelivered) {
		t.Fatalf("a send that errored after the pane was reached is uncertain, got %v", err)
	}
	err = sendBacklogPrompt(manager, SendPromptRequest{Title: "no-such-session", RepoID: repoID, Prompt: "continue"})
	if !errors.Is(err, errBacklogPromptNotDelivered) {
		t.Fatalf("a refusal before any byte was sent is proven not delivered, got %v", err)
	}
}

// A continuation refused as undelivered must not swallow the previous run's
// completion (Codex P2 on #6294): arming bumped the generation, so the queued
// edge for the run that had ended fails its fence; the rollback settles the
// session's current idle state instead.
func TestFailedContinueSettlesThePreviousRunsEnd(t *testing.T) {
	m, repoID, inst := startedItem(t)
	idleItemSession(t, inst) // the run ended; its completion has not been applied yet
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error { return errBacklogPromptNotDelivered }
	t.Cleanup(func() { sendBacklogPrompt = prev })
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err == nil {
		t.Fatal("a proven non-delivery was reported as a started continue")
	}
	m.backgroundMutationWG.Wait()
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review: the previous run's end was lost", it.Status)
	}
}
