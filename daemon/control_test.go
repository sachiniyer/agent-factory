package daemon

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func setupControlRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run(); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "config", "user.name", "Test User").Run(); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "init").Run(); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return repo
}

func installInstantBackend(t *testing.T) {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return backend, nil
	})
	t.Cleanup(restore)
}

func TestManagerCreateSessionPersistsAndRejectsDuplicate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(CreateSessionRequest{
		Title:    "daemon-owned",
		RepoPath: repoPath,
		Program:  "claude",
		AutoYes:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if data.Title != "daemon-owned" || !data.AutoYes || data.Status != session.Running {
		t.Fatalf("unexpected created data: %+v", data)
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != 1 || stored[0].Title != "daemon-owned" {
		t.Fatalf("expected created session in storage, got %+v", stored)
	}

	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    "daemon-owned",
		RepoPath: repoPath,
		Program:  "claude",
	}); err == nil {
		t.Fatalf("expected duplicate title to be rejected")
	}
}

func TestControlServerCreateAndKillSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var createResp CreateSessionResponse
	if err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "rpc-session",
		RepoPath: repoPath,
		Program:  "claude",
	}, &createResp); err != nil {
		t.Fatalf("rpc CreateSession: %v", err)
	}
	if createResp.Instance.Title != "rpc-session" {
		t.Fatalf("unexpected create response: %+v", createResp)
	}

	var killResp KillSessionResponse
	if err := callDaemonNoEnsure("KillSession", KillSessionRequest{
		Title:  "rpc-session",
		RepoID: repo.ID,
	}, &killResp); err != nil {
		t.Fatalf("rpc KillSession: %v", err)
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected storage to be empty after kill, got %+v", stored)
	}
}
