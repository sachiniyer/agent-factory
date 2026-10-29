package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedWorkspace builds a session worktree the way a first-time create does,
// commits a file on its branch, and returns a FRESH hosted instance — what a
// restarted agent-server constructs — for it.
func hostedWorkspace(t *testing.T) (*Instance, *git.GitWorktree, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := initInPlaceRepo(t, "main")
	const branch = "af/restart-adopt"
	gw, _, err := git.NewGitWorktreeNamed(repo, "restart-adopt", branch)
	require.NoError(t, err)
	require.NoError(t, gw.Setup())
	require.NoError(t, os.WriteFile(filepath.Join(gw.GetWorktreePath(), "work.txt"), []byte("agent work"), 0o644))
	gitOut(t, gw.GetWorktreePath(), "add", "work.txt")
	gitOut(t, gw.GetWorktreePath(), "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "agent work")
	inst := &Instance{Title: "restart-adopt", Path: repo, branchName: branch, hostedRemotely: true}
	return inst, gw, branch
}

func TestAdoptExistingWorktreeBindsAPresentWorktreeOnItsBranch(t *testing.T) {
	inst, gw, branch := hostedWorkspace(t)
	require.Nil(t, inst.gitWorktree, "a restarted agent-server starts with no handle")

	require.NoError(t, inst.adoptExistingWorktree())
	require.NotNil(t, inst.gitWorktree)
	assert.Equal(t, gw.GetWorktreePath(), inst.gitWorktree.GetWorktreePath())
	assert.Equal(t, branch, inst.Branch)
}

func TestAdoptExistingWorktreeRebuildsAGoneWorktreeFromItsBranchFirst(t *testing.T) {
	inst, gw, _ := hostedWorkspace(t)
	path := gw.GetWorktreePath()
	// Deleted outside af, registration left behind (no prune): the stale entry
	// is what tells "gone" from "never had one", and the rebuild clears it.
	require.NoError(t, os.RemoveAll(path))

	require.NoError(t, inst.adoptExistingWorktree())
	require.NotNil(t, inst.gitWorktree)
	assert.Equal(t, path, inst.gitWorktree.GetWorktreePath(), "rebuilt where the session's worktree was")
	data, err := os.ReadFile(filepath.Join(path, "work.txt"))
	require.NoError(t, err, "the rebuilt worktree carries the branch's committed work")
	assert.Equal(t, "agent work", string(data))
}

func TestAdoptExistingWorktreeRefusesWhenTheBranchCannotBeRestored(t *testing.T) {
	t.Run("gone and its branch gone too", func(t *testing.T) {
		inst, gw, branch := hostedWorkspace(t)
		require.NoError(t, os.RemoveAll(gw.GetWorktreePath()))
		gitOut(t, inst.Path, "worktree", "prune")
		gitOut(t, inst.Path, "branch", "-D", branch)

		require.Error(t, inst.adoptExistingWorktree())
		assert.Nil(t, inst.gitWorktree)
		_, err := os.Stat(gw.GetWorktreePath())
		assert.True(t, os.IsNotExist(err), "a missing branch must never be replaced by a fresh empty worktree")
	})
}

func TestAdoptExistingWorktreeIsNotForPersistedOrLocalInstances(t *testing.T) {
	inst, _, _ := hostedWorkspace(t)
	inst.hostedRemotely = false
	require.NoError(t, inst.adoptExistingWorktree())
	assert.Nil(t, inst.gitWorktree, "only a hosted instance derives its handle at restore")
}

// A deleted worktree whose agent survived is rebuilt only after that agent is
// stopped: rebuilding under it leaves a stale writer attached to the new tree
// (#5995 review).
func TestAdoptExistingWorktreeStopsASurvivingAgentBeforeRebuilding(t *testing.T) {
	inst, gw, _ := hostedWorkspace(t)
	path := gw.GetWorktreePath()
	require.NoError(t, os.RemoveAll(path))

	var mu sync.Mutex
	killed, treeAtKill := false, "unset"
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			mu.Lock()
			defer mu.Unlock()
			args := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(args, "kill-session"):
				killed = true
				if _, err := os.Stat(path); os.IsNotExist(err) {
					treeAtKill = "absent"
				} else {
					treeAtKill = "present"
				}
			case strings.Contains(args, "has-session"):
				if killed {
					return &exec.ExitError{}
				}
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte(""), nil },
	}
	inst.setTmuxLocked(tmux.NewTmuxSessionWithDeps("restart-adopt", "claude", nil, cmdExec))

	require.NoError(t, inst.adoptExistingWorktree())
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, killed, "the surviving agent must be stopped")
	assert.Equal(t, "absent", treeAtKill, "the agent must be stopped BEFORE the tree is rebuilt")
	_, err := os.Stat(filepath.Join(path, "work.txt"))
	assert.NoError(t, err, "the worktree is rebuilt once the agent is stopped")
}

