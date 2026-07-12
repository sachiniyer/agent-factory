package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// slowKillBackend is a readyFakeBackend whose Kill blocks until released, so
// tests can hold a session inside its teardown window.
type slowKillBackend struct {
	readyFakeBackend
	killStarted chan struct{}
	killBlock   chan struct{}
}

func (b *slowKillBackend) Kill(inst *session.Instance) error {
	close(b.killStarted)
	<-b.killBlock
	return b.readyFakeBackend.Kill(inst)
}

// TestKillSessionBlocksTitleReuseDuringTeardown pins the title-reservation
// property the async TUI kill (#844) leans on: while Manager.KillSession is
// still tearing a session down, its instance stays in the manager's map and
// its record stays on disk, so a CreateSession reusing the title must be
// rejected. Only after the teardown completes may the title be reused.
func TestKillSessionBlocksTitleReuseDuringTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

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

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
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
	select {
	case <-backend.killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession never reached the backend teardown")
	}

	// Mid-teardown: the title must still be blocked against reuse.
	_, err = manager.CreateSession(CreateSessionRequest{
		Title:    "busy",
		RepoPath: repoPath,
		Program:  "claude",
	})
	if err == nil {
		t.Fatal("expected title reuse to be rejected while teardown is in flight")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("conflict error should name the title, got: %v", err)
	}

	close(backend.killBlock)
	select {
	case err := <-killDone:
		if err != nil {
			t.Fatalf("KillSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession did not complete after the teardown was released")
	}

	// Teardown finished: the title is free again.
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "busy",
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("expected title reuse to succeed after teardown, got: %v", err)
	}
}
