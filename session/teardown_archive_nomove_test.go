package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// An archived row flagged worktree-missing whose path was later recreated holds
// an unverified directory there. Rename — run automatically on title reuse —
// must refuse rather than move it, and must not rename the branch first (#5102).
func TestRenameArchived_FlaggedRowWithARecreatedPathRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	wtPath := filepath.Join(filepath.Dir(repoRoot), "flagged-title")
	runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/flagged-title", wtPath)
	require.NoError(t, os.RemoveAll(wtPath))
	require.NoError(t, os.Mkdir(wtPath, 0o755))
	stranger := filepath.Join(wtPath, "not-the-session.txt")
	require.NoError(t, os.WriteFile(stranger, []byte("unverified"), 0o644))

	inst, err := NewInstance(InstanceOptions{Title: "flagged-title", Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "flagged-title", "af/flagged-title", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.liveness = LiveArchived
	inst.SetWorktreeMissing(WorktreeMissingArchivedReason(wtPath))

	dest := filepath.Join(filepath.Dir(repoRoot), "flagged-title-renamed")
	err = inst.RenameArchived("flagged-title-renamed", dest, "af/flagged-title-renamed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
	assert.Equal(t, "flagged-title", inst.Title)
	assert.Equal(t, wtPath, gw.GetWorktreePath())
	assert.Equal(t, "af/flagged-title", gw.GetBranchName())
	runRenameGit(t, repoRoot, "show-ref", "--verify", "refs/heads/af/flagged-title")
	data, err := os.ReadFile(stranger)
	require.NoError(t, err)
	assert.Equal(t, "unverified", string(data))
	assert.NoDirExists(t, dest, "nothing may be moved to the renamed path")
}

// Restore of an archived-missing row rebuilds the worktree from the kept
// branch at the chosen destination (#5102); a failed rebuild re-aims the record
// back at the recorded path so the row stays the archived-missing row it was.
func TestRebuildAbsentArchivedWorktree(t *testing.T) {
	setup := func(t *testing.T, title string) (*Instance, *git.GitWorktree, string, string) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
		wtPath := filepath.Join(filepath.Dir(repoRoot), title)
		runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/"+title, wtPath)
		require.NoError(t, os.RemoveAll(wtPath))
		inst, err := NewInstance(InstanceOptions{Title: title, Path: repoRoot, Program: "claude"})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/"+title, "", false, true)
		require.NoError(t, err)
		inst.gitWorktree = gw
		inst.liveness = LiveArchived
		inst.SetWorktreeMissing(WorktreeMissingArchivedReason(wtPath))
		return inst, gw, repoRoot, wtPath
	}

	t.Run("rebuilds from the branch at dest", func(t *testing.T) {
		inst, gw, repoRoot, _ := setup(t, "rebuilt")
		dest := filepath.Join(filepath.Dir(repoRoot), "rebuilt-restored")
		require.NoError(t, inst.RebuildAbsentArchivedWorktree(dest))
		assert.Equal(t, dest, gw.GetWorktreePath())
		out, err := exec.Command("git", "-C", dest, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Equal(t, "af/rebuilt", strings.TrimSpace(string(out)))
		// The flag follows the pointer until the caller's restore commits, and
		// the rebuilt directory is pinned for the caller's boundary checks.
		missing, reason := inst.WorktreeMissing()
		assert.True(t, missing)
		assert.Equal(t, WorktreeMissingPlacingReason(dest), reason)
		require.NoError(t, inst.ReconfirmPlacedWorktree())
		require.NoError(t, os.Rename(dest, dest+"-swapped"))
		require.NoError(t, os.Mkdir(dest, 0o755))
		require.Error(t, inst.ReconfirmPlacedWorktree(), "a directory swapped in after the rebuild is refused")
	})

	t.Run("an unflagged row's failed rebuild is left flagged at its recorded path", func(t *testing.T) {
		inst, gw, repoRoot, wtPath := setup(t, "unflagged")
		inst.ClearWorktreeMissing()
		runRenameGit(t, repoRoot, "worktree", "prune")
		runRenameGit(t, repoRoot, "branch", "-D", "af/unflagged")
		dest := filepath.Join(filepath.Dir(repoRoot), "unflagged-restored")
		require.Error(t, inst.RebuildAbsentArchivedWorktree(dest))
		assert.Equal(t, wtPath, gw.GetWorktreePath())
		missing, reason := inst.WorktreeMissing()
		assert.True(t, missing, "a row that entered unflagged must leave the route flagged")
		assert.Equal(t, WorktreeMissingDeletedReason(wtPath), reason)
	})

	t.Run("a missing branch fails and re-aims back", func(t *testing.T) {
		inst, gw, repoRoot, wtPath := setup(t, "branchless")
		runRenameGit(t, repoRoot, "worktree", "prune")
		runRenameGit(t, repoRoot, "branch", "-D", "af/branchless")
		dest := filepath.Join(filepath.Dir(repoRoot), "branchless-restored")
		require.Error(t, inst.RebuildAbsentArchivedWorktree(dest))
		assert.Equal(t, wtPath, gw.GetWorktreePath())
		assert.NoDirExists(t, dest)
	})

	t.Run("a recorded path that exists is not repointed", func(t *testing.T) {
		inst, gw, repoRoot, wtPath := setup(t, "present")
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		dest := filepath.Join(filepath.Dir(repoRoot), "present-restored")
		require.Error(t, inst.RebuildAbsentArchivedWorktree(dest))
		assert.Equal(t, wtPath, gw.GetWorktreePath())
		assert.NoDirExists(t, dest)
	})
}
