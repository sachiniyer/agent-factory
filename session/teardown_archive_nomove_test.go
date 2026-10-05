package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// A vanished worktree that reappears before the archive's no-move step holds a
// directory nobody verified. The step must refuse as unknown AND fence the
// record, or the daemon's drop to Lost lets recovery respawn the agent into it
// (#5102).
func TestConfirmGoneForArchive(t *testing.T) {
	newGW := func(t *testing.T) (*git.GitWorktree, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "wt")
		gw, err := git.NewGitWorktreeFromStorage(filepath.Join(filepath.Dir(path), "repo"), path, "gone", "af/gone", "", false, true)
		require.NoError(t, err)
		return gw, path
	}

	t.Run("still absent proceeds", func(t *testing.T) {
		gw, _ := newGW(t)
		state, err := confirmGoneForArchive(gw, "gone")
		require.NoError(t, err)
		assert.Equal(t, stateKnown, state)
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("reappeared is unknown and fenced", func(t *testing.T) {
		gw, path := newGW(t)
		require.NoError(t, os.Mkdir(path, 0o755))
		state, err := confirmGoneForArchive(gw, "gone")
		require.Error(t, err)
		assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
		assert.Equal(t, stateUnknown, state, "finalize must not run over an unverified directory")
		assert.True(t, gw.HasUnresolvedRelocation(),
			"the record must be fenced so Lost recovery does not respawn into it")
	})
}

// An archived worktree the user moved with `git worktree move` reads Absent at
// its recorded path, but git has the branch live elsewhere. Rename must refuse
// before it touches the branch, so the user's checkout is left exactly as it was.
func TestRenameArchived_RefusesAUserMovedArchive(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	wtPath := filepath.Join(filepath.Dir(repoRoot), "archived-title")
	runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/archived-title", wtPath)
	elsewhere := filepath.Join(t.TempDir(), "users-copy")
	runRenameGit(t, repoRoot, "worktree", "move", wtPath, elsewhere)

	inst, err := NewInstance(InstanceOptions{Title: "archived-title", Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "archived-title", "af/archived-title", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.liveness = LiveArchived

	err = inst.RenameArchived("archived-title-renamed", filepath.Join(filepath.Dir(repoRoot), "renamed"), "af/archived-title-renamed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "moved outside af")
	assert.Equal(t, "archived-title", inst.Title)
	assert.Equal(t, wtPath, gw.GetWorktreePath(), "the record must not be re-aimed")
	assert.Equal(t, "af/archived-title", gw.GetBranchName())
	runRenameGit(t, repoRoot, "show-ref", "--verify", "refs/heads/af/archived-title")
	assert.DirExists(t, elsewhere)
}
