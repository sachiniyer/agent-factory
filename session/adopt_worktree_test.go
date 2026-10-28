package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
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
