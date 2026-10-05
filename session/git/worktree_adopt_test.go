package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An archive or restore whose move landed but was never recorded leaves the
// record naming a vacated path while git registers the bytes at the move's
// destination (#5102). That destination is adopted; nothing else is.
func TestAdoptLandedWorktreeMove(t *testing.T) {
	t.Run("git-registered move destination on this branch is adopted", func(t *testing.T) {
		gw, repoRoot, src := archiveTestWorktree(t)
		dest := filepath.Join(filepath.Dir(repoRoot), "landed")
		runGitInPlaceTest(t, repoRoot, "worktree", "move", src, dest)

		registered, listed, err := gw.RegisteredPathForBranch()
		require.NoError(t, err)
		require.True(t, listed)
		assert.Equal(t, filepath.Base(dest), filepath.Base(registered))

		require.NoError(t, gw.AdoptLandedWorktreeMove(dest))
		assert.Equal(t, dest, gw.GetWorktreePath())
		data, err := os.ReadFile(filepath.Join(dest, "dirty.txt"))
		require.NoError(t, err, "adoption must not touch the bytes")
		assert.Equal(t, "uncommitted work", string(data))
	})

	t.Run("an unregistered occupant is refused", func(t *testing.T) {
		gw, repoRoot, src := archiveTestWorktree(t)
		require.NoError(t, os.RemoveAll(src))
		dest := filepath.Join(filepath.Dir(repoRoot), "stranger")
		require.NoError(t, os.Mkdir(dest, 0o755))
		require.Error(t, gw.AdoptLandedWorktreeMove(dest))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("a worktree on another branch is refused", func(t *testing.T) {
		gw, repoRoot, src := archiveTestWorktree(t)
		require.NoError(t, os.RemoveAll(src))
		dest := filepath.Join(filepath.Dir(repoRoot), "other-branch")
		runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "someone/else", dest)
		require.Error(t, gw.AdoptLandedWorktreeMove(dest))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("a recorded path that still exists is refused", func(t *testing.T) {
		gw, repoRoot, src := archiveTestWorktree(t)
		dest := filepath.Join(filepath.Dir(repoRoot), "landed")
		runGitInPlaceTest(t, repoRoot, "worktree", "move", src, dest)
		require.NoError(t, os.Mkdir(src, 0o755))
		require.Error(t, gw.AdoptLandedWorktreeMove(dest))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("a branch with no live checkout is not listed", func(t *testing.T) {
		gw, repoRoot, src := archiveTestWorktree(t)
		runGitInPlaceTest(t, repoRoot, "worktree", "remove", "--force", src)
		_, listed, err := gw.RegisteredPathForBranch()
		require.NoError(t, err)
		assert.False(t, listed)
	})
}

// The adoption proof is about one directory. Between it and the archive commit
// the caller tears down editors, hooks and tmux; a directory swapped in under
// the same name in that window must be caught by device/inode, and must fence
// the record so recovery does not act on what may be unrelated files (#5102).
func TestReconfirmAdoptedWorktree(t *testing.T) {
	adopt := func(t *testing.T) (*GitWorktree, string) {
		t.Helper()
		gw, repoRoot, src := archiveTestWorktree(t)
		dest := filepath.Join(filepath.Dir(repoRoot), "landed")
		runGitInPlaceTest(t, repoRoot, "worktree", "move", src, dest)
		require.NoError(t, gw.AdoptLandedWorktreeMove(dest))
		return gw, dest
	}

	t.Run("the proven directory reconfirms", func(t *testing.T) {
		gw, _ := adopt(t)
		require.NoError(t, gw.ReconfirmAdoptedWorktree())
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("a swapped directory is refused and fenced", func(t *testing.T) {
		gw, dest := adopt(t)
		require.NoError(t, os.Rename(dest, dest+".real"))
		require.NoError(t, os.Mkdir(dest, 0o755))
		err := gw.ReconfirmAdoptedWorktree()
		require.ErrorIs(t, err, ErrRelocateStateUnknown)
		assert.True(t, gw.HasUnresolvedRelocation(),
			"the record must be fenced so respawn and cleanup refuse the swapped directory")
		assert.Error(t, gw.ReconfirmAdoptedWorktree(), "the proof is spent once it failed")
	})

	t.Run("a vanished directory is reported without a fence", func(t *testing.T) {
		gw, dest := adopt(t)
		require.NoError(t, os.Rename(dest, dest+".moved"))
		err := gw.ReconfirmAdoptedWorktree()
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("no adoption on record is an error, not a pass", func(t *testing.T) {
		gw, _, _ := archiveTestWorktree(t)
		assert.Error(t, gw.ReconfirmAdoptedWorktree())
	})
}
