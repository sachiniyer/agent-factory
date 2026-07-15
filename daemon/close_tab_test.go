package daemon

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
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

// TestCloseTab_RejectsArchivedSession is the #1809 follow-up gate: archive now
// PRESERVES web tabs so a restore can render them again, which made an archived
// session the first one to carry a closable (non-agent) tab. Without this guard a
// tab-delete would permanently strip that URL out of the archived record BEFORE
// the restore that was supposed to bring it back — the very loss the preservation
// exists to prevent, just moved later. The refusal must be actionable, leave the
// record intact, and lift on restore.
func TestCloseTab_RejectsArchivedSession(t *testing.T) {
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
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	const target = "http://localhost:3000"
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: target, Name: "webpreview"}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	inst.SetStatusForTest(session.Archived)

	// Both addressing modes are refused: `tab-delete --name` drives the name path
	// (#1021) and the web × drives the index path, so a gate on only one would leave
	// the other able to strip the URL.
	for _, req := range []CloseTabRequest{
		{Title: title, RepoID: repo.ID, TabIndex: 1},
		{Title: title, RepoID: repo.ID, TabName: "webpreview"},
	} {
		_, err = manager.CloseTab(req)
		if err == nil {
			t.Fatalf("expected error closing a tab on an archived session (req %+v), got nil", req)
		}
		if !strings.Contains(err.Error(), "archived") {
			t.Fatalf("expected an actionable archived rejection, got: %v", err)
		}
	}

	// The preserved tab is still on the record — the refusal didn't half-close it.
	tabs := inst.GetTabs()
	if len(tabs) != 2 {
		t.Fatalf("archived session tabs = %d, want 2 (agent + the preserved web tab)", len(tabs))
	}
	if tabs[1].Kind != session.TabKindWeb || tabs[1].URL != target {
		t.Fatalf("preserved web tab = {kind:%v url:%q}, want the web tab at %q intact", tabs[1].Kind, tabs[1].URL, target)
	}

	// Restored: the tab is closable again — the gate is state, not a tombstone.
	inst.SetStatusForTest(session.Running)
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabIndex: 1}); err != nil {
		t.Fatalf("closing the web tab of a restored session: %v", err)
	}
	if got := len(inst.GetTabs()); got != 1 {
		t.Fatalf("restored session tabs after close = %d, want 1", got)
	}
}

// TestCloseTab_ArchiveWinningOpLockRaceKeepsWebTab closes the hole the plain
// archived gate leaves open: that gate runs BEFORE the op-lock, so it only sees a
// session that was already archived when the close arrived. ArchiveSession holds
// the same op-lock, commits the archive under it, and leaves the SAME instance in
// m.instances (an archived row stays tracked — it is still listed and restorable).
// So a close that resolves a live session and then queues behind an archive finds
// current == instance and UserKilled() false: every pre-existing post-lock check
// passes, and it would go on to delete the web tab the archive just preserved and
// persist the loss — the #1809 URL loss reached through the race rather than the
// front door.
func TestCloseTab_ArchiveWinningOpLockRaceKeepsWebTab(t *testing.T) {
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
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	const target = "http://localhost:3000"
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: target, Name: "webpreview"}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	key := daemonInstanceKey(repo.ID, title)
	opLock := manager.opLockFor(key)
	opLock.Lock()

	done := make(chan error, 1)
	go func() {
		_, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabIndex: 1})
		done <- err
	}()

	// The close must PARK on the op-lock rather than return: that is what proves it
	// passed the pre-lock archived check while the session was still live, which is
	// the only interleaving that can reach the post-lock gate. Without this the test
	// would pass on the pre-lock check alone and prove nothing.
	select {
	case err := <-done:
		opLock.Unlock()
		t.Fatalf("CloseTab returned while the op-lock was held (it never raced the archive): %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// The archive commits under the op-lock, leaving the same pointer tracked —
	// exactly what ArchiveSession does between BeginArchive and releasing the lock.
	inst.SetStatusForTest(session.Archived)
	opLock.Unlock()

	err = <-done
	if err == nil {
		t.Fatal("CloseTab queued behind an archive returned nil: it deleted the web tab the archive had just preserved")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("CloseTab lost-the-race error = %v, want the same actionable archived rejection an up-front close gets", err)
	}

	// The preserved URL survived the race — this is the whole point of #1809.
	tabs := inst.GetTabs()
	if len(tabs) != 2 {
		t.Fatalf("archived session tabs = %d, want 2 (agent + the preserved web tab)", len(tabs))
	}
	if tabs[1].Kind != session.TabKindWeb || tabs[1].URL != target {
		t.Fatalf("preserved web tab = {kind:%v url:%q}, want the web tab at %q intact", tabs[1].Kind, tabs[1].URL, target)
	}
}

// TestCloseTab_RejectsAgentTabByName verifies the agent tab is unclosable when
// targeted by its name too, not just by index 0 — the name path is the one
// `af sessions tab-delete --name` drives (#1021).
func TestCloseTab_RejectsAgentTabByName(t *testing.T) {
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
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	agentTab := inst.GetTabs()[0].Name

	_, err = manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: agentTab})
	if err == nil {
		t.Fatal("expected error closing the agent tab by name, got nil")
	}
	if !strings.Contains(err.Error(), "agent tab") {
		t.Fatalf("expected agent-tab rejection, got: %v", err)
	}
	if inst.TabCount() != 1 {
		t.Fatalf("agent tab must survive, got %d tabs", inst.TabCount())
	}
}

