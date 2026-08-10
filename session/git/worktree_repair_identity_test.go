package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// prepareDestinationRepairRetry leaves a completed fast-path rename behind a
// timeout, so the retry resolves the identity-qualified destination and enters
// repairDestination without moving the bytes a second time.
func prepareDestinationRepairRetry(t *testing.T) (*GitWorktree, string) {
	t.Helper()
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, src, dest string) error {
		require.NoError(t, os.Rename(src, dest))
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	require.ErrorIs(t, gw.MoveWorktree(dest), ErrRelocateStateUnknown)
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
	return gw, dest
}

func replaceRepairDestination(t *testing.T, dest, movedAside string) {
	t.Helper()
	require.NoError(t, os.Rename(dest, movedAside))
	require.NoError(t, os.Mkdir(dest, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "replacement.txt"), []byte("replacement"), 0o644))
}

func assertStaleRepairClaimMatches(t *testing.T, gw *GitWorktree, movedAside string) {
	t.Helper()
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok, "a replaced repair destination must remain unresolved")
	assert.Equal(t, RelocationRecoveryClaimStale, recovery.State)
	realIdentity, err := inspectRelocationPathIdentity(movedAside)
	require.NoError(t, err)
	assert.True(t, recovery.identity().same(realIdentity),
		"recovery must retain the real worktree identity, not accept its pathname replacement")
}

func TestRelocate_RetryRevalidatesDestinationBeforeSubmoduleRepair(t *testing.T) {
	gw, dest := prepareDestinationRepairRetry(t)
	movedAside := dest + "-real-worktree"

	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error {
		replaceRepairDestination(t, dest, movedAside)
		return nil
	}
	t.Cleanup(func() { worktreeRepair = previousRepair })

	previousSubmoduleRepair := worktreeRepairSubmodules
	submoduleAttempted := false
	worktreeRepairSubmodules = func(*GitWorktree, string) error {
		submoduleAttempted = true
		return os.WriteFile(filepath.Join(dest, "submodule-mutated.txt"), []byte("mutated"), 0o644)
	}
	t.Cleanup(func() { worktreeRepairSubmodules = previousSubmoduleRepair })

	err := gw.MoveWorktree(dest)
	assert.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.False(t, submoduleAttempted,
		"submodule repair must not run after registration repair loses the claimed destination identity")
	assert.NoFileExists(t, filepath.Join(dest, "submodule-mutated.txt"),
		"the raced-in replacement must never be mutated")
	assert.FileExists(t, filepath.Join(dest, "replacement.txt"))
	assert.FileExists(t, filepath.Join(movedAside, "dirty.txt"))
	assertStaleRepairClaimMatches(t, gw, movedAside)
}

func TestRelocate_RetryRevalidatesDestinationBeforeSettlement(t *testing.T) {
	gw, dest := prepareDestinationRepairRetry(t)
	movedAside := dest + "-real-worktree"

	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepair = previousRepair })

	previousSubmoduleRepair := worktreeRepairSubmodules
	worktreeRepairSubmodules = func(*GitWorktree, string) error {
		replaceRepairDestination(t, dest, movedAside)
		return nil
	}
	t.Cleanup(func() { worktreeRepairSubmodules = previousSubmoduleRepair })

	err := gw.MoveWorktree(dest)
	assert.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.FileExists(t, filepath.Join(dest, "replacement.txt"))
	assert.FileExists(t, filepath.Join(movedAside, "dirty.txt"))
	assertStaleRepairClaimMatches(t, gw, movedAside)
}
