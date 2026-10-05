package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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

// FenceUnverifiedWorktree's identity-unknown stall must settle once the path is
// conclusively absent — otherwise every later claim (archive, restore) and every
// cleanup (kill) refuses on the same unknown and the row is stranded for good —
// and must keep fencing while the path exists or cannot be answered (#5102).
func TestIdentityUnknownStallSettlesOnlyOnProvenAbsence(t *testing.T) {
	fenced := func(t *testing.T) (*GitWorktree, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "wt")
		require.NoError(t, os.Mkdir(path, 0o755))
		gw := presenceTestWorktree(t, path)
		require.ErrorIs(t, gw.FenceUnverifiedWorktree(errors.New("reappeared")), ErrRelocateStateUnknown)
		require.True(t, gw.HasUnresolvedRelocation())
		return gw, path
	}

	t.Run("absent path: the claim discharges the fence and answers plain ENOENT", func(t *testing.T) {
		gw, path := fenced(t)
		require.NoError(t, os.Remove(path))
		_, err := gw.ClaimRelocationSource()
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.NotErrorIs(t, err, ErrRelocateStateUnknown, "the gone route must be reachable again")
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("absent path: the discharge cleanup shares clears the fence", func(t *testing.T) {
		gw, path := fenced(t)
		require.NoError(t, os.Remove(path))
		gw.relocationMu.Lock()
		assert.True(t, gw.settleAbsentIdentityUnknownStallLocked())
		gw.relocationMu.Unlock()
		assert.False(t, gw.HasUnresolvedRelocation())
	})

	t.Run("present path keeps fencing", func(t *testing.T) {
		gw, _ := fenced(t)
		gw.relocationMu.Lock()
		assert.False(t, gw.settleAbsentIdentityUnknownStallLocked())
		gw.relocationMu.Unlock()
		assert.True(t, gw.HasUnresolvedRelocation())
	})

	t.Run("unanswerable path keeps fencing", func(t *testing.T) {
		gw, path := fenced(t)
		require.NoError(t, os.Remove(path))
		stubPresenceLstat(t, path, &os.PathError{Op: "lstat", Path: path, Err: syscall.EACCES})
		_, err := gw.ClaimRelocationSource()
		require.Error(t, err)
		assert.True(t, gw.HasUnresolvedRelocation(), "only a proven absence may discharge the fence")
	})

	t.Run("an identity-qualified record is never discharged this way", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wt")
		gw := presenceTestWorktree(t, path)
		require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
			State: RelocationRecoveryStalled, IdentityKnown: true, Device: 1, Inode: 2,
		}))
		gw.relocationMu.Lock()
		assert.False(t, gw.settleAbsentIdentityUnknownStallLocked())
		gw.relocationMu.Unlock()
		assert.True(t, gw.HasUnresolvedRelocation())
	})
}

// A refused claim on an unverified directory must not record that directory's
// identity as the worktree's: removing it afterwards would then read as a
// vanished identity and strand the row. Returned unverified, the fence stays
// identity-unknown and settles once the path is gone (#5102).
func TestReturnClaimUnverifiedKeepsTheFenceSettleable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.Mkdir(path, 0o755))
	gw := presenceTestWorktree(t, path)
	require.ErrorIs(t, gw.FenceUnverifiedWorktree(errors.New("reappeared")), ErrRelocateStateUnknown)

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "premise: the stalled fence re-resolves a present path")
	gw.ReturnClaimUnverified(claim)
	recovery, fenced := gw.GetRelocationRecovery()
	require.True(t, fenced)
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)
	assert.False(t, recovery.IdentityKnown, "the unverified directory's identity must not be recorded")

	require.NoError(t, os.Remove(path))
	_, err = gw.ClaimRelocationSource()
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NotErrorIs(t, err, ErrRelocateStateUnknown)
	assert.False(t, gw.HasUnresolvedRelocation())

	t.Run("a record-free claim leaves no record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wt")
		require.NoError(t, os.Mkdir(path, 0o755))
		gw := presenceTestWorktree(t, path)
		claim, err := gw.ClaimRelocationSource()
		require.NoError(t, err)
		gw.ReturnClaimUnverified(claim)
		assert.False(t, gw.HasUnresolvedRelocation())
	})
}
