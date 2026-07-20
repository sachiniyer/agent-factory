package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// tabPtyFactory is a tmux.PtyFactory returning a real temp file as the PTY and
// forwarding the command to the mock executor so session-existence bookkeeping
// fires — the hermetic plumbing AddProcessTab needs to spawn a sibling session
// without touching a real tmux server.
type tabPtyFactory struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
}

func (p tabPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	f, err := os.CreateTemp(p.t.TempDir(), "pty-")
	if err == nil {
		_ = p.cmdExec.Run(cmd)
	}
	return f, err
}
func (p tabPtyFactory) Close() {}

// tabNameKeyedExec is a tmux mock tracking session existence per session name,
// so an instance's agent session and its CLI-spawned process tab are
// independent. Names in `alive` report existing immediately; others come into
// existence after their new-session.
func tabNameKeyedExec(alive map[string]bool) cmd_test.MockCmdExec {
	existing := map[string]bool{}
	for k, v := range alive {
		existing[k] = v
	}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				// Strip the exact-match `=name:` wrapper (`-t =name:`) so the
				// modeled session name matches the bare key tmux resolves to (#1006).
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return &tabNoSessionErr{}
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
}

type tabNoSessionErr struct{}

func (*tabNoSessionErr) Error() string { return "session does not exist" }

// remoteTypeBackend is a FakeBackend that reports a WorkspaceRemote capability
// descriptor, letting CreateTab's remote-rejection branch (no TabManagement) be
// exercised without a real hook backend.
type remoteTypeBackend struct {
	*session.FakeBackend
}

func (remoteTypeBackend) Type() string { return "remote" }

func (remoteTypeBackend) Capabilities() session.Capabilities {
	return session.Capabilities{Workspace: session.WorkspaceRemote}
}

// assertTabRejection pins the copy of the no-tab-management rejection (#1874).
// A bare Contains(err, "remote") passed for years while the message pointed at
// remote_hooks.terminal_cmd — a knob #1592 Phase 4 PR7 deleted — so match the
// rule the user can actually act on, and fail loudly if the deleted knob ever
// comes back into user-facing copy.
func assertTabRejection(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejection error for an off-box instance, got nil")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected the rejection to say %q, got: %v", want, err)
	}
	if strings.Contains(err.Error(), "terminal_cmd") {
		t.Fatalf("rejection points at the deleted remote_hooks.terminal_cmd knob: %v", err)
	}
}

// startedLocalTabInstance builds a started local instance whose agent tab is a
// mock-backed tmux session, registers it in the manager, and seeds a matching
// on-disk record so the manager's refresh keeps the live in-memory instance.
func startedLocalTabInstance(t *testing.T, m *Manager, repoID, repoPath, title, agentName string) *session.Instance {
	t.Helper()
	exec := tabNameKeyedExec(map[string]bool{agentName: true})
	pty := tabPtyFactory{t: t, cmdExec: exec}

	gw, err := git.NewGitWorktreeFromStorage(
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

// TestCreateTab_RejectsEmptyCommand verifies an empty command is refused before
// any session lookup or tab spawn.
func TestCreateTab_RejectsEmptyCommand(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, _, err := manager.CreateTab(CreateTabRequest{Title: "x", Command: "   "}); err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

// TestCreateTab_RejectsRemoteInstance verifies a process tab cannot be created on
// a remote session — it has no local worktree to run a command in.
func TestCreateTab_RejectsRemoteInstance(t *testing.T) {
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

	_, _, err = manager.CreateTab(CreateTabRequest{Title: "rem", RepoID: repo.ID, Command: "btop"})
	assertTabRejection(t, err, "only local sessions support user-managed tabs")
}

// TestCreateTab_SpawnsPersistsAndReturnsName is the headline daemon test: a
// successful CreateTab spawns a Process tab, returns its resolved name, and
// persists it to disk (with command + tmux name) so it survives a restart.
func TestCreateTab_SpawnsPersistsAndReturnsName(t *testing.T) {
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

	const title = "worker"
	agentName := "af_" + title + "_agent"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, agentName)

	name, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop -t"})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if name != "btop" {
		t.Fatalf("resolved tab name = %q, want %q (derived from command basename)", name, "btop")
	}

	// The tab must be persisted to disk with its command + a derived tmux name so
	// it reconnects across a restart.
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
	var proc *session.TabData
	for i := range data[0].Tabs {
		if data[0].Tabs[i].Kind == session.TabKindProcess {
			proc = &data[0].Tabs[i]
		}
	}
	if proc == nil {
		t.Fatalf("no persisted process tab found in %+v", data[0].Tabs)
	}
	if proc.Name != "btop" {
		t.Fatalf("persisted process tab name = %q, want btop", proc.Name)
	}
	if proc.Command != "btop -t" {
		t.Fatalf("persisted process tab command = %q, want %q", proc.Command, "btop -t")
	}
	if proc.TmuxName != agentName+"__btop" {
		t.Fatalf("persisted process tab tmux name = %q, want %q", proc.TmuxName, agentName+"__btop")
	}
}

// TestCreateTab_ShellSpawnsPersistsAndReturnsName verifies the shell-tab path
// the TUI's `t` mutation routes to (#960 PR 2): Shell=true creates a Shell-kind
// tab running $SHELL (ignoring Command), returns its auto-derived name, and
// persists it so it survives a restart.
func TestCreateTab_ShellSpawnsPersistsAndReturnsName(t *testing.T) {
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

	const title = "shellworker"
	agentName := "af_" + title + "_agent"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, agentName)

	// Shell=true ignores Command and creates a $SHELL tab named "shell".
	name, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Shell: true})
	if err != nil {
		t.Fatalf("CreateTab(shell): %v", err)
	}
	if name != "shell" {
		t.Fatalf("resolved shell tab name = %q, want %q", name, "shell")
	}

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
	var shell *session.TabData
	for i := range data[0].Tabs {
		if data[0].Tabs[i].Kind == session.TabKindShell {
			shell = &data[0].Tabs[i]
		}
	}
	if shell == nil {
		t.Fatalf("no persisted shell tab found in %+v", data[0].Tabs)
	}
	if shell.Name != "shell" {
		t.Fatalf("persisted shell tab name = %q, want shell", shell.Name)
	}
	if shell.TmuxName != agentName+"__shell" {
		t.Fatalf("persisted shell tab tmux name = %q, want %q", shell.TmuxName, agentName+"__shell")
	}
}

// TestCreateTab_ShellRejectsRemoteInstance verifies the shell path also refuses
// remote sessions (no local worktree), matching the process path and the TUI's
// `t` rule.
func TestCreateTab_ShellRejectsRemoteInstance(t *testing.T) {
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

	_, _, err = manager.CreateTab(CreateTabRequest{Title: "rem", RepoID: repo.ID, Shell: true})
	assertTabRejection(t, err, "only local sessions support user-managed tabs")
}
