package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// loadWebTab is a small helper: load the single persisted instance and return its
// first web tab (or nil).
func loadWebTab(t *testing.T, repoID string) *session.TabData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 persisted instance, got %d", len(data))
	}
	for i := range data[0].Tabs {
		if data[0].Tabs[i].Kind == session.TabKindWeb {
			return &data[0].Tabs[i]
		}
	}
	return nil
}

// TestCreateTab_WebSpawnsPersistsAndReturnsName verifies the web-tab path: a
// Kind=web request with a URL creates a PTY-less web tab, returns its name, and
// persists it (with the URL, no tmux name) so it survives a restart.
func TestCreateTab_WebSpawnsPersistsAndReturnsName(t *testing.T) {
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

	const title = "webworker"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	name, _, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173",
	})
	if err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	if name != "web" {
		t.Fatalf("resolved web tab name = %q, want %q", name, "web")
	}

	web := loadWebTab(t, repo.ID)
	if web == nil {
		t.Fatal("no persisted web tab found")
	}
	if web.URL != "http://localhost:5173" {
		t.Fatalf("persisted web tab URL = %q, want http://localhost:5173", web.URL)
	}
	if web.TmuxName != "" {
		t.Fatalf("web tab must persist with no tmux name, got %q", web.TmuxName)
	}
	if web.Command != "" {
		t.Fatalf("web tab must persist with no command, got %q", web.Command)
	}
}

// TestCreateTab_WebPortConvenience verifies --port <n> becomes
// http://localhost:<n>.
func TestCreateTab_WebPortConvenience(t *testing.T) {
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

	const title = "webport"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", Port: 3000}); err != nil {
		t.Fatalf("CreateTab(web,port): %v", err)
	}
	web := loadWebTab(t, repo.ID)
	if web == nil || web.URL != "http://localhost:3000" {
		t.Fatalf("web tab URL from --port = %v, want http://localhost:3000", web)
	}
}

// TestCreateTab_WebRejectsMissingTarget verifies a web tab with neither URL nor
// port is refused.
func TestCreateTab_WebRejectsMissingTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, _, err := manager.CreateTab(CreateTabRequest{Title: "x", Kind: "web"}); err == nil {
		t.Fatal("expected error for web tab with no target, got nil")
	}
}

// TestCreateTab_WebRejectsUnknownKind verifies an unrecognized --kind is refused.
func TestCreateTab_WebRejectsUnknownKind(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, _, err := manager.CreateTab(CreateTabRequest{Title: "x", Kind: "bogus", URL: "http://localhost:1"}); err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
}

// TestCreateTab_WebRejectsRemoteInstance verifies a web tab cannot be created on a
// remote session (no local worktree to persist/rebuild it against).
func TestCreateTab_WebRejectsRemoteInstance(t *testing.T) {
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

	inst, err := session.NewInstance(session.InstanceOptions{Title: "rem", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	seedDiskInstance(t, repo.ID, "rem", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, "rem")] = inst
	manager.mu.Unlock()

	_, _, err = manager.CreateTab(CreateTabRequest{Title: "rem", RepoID: repo.ID, Kind: "web", URL: "http://localhost:3000"})
	if err == nil {
		t.Fatal("expected error for remote instance, got nil")
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("expected remote-rejection error, got: %v", err)
	}
}
