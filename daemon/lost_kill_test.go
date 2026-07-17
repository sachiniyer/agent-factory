package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// failKillBackend is a readyFakeBackend whose Kill always errors, modelling a
// teardown that dies partway — the crash window the kill-intent tombstone
// (#1108) exists for.
type failKillBackend struct {
	readyFakeBackend
}

func (failKillBackend) Kill(*session.Instance) error {
	return errors.New("teardown interrupted")
}

// recordFor returns the persisted InstanceData for title, or nil.
func recordFor(t *testing.T, repoID, title string) *session.InstanceData {
	t.Helper()
	data, err := loadRepoInstanceData(repoID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	for i := range data {
		if data[i].Title == title {
			return &data[i]
		}
	}
	return nil
}

// TestKillSession_TombstoneSurvivesFailedTeardown pins the #1108 ordering
// contract: KillSession persists the kill-intent tombstone BEFORE teardown
// begins, so a teardown that errors (or a daemon crash) after that point
// leaves a record that is provably a user kill — never a Lost session the
// restore loop would resurrect.
func TestKillSession_TombstoneSurvivesFailedTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return failKillBackend{readyFakeBackend{backend}}, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "doomed",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := manager.KillSession(KillSessionRequest{Title: "doomed", RepoID: repo.ID}); err == nil {
		t.Fatal("expected KillSession to surface the teardown failure")
	}

	rec := recordFor(t, repo.ID, "doomed")
	if rec == nil {
		t.Fatal("the record must survive a failed teardown (KillSession returned before DeleteInstance)")
	}
	if !rec.UserKilled {
		t.Fatal("the surviving record must carry the kill-intent tombstone (#1108)")
	}
}

// TestRefreshStatuses_FinishesTombstonedKill: the status poll's finish-kill
// pass (#1108). A tombstoned instance — a kill interrupted after its tombstone
// write — must have its teardown finished on the next poll: record deleted,
// instance dropped from the manager. It must never be probed, marked Lost, or
// restored.
func TestRefreshStatuses_FinishesTombstonedKill(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// A dead-tmux backend would flip an ordinary probed instance to Lost;
	// using it proves the tombstone short-circuits the probe entirely.
	inst := registerStarted(t, manager, repoID, repoPath, "half-killed", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)
	inst.MarkUserKilled()

	manager.RefreshStatuses()

	if rec := recordFor(t, repoID, "half-killed"); rec != nil {
		t.Fatalf("tombstoned record must be deleted by the finish-kill pass, still present: %+v", rec)
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "half-killed")]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("finished kill must drop the instance from the manager's map")
	}
	if got := inst.GetStatus(); got == session.Lost {
		t.Fatal("a tombstoned instance must never be classified Lost")
	}
}

// TestKillSession_RejectsConcurrentDuplicate: while one KillSession's teardown
// is in flight, a second KillSession for the same session is rejected instead
// of running a concurrent double-teardown (#1108 killsInFlight guard).
func TestKillSession_RejectsConcurrentDuplicate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	backend := &slowKillBackend{
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.readyFakeBackend = readyFakeBackend{fake}
		return backend, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "busy",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "busy", RepoID: repo.ID})
		killDone <- kerr
	}()
	<-backend.killStarted

	if _, err := manager.KillSession(KillSessionRequest{Title: "busy", RepoID: repo.ID}); err == nil {
		t.Fatal("expected the duplicate kill to be rejected while the first teardown is in flight")
	}

	close(backend.killBlock)
	if err := <-killDone; err != nil {
		t.Fatalf("first KillSession: %v", err)
	}
}
