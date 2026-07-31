package daemon

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func TestCloseTab_PublishesCommittedRosterBeforeTmuxTeardownCompletes(t *testing.T) {
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

	const title, agentName = "worker", "af_worker_agent"
	processTmuxName := agentName + "__btop"
	killStarted := make(chan struct{}, 1)
	releaseKill := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseKill) }) })
	mockExec, _ := closeBlockingTabExec(
		map[string]bool{agentName: true}, processTmuxName, killStarted, releaseKill)
	startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, mockExec)
	created, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Command: "btop",
	})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}

	_, events := manager.events.subscribe()
	done := make(chan error, 1)
	go func() {
		_, closeErr := manager.CloseTab(CloseTabRequest{
			Title: title, RepoID: repo.ID, TabID: created.ID,
		})
		done <- closeErr
	}()

	select {
	case <-killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseTab never reached tmux teardown")
	}

	// Reaching tmux teardown proves the commit callback returned. Pin that the
	// smaller roster really is durable before checking its client notification.
	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances during teardown: %v", err)
	}
	var persisted []session.InstanceData
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal committed instances: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("committed instance count = %d, want 1", len(persisted))
	}
	for _, tab := range persisted[0].Tabs {
		if tab.ID == created.ID {
			t.Fatalf("tab %q was not durably removed before tmux teardown", created.ID)
		}
	}

	publishedBeforeTeardown := false
	select {
	case event := <-events:
		if event.Type != agentproto.EventSessionUpdated {
			t.Fatalf("event type = %q, want %q", event.Type, agentproto.EventSessionUpdated)
		}
		var update session.InstanceData
		if err := json.Unmarshal(event.Data, &update); err != nil {
			t.Fatalf("unmarshal session.updated: %v", err)
		}
		publishedBeforeTeardown = true
		for _, tab := range update.Tabs {
			if tab.ID == created.ID {
				t.Fatalf("session.updated still carries closed tab %q", created.ID)
			}
		}
	case <-time.After(250 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseKill) })
	if err := <-done; err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if !publishedBeforeTeardown {
		t.Fatal("CloseTab waited for tmux teardown before publishing the durably committed roster")
	}
}