// TestCloseTab_RejectsUnknownSession verifies targeting a session that doesn't
// exist is a clear error, not a panic or silent success (#1021).
func TestCloseTab_RejectsUnknownSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = manager.CloseTab(CloseTabRequest{Title: "ghost", TabName: "watcher"})
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected error naming the missing session, got: %v", err)
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
	assertTabRejection(t, err, "fixed by its runtime")
}

func closeBlockingTabExec(alive map[string]bool, blockedKillName string, killStarted chan<- struct{}, releaseKill <-chan struct{}) (cmd_test.MockCmdExec, func(string) bool) {
	var mu sync.Mutex
	existing := map[string]bool{}
	for name, ok := range alive {
		existing[name] = ok
	}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				mu.Lock()
				ok := existing[name]
				mu.Unlock()
				if ok {
					return nil
				}
				return &tabNoSessionErr{}
			case strings.Contains(s, "new-session"):
				mu.Lock()
				existing[name] = true
				mu.Unlock()
				return nil
			case strings.Contains(s, "kill-session"):
				if name == blockedKillName {
					select {
					case killStarted <- struct{}{}:
					default:
					}
					<-releaseKill
				}
				mu.Lock()
				delete(existing, name)
				mu.Unlock()
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	return exec, func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return existing[name]
	}
}

func startedLocalTabInstanceWithExec(t *testing.T, m *Manager, repoID, repoPath, title, agentName string, exec cmd_test.MockCmdExec) *session.Instance {
	t.Helper()
	pty := tabPtyFactory{t: t, cmdExec: exec}
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), title,
		title+"-branch", "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec))
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// TestCloseTab_SerializedWithInFlightKillDoesNotCloseStaleTab models the
// #1434 race: a kill/archive holds the per-session op-lock while tearing down
// tmux, and a concurrent CloseTab must wait instead of also closing the process
// tab's tmux session. If the destructive op wins and removes the session while
// CloseTab waits, CloseTab must reject the stale pointer and must not re-persist
// it.
func TestCloseTab_SerializedWithInFlightKillDoesNotCloseStaleTab(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	const title, agentName = "worker", "af_worker_agent"
	processTmuxName := agentName + "__btop"
	killStarted := make(chan struct{}, 1)
	releaseKill := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseKill) }) })

	exec, isAlive := closeBlockingTabExec(map[string]bool{agentName: true}, processTmuxName, killStarted, releaseKill)
	inst := startedLocalTabInstanceWithExec(t, manager, repoID, repoPath, title, agentName, exec)
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Command: "btop"}); err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if !isAlive(processTmuxName) {
		t.Fatalf("process tab %q was not spawned", processTmuxName)
	}

	key := daemonInstanceKey(repoID, title)
	opLock := manager.opLockFor(key)
	opLock.Lock()

	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		name, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: "btop"})
		done <- result{name: name, err: err}
	}()

	select {
	case <-killStarted:
		releaseOnce.Do(func() { close(releaseKill) })
		t.Fatal("CloseTab reached tmux kill-session while the teardown op-lock was held")
	case res := <-done:
		t.Fatalf("CloseTab returned while the teardown op-lock was held: name=%q err=%v", res.name, res.err)
	case <-time.After(150 * time.Millisecond):
	}
	if !isAlive(processTmuxName) {
		t.Fatalf("CloseTab closed process tmux %q while teardown op-lock was held", processTmuxName)
	}

	manager.mu.Lock()
	delete(manager.instances, key)
	manager.mu.Unlock()
	opLock.Unlock()

	res := <-done
	if res.err == nil {
		t.Fatalf("CloseTab on a stale instance returned nil error")
	}
	if !strings.Contains(res.err.Error(), "changed state") {
		t.Fatalf("CloseTab stale-instance error = %v, want changed-state error", res.err)
	}
	if res.name != "" {
		t.Fatalf("CloseTab stale-instance name = %q, want empty", res.name)
	}
	if got := inst.TabCount(); got != 2 {
		t.Fatalf("CloseTab mutated stale instance tabs: got %d, want 2", got)
	}
	if !isAlive(processTmuxName) {
		t.Fatalf("CloseTab closed stale process tmux %q after kill won", processTmuxName)
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
func TestRPCClients_CloseTab_RoundTrip(t *testing.T) {
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
	// The SetPRInfo net/rpc client wrapper moved onto the HTTP apiclient in #1592
	// Phase 2 PR3 (the TUI was its only caller), so this round-trip now covers
	// CloseTab alone. The controlServer.SetPRInfo handler stays covered by
	// set_prinfo_test.go.
}
