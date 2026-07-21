package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

type createCallResult struct {
	resp CreateSessionResponse
	err  error
}

// blockingCreateFactory stops inside NewInstance, the earliest slow boundary for
// docker/ssh/hook provisioning. Seeing a row while this is blocked proves the
// projection is daemon-owned and published before a concrete Instance exists.
func blockingCreateFactory(
	t *testing.T,
	result session.Backend,
	factoryErr error,
) (<-chan session.InstanceOptions, func()) {
	t.Helper()
	entered := make(chan session.InstanceOptions, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		entered <- opts
		<-release
		return result, factoryErr
	})
	t.Cleanup(restore)
	return entered, unblock
}

func waitForCreateFactory(t *testing.T, entered <-chan session.InstanceOptions) session.InstanceOptions {
	t.Helper()
	select {
	case opts := <-entered:
		return opts
	case <-time.After(3 * time.Second):
		t.Fatal("create did not reach the blocked backend factory")
		return session.InstanceOptions{}
	}
}

func waitForCreateResult(t *testing.T, done <-chan createCallResult) createCallResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("CreateSession did not return after the backend factory was released")
		return createCallResult{}
	}
}

func startCreateCall(cs *controlServer, req CreateSessionRequest) <-chan createCallResult {
	done := make(chan createCallResult, 1)
	go func() {
		var resp CreateSessionResponse
		err := cs.createSession(context.Background(), req, &resp)
		done <- createCallResult{resp: resp, err: err}
	}()
	return done
}

func assertCreatingProjection(t *testing.T, got session.InstanceData, title, repoPath string) {
	t.Helper()
	if got.ID == "" {
		t.Fatal("creating projection has no stable id")
	}
	if got.Title != title {
		t.Fatalf("creating title = %q, want %q", got.Title, title)
	}
	if got.InFlightOp != session.OpCreating || got.Status != session.Loading {
		t.Fatalf("creating state = (op %v, status %v), want (OpCreating, Loading)", got.InFlightOp, got.Status)
	}
	if got.Worktree.RepoPath != repoPath {
		t.Fatalf("creating repo path = %q, want %q", got.Worktree.RepoPath, repoPath)
	}
}

func TestCreateSessionPublishesAuthoritativeCreatingProjection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	backend := session.NewFakeBackend()
	backend.CompleteStart()
	entered, unblock := blockingCreateFactory(t, readyFakeBackend{backend}, nil)
	_, events := manager.events.subscribe()
	done := startCreateCall(&controlServer{manager: manager}, CreateSessionRequest{
		Title: "slow-create", RepoPath: repoPath, Program: "claude",
	})
	opts := waitForCreateFactory(t, entered)

	pendingEvent := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	assertCreatingProjection(t, pendingEvent, "slow-create", repoPath)
	snapshot := manager.Snapshot(repo.ID)
	if len(snapshot) != 1 {
		t.Fatalf("Snapshot during create returned %d rows, want 1: %+v", len(snapshot), snapshot)
	}
	assertCreatingProjection(t, snapshot[0], "slow-create", repoPath)
	if opts.ID != pendingEvent.ID || !opts.CreatedAt.Equal(pendingEvent.CreatedAt) {
		t.Fatalf("constructor identity = (%q, %v), pending = (%q, %v)",
			opts.ID, opts.CreatedAt, pendingEvent.ID, pendingEvent.CreatedAt)
	}

	unblock()
	result := waitForCreateResult(t, done)
	if result.err != nil {
		t.Fatalf("CreateSession: %v", result.err)
	}
	if result.resp.Instance.ID != pendingEvent.ID || !result.resp.Instance.CreatedAt.Equal(pendingEvent.CreatedAt) {
		t.Fatalf("completed identity = (%q, %v), pending = (%q, %v)",
			result.resp.Instance.ID, result.resp.Instance.CreatedAt, pendingEvent.ID, pendingEvent.CreatedAt)
	}
	createdEvent := drainNextSessionEvent(t, events, agentproto.EventSessionCreated)
	if createdEvent.ID != pendingEvent.ID || createdEvent.InFlightOp != session.OpNone {
		t.Fatalf("created event did not settle pending identity: %+v", createdEvent)
	}
	settled := manager.Snapshot(repo.ID)
	if len(settled) != 1 || settled[0].ID != pendingEvent.ID || settled[0].InFlightOp != session.OpNone {
		t.Fatalf("settled Snapshot = %+v", settled)
	}
}

func TestCreateSessionFailureRemovesCreatingProjection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	backendErr := errors.New("slow provision failed with daemon detail")
	entered, unblock := blockingCreateFactory(t, nil, backendErr)
	_, events := manager.events.subscribe()
	done := startCreateCall(&controlServer{manager: manager}, CreateSessionRequest{
		Title: "failed-create", RepoPath: repoPath, Program: "claude",
	})
	waitForCreateFactory(t, entered)

	pendingEvent := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	assertCreatingProjection(t, pendingEvent, "failed-create", repoPath)
	if got := manager.Snapshot(repo.ID); len(got) != 1 || got[0].ID != pendingEvent.ID {
		t.Fatalf("Snapshot during failed create = %+v", got)
	}

	unblock()
	result := waitForCreateResult(t, done)
	if !errors.Is(result.err, backendErr) {
		t.Fatalf("CreateSession error = %v, want daemon backend error %v", result.err, backendErr)
	}
	removedEvent := drainNextSessionEvent(t, events, agentproto.EventSessionKilled)
	if removedEvent.ID != pendingEvent.ID || removedEvent.Title != pendingEvent.Title {
		t.Fatalf("removal event = %+v, pending = %+v", removedEvent, pendingEvent)
	}
	if got := manager.Snapshot(repo.ID); len(got) != 0 {
		t.Fatalf("failed create left a phantom Snapshot row: %+v", got)
	}
}
