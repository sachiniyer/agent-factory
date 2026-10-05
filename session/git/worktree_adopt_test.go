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

// Restore adopts a landed move only where restore itself could have put it, so
// a checkout of the branch the user made elsewhere is never claimed as af's.
func TestIsRestorePlacement(t *testing.T) {
	_, repoRoot, _ := archiveTestWorktree(t)
	base, err := RestoreWorktreePath(repoRoot, "my-session", "af/my-session")
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		candidate string
		want      bool
	}{
		{"the placement base", base, true},
		{"a collision variant", base + "-3", true},
		{"a non-numeric suffix", base + "-old", false},
		{"suffix 1 is never chosen", base + "-1", false},
		{"a different parent", filepath.Join(t.TempDir(), filepath.Base(base)), false},
		{"a different leaf", filepath.Join(filepath.Dir(base), "elsewhere"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IsRestorePlacement(repoRoot, "my-session", "af/my-session", tc.candidate)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
