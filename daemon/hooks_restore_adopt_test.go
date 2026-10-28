//go:build linux

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/session"
)

// installRestoreSurvivorSystemctl scripts the user manager so the test decides
// whether a hook scope is still active. It reports one live scope under prefix
// until release exists, and answers everything else the way a manager with
// nothing loaded would — a restore consults systemctl for other reasons, and a
// shim that failed those would fail this test for the wrong reason.
func installRestoreSurvivorSystemctl(t *testing.T, prefix, release string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *list-units*)
    if [ ! -f '` + release + `' ]; then
      printf '%s\n' '` + prefix + `-g0-0.scope loaded active running Hook'
    fi
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write systemctl shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// claimDaemonProcessForRestore makes RunningDaemonProcess() answer true, which
// is the gate on every hook-scope behaviour: only the process systemd started as
// the daemon puts hooks in scopes, so only it has scopes to find on restore.
func claimDaemonProcessForRestore(t *testing.T) {
	t.Helper()
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
}

func writeRepoHookConfig(t *testing.T, repoID string, commands []string) {
	t.Helper()
	configDir, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	dir := filepath.Join(configDir, "repos", repoID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo config: %v", err)
	}
	data, err := json.MarshalIndent(&config.RepoConfig{PostWorktreeCommands: commands}, "", "  ")
	if err != nil {
		t.Fatalf("marshal repo config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.RepoConfigFileName), data, 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
}

func restoredInstanceByTitle(t *testing.T, manager *Manager, title string) *session.Instance {
	t.Helper()
	for _, instance := range manager.InstancesSnapshot() {
		if instance.Title == title {
			return instance
		}
	}
	t.Fatalf("the daemon did not restore session %q", title)
	return nil
}

// TestRestoredSessionShowsHooksStillRunningAfterADaemonRestart is #3682.
//
// #3658 deliberately keeps a daemon-spawned post_worktree_commands run alive
// across a daemon restart: its scope has no dependency edge to the daemon unit,
// so an operator's `make dev_install` is not killed mid-pnpm by every restart
// and every #2212 auto-upgrade. The survivor is then left to finish over an
// INTACT tree — only the paths that rebuild, remove or move the tree stop it.
//
// What the successor daemon does not do is SAY so. hooksCancel, cmd.Wait and the
// process-group pgid all died with the daemon that started the run, so the
// restored session's hook state is empty and every consumer of it — the
// readiness budget that must not charge a slow build against the agent
// (task.WaitForReady), the task-lifecycle teardown that must not move a tree a
// hook is writing into — reads "nothing in flight" while a hook is very much in
// flight.
//
// Two claims, and they are load-bearing in opposite directions:
//
//  1. The restored session reports hooks in flight, through the SAME state a
//     first run uses, so nothing has to learn a second way to ask. This is the
//     red: on master the channel is nil.
//  2. The hook is NOT re-run over the intact tree. That half passes on master
//     (nothing on the restore path starts hooks) and is pinned here so the fix
//     for the first claim cannot buy it by starting a second run — the
//     "executes the operator's post_worktree_commands TWICE over the same path"
//     hazard #2770 exists to prevent.
//
// The record loads inert (StartupStateUnknown) so the test stays off tmux: hook
// adoption is about the hook, not about the session's runtime, and it runs over
// every restored session the same way.
func TestRestoredSessionShowsHooksStillRunningAfterADaemonRestart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const (
		sessionID = "3682a1b2-0000-4000-8000-abcdefabcdef"
		title     = "hooks-still-running"
	)
	prefix := systemdunit.HookScopeUnitPrefix(sessionID)
	if prefix == "" {
		t.Fatal("no scope prefix derives from a session id; this test would prove nothing")
	}
	release := filepath.Join(t.TempDir(), "release")
	installRestoreSurvivorSystemctl(t, prefix, release)
	claimDaemonProcessForRestore(t)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	// The oracle for "a second run" is the operator's side effect actually
	// happening, not a process existing.
	reran := filepath.Join(t.TempDir(), "hook-ran")
	writeRepoHookConfig(t, repo.ID, []string{"printf ran > " + strconv.Quote(reran)})

	worktreePath := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                  sessionID,
		Title:               title,
		Path:                repoPath,
		Status:              session.Lost,
		Liveness:            session.LiveLost,
		StartupStateUnknown: true,
		BackendType:         "local",
		Worktree: session.GitWorktreeData{
			RepoPath:            repoPath,
			WorktreePath:        worktreePath,
			SessionName:         title,
			BranchName:          "af/" + title,
			HookScopeUnitPrefix: prefix,
		},
	}); err != nil {
		t.Fatalf("seed the session record: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	instance := restoredInstanceByTitle(t, manager, title)

	done := instance.PostWorktreeHooksDone()
	if done == nil {
		t.Fatalf("the restored session reports no post-worktree hook in flight while %s-*.scope is still active; "+
			"the survivor #3658 keeps alive on purpose is invisible to every consumer of the session's hook state (#3682)", prefix)
	}
	select {
	case <-done:
		t.Fatalf("the restored session reported its hooks finished while %s-*.scope is still active", prefix)
	default:
	}

	if _, err := os.Stat(reran); err == nil {
		t.Fatal("restoring the session ran post_worktree_commands again over the intact tree; " +
			"the operator's provisioning commands must never execute twice over the same path (#2770)")
	}

	// Once the survivor is gone the session must say so, or the state would be
	// stuck reporting a hook that finished — worse than the gap being fixed.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the survivor: %v", err)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the adopted hook run never reported finishing after its scope exited")
	}
	if _, err := os.Stat(reran); err == nil {
		t.Fatal("post_worktree_commands ran while the adopted survivor was being watched")
	}
}
