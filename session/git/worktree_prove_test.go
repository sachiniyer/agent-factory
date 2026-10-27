package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A flagged row's directory re-qualifies only on git's own proof — the
// session's branch registered at the recorded path, back-bound into this repo —
// and only for the directory that was proven (#5102).
func TestProveRecordedWorktree(t *testing.T) {
	t.Run("a registered checkout of the session's branch is proven and pinned", func(t *testing.T) {
		gw, _, wtPath := archiveTestWorktree(t)
		require.NoError(t, gw.ProveRecordedWorktree())
		require.NoError(t, gw.ReconfirmAdoptedWorktree(), "the pinned directory is still the one at the path")
		assert.Equal(t, wtPath, gw.GetWorktreePath())
	})

	t.Run("a plain directory is refused", func(t *testing.T) {
		gw, _, wtPath := archiveTestWorktree(t)
		require.NoError(t, os.RemoveAll(wtPath))
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		require.Error(t, gw.ProveRecordedWorktree())
	})

	t.Run("a checkout of another branch is refused", func(t *testing.T) {
		gw, repoRoot, wtPath := archiveTestWorktree(t)
		require.NoError(t, os.RemoveAll(wtPath))
		runGitInPlaceTest(t, repoRoot, "worktree", "prune")
		runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "someone/else", wtPath)
		require.Error(t, gw.ProveRecordedWorktree())
	})

	t.Run("a directory swapped in after the proof fails the reconfirm and fences", func(t *testing.T) {
		gw, _, wtPath := archiveTestWorktree(t)
		require.NoError(t, gw.ProveRecordedWorktree())
		aside := wtPath + "-aside"
		require.NoError(t, os.Rename(wtPath, aside))
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		err := gw.ReconfirmAdoptedWorktree()
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRelocateStateUnknown))
		assert.True(t, gw.HasUnresolvedRelocation(), "the swap leaves the record fenced")
	})

	t.Run("an outstanding relocation record refuses", func(t *testing.T) {
		gw, _, _ := archiveTestWorktree(t)
		_ = gw.FenceUnverifiedWorktree(errors.New("test"))
		err := gw.ProveRecordedWorktree()
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRelocateStateUnknown))
	})
}

// An archive or restore that found a directory back at a flagged row's path
// verifies it against the claim it took: the proof must hold AND the claimed
// directory must still be the one there, and nothing is recorded either way.
func TestVerifyClaimedWorktree(t *testing.T) {
	t.Run("the session's own checkout put back is verified", func(t *testing.T) {
		gw, repoRoot, wtPath := archiveTestWorktree(t)
		elsewhere := filepath.Join(filepath.Dir(repoRoot), "moved-away")
		runGitInPlaceTest(t, repoRoot, "worktree", "move", wtPath, elsewhere)
		runGitInPlaceTest(t, repoRoot, "worktree", "move", elsewhere, wtPath)
		claim, err := gw.ClaimRelocationSource()
		require.NoError(t, err)
		require.NoError(t, gw.VerifyClaimedWorktree(claim))
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("a stranger directory is refused without recording it", func(t *testing.T) {
		gw, _, wtPath := archiveTestWorktree(t)
		require.NoError(t, os.RemoveAll(wtPath))
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		claim, err := gw.ClaimRelocationSource()
		require.NoError(t, err)
		require.Error(t, gw.VerifyClaimedWorktree(claim))
		_, recovery, unresolved := gw.RelocationSnapshot()
		assert.False(t, unresolved, "a refused verification must not record the stranger's identity: %+v", recovery)
	})

	t.Run("a directory swapped after the claim is refused", func(t *testing.T) {
		gw, _, wtPath := archiveTestWorktree(t)
		claim, err := gw.ClaimRelocationSource()
		require.NoError(t, err)
		// Same registration and back-binding, different directory: the proof
		// alone would pass, the claimed identity does not.
		require.NoError(t, os.Rename(wtPath, wtPath+"-old"))
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		gitFile, readErr := os.ReadFile(filepath.Join(wtPath+"-old", ".git"))
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(wtPath, ".git"), gitFile, 0o644))
		require.Error(t, gw.VerifyClaimedWorktree(claim))
	})
}