// The restart stops a new agent-server only BEFORE it has been asked to
// Provision (reattach steps 4 to 6). That is safe because the agent-server's
// SIGTERM path (daemon/agentserver_headless_workspace.go shutdownWorkspace ->
// AgentServer.Kill(false) -> LocalBackend.Kill -> teardownTabs) acts only on
// what the instance holds, and a never-provisioned instance holds no worktree
// handle or tmux binding. Pinned here so a change to Kill that starts deriving
// a worktree to delete from the instance's title/branch would fail this, not a
// user's surviving work.
func TestKillOfANeverProvisionedHostedInstanceDeletesNoWorkspace(t *testing.T) {
	_, gw, branch := hostedWorkspace(t)
	path := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(path, "uncommitted.txt"), []byte("surviving work"), 0o644))

	// The instance a restarted agent-server builds (daemon/agentserver_headless.go):
	// local runtime, hosted, the recorded branch, and NOTHING provisioned yet.
	fresh, err := NewInstance(InstanceOptions{
		Title: "restart-adopt", Path: gw.GetRepoPath(), Program: "claude",
		Backend: BackendLocal, HostedRemotely: true, BranchName: branch,
	})
	require.NoError(t, err)
	require.Nil(t, fresh.gitWorktree, "precondition: nothing provisioned")

	_ = fresh.AgentServer().Kill(false)

	data, rerr := os.ReadFile(filepath.Join(path, "uncommitted.txt"))
	require.NoError(t, rerr, "the surviving worktree must be untouched")
	assert.Equal(t, "surviving work", string(data))
	assert.Contains(t, gitOut(t, gw.GetRepoPath(), "branch", "--list", branch), branch, "and so must its branch")
}

// Control for the test above: the same Kill DOES delete the worktree once the
// instance holds its handle, so the unprovisioned result is the instance's
// emptiness and not a Kill that never deletes anything.
func TestKillOfAProvisionedHostedInstanceDeletesItsWorkspace(t *testing.T) {
	_, gw, branch := hostedWorkspace(t)
	path := gw.GetWorktreePath()
	inst, err := NewInstance(InstanceOptions{
		Title: "restart-adopt", Path: gw.GetRepoPath(), Program: "claude",
		Backend: BackendLocal, HostedRemotely: true, BranchName: branch,
	})
	require.NoError(t, err)
	inst.mu.Lock()
	inst.setGitWorktreeLocked(gw)
	inst.mu.Unlock()

	_ = inst.AgentServer().Kill(false)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "an instance that holds the worktree deletes it on Kill (the reason a post-Provision server is never signalled)")
}

// Git listing no worktree on the session's branch is not proof the workspace is
// gone: the checkout may have been switched to another branch with its work still
// on disk. Adoption refuses, stops nothing, and rebuilds nothing (#5995 review).
func TestAdoptExistingWorktreeRefusesAWorktreeSwitchedToAnotherBranch(t *testing.T) {
	inst, gw, _ := hostedWorkspace(t)
	path := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(path, "uncommitted.txt"), []byte("surviving work"), 0o644))
	gitOut(t, path, "checkout", "-b", "other-branch")

	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "kill-session") {
				killed = true
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte(""), nil },
	}
	inst.setTmuxLocked(tmux.NewTmuxSessionWithDeps("restart-adopt", "claude", nil, cmdExec))

	err := inst.adoptExistingWorktree()
	require.Error(t, err)
	assert.Nil(t, inst.gitWorktree, "nothing is adopted")
	assert.False(t, killed, "the surviving agent is not stopped on an unproven absence")
	data, rerr := os.ReadFile(filepath.Join(path, "uncommitted.txt"))
	require.NoError(t, rerr, "the surviving work is untouched")
	assert.Equal(t, "surviving work", string(data))
	entries, _ := filepath.Glob(path + "-*")
	assert.Empty(t, entries, "no second checkout was created")
}
