package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestCloseTab_RemovesNonAgentTabAndPersists is the headline CloseTab test: a
// non-agent tab is closed (by name), the resolved name is returned, the
// in-memory tab list shrinks back to the agent tab, and the persisted record
// no longer carries the closed tab so it does not reappear on restart.
func TestCloseTab_RemovesNonAgentTabAndPersists(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	agentName := "af_" + title + "_agent"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, agentName)
	if _, err := inst.AddProcessTab("btop -t", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}
	if inst.TabCount() != 2 {
		t.Fatalf("expected 2 tabs after AddProcessTab, got %d", inst.TabCount())
	}

	name, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "btop"})
	if err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if name != "btop" {
		t.Fatalf("closed tab name = %q, want %q", name, "btop")
	}
	if inst.TabCount() != 1 {
		t.Fatalf("expected 1 tab after CloseTab, got %d", inst.TabCount())
	}
	if got := inst.GetTabs(); got[0].Kind != session.TabKindAgent {
		t.Fatalf("remaining tab kind = %v, want agent", got[0].Kind)
	}

	// The persisted record must reflect the close so the tab does not return
	// on a restart.
	raw, err := config.LoadRepoInstances(repo.ID)
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
	for _, tab := range data[0].Tabs {
		if tab.Kind == session.TabKindProcess {
			t.Fatalf("persisted record still carries process tab %q after close", tab.Name)
		}
	}
}

// TestCloseTab_RejectsAgentTab verifies the agent tab (index 0) cannot be
// closed — KillSession tears down the whole session instead.
func TestCloseTab_RejectsAgentTab(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	_, err = manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabIndex: 0})
	if err == nil {
		t.Fatal("expected error closing the agent tab, got nil")
	}
	if !strings.Contains(err.Error(), "agent tab") {
		t.Fatalf("expected agent-tab rejection, got: %v", err)
	}
}

// TestCloseTab_RejectsUnknownTab verifies a name that matches no tab is
// rejected rather than silently closing the wrong tab.
func TestCloseTab_RejectsUnknownTab(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	_, err = manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "ghost"})
	if err == nil {
		t.Fatal("expected error for unknown tab, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected unknown-tab error naming the tab, got: %v", err)
	}
}

// TestCloseTab_RejectsRemoteInstance verifies remote sessions' tabs (fixed by
// their hook config) cannot be closed, mirroring the TUI's `w` rule.
func TestCloseTab_RejectsRemoteInstance(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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

	_, err = manager.CloseTab(CloseTabRequest{Title: "rem", RepoID: repo.ID, TabName: "shell"})
	if err == nil {
		t.Fatal("expected error for remote instance, got nil")
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("expected remote-rejection error, got: %v", err)
	}
}

// TestControlServer_CloseTab_GatedAndValidated covers the RPC-handler gate: a
// warming (not-ready) manager fails fast with the typed starting error, and a
// traversal RepoID is rejected at the network boundary.
func TestControlServer_CloseTab_GatedAndValidated(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	shell, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	if shell.Ready() {
		t.Fatal("manager shell must not report ready")
	}
	notReady := &controlServer{manager: shell}
	var resp CloseTabResponse
	if err := notReady.CloseTab(CloseTabRequest{Title: "x"}, &resp); !IsDaemonStartingErr(err) {
		t.Fatalf("CloseTab on warming manager: want daemon-starting error, got: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ready := &controlServer{manager: manager}
	err = ready.CloseTab(CloseTabRequest{Title: "x", RepoID: "../../../etc/passwd"}, &resp)
	if err == nil || !strings.Contains(err.Error(), "rejected RPC request") {
		t.Fatalf("CloseTab traversal RepoID: want rejection, got: %v", err)
	}
}

// TestRPCClients_CloseTabAndSetPRInfo_RoundTrip drives the package-level client
// funcs (daemon.CloseTab / daemon.SetPRInfo) through an in-process control
// server bound on a temp-HOME socket — exercising the full client → RPC →
// Manager → persist wire path. It is hermetic: the launch seam is stubbed so a
// ping race can never fork the real daemon, and the socket lives under the test
// temp HOME.
func TestRPCClients_CloseTabAndSetPRInfo_RoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return fmt.Errorf("test must not spawn a real daemon") }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "client-rt"
	agentName := "af_" + title + "_agent"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, agentName)
	if _, err := inst.AddProcessTab("btop", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}

	closeServer, err := startControlServer(manager, newTaskScheduler(), nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	name, err := CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "btop"})
	if err != nil {
		t.Fatalf("CloseTab client: %v", err)
	}
	if name != "btop" {
		t.Fatalf("CloseTab client returned name %q, want btop", name)
	}
	if inst.TabCount() != 1 {
		t.Fatalf("expected 1 tab after client CloseTab, got %d", inst.TabCount())
	}

	if err := SetPRInfo(SetPRInfoRequest{
		Title:  title,
		RepoID: repo.ID,
		PRInfo: session.PRInfoData{Number: 7, Title: "feat", URL: "https://example/pr/7", State: "OPEN"},
	}); err != nil {
		t.Fatalf("SetPRInfo client: %v", err)
	}
	got := inst.GetPRInfo()
	if got == nil || got.Number != 7 || got.State != "OPEN" {
		t.Fatalf("PR info after client SetPRInfo = %+v, want Number 7 / OPEN", got)
	}
}
