package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestClaimRelocationSource_RecordedStallIsUnknown(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	t.Cleanup(SetRelocationIdentityErrorForTest(src, context.DeadlineExceeded))

	_, err := gw.ClaimRelocationSource()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRelocateStateUnknown,
		"an error which creates durable recovery state must be classified so every caller persists it")
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)
}

func TestRelocate_AnsweredPartialFastMoveRepairsDestination(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })
	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, source, destination string) error {
		require.NoError(t, os.Rename(source, destination))
		return errors.New("registration update answered with an error")
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })
	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepair = previousRepair })
	previousSubmoduleRepair := worktreeRepairSubmodules
	worktreeRepairSubmodules = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepairSubmodules = previousSubmoduleRepair })

	require.NoError(t, gw.MoveWorktree(dest),
		"an answered fast-path error may still have committed the rename; fallback must select and repair that identity")
	assert.NoDirExists(t, src)
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
	assert.False(t, gw.HasUnresolvedRelocation())
}

func TestRelocate_CrossDeviceRepairTimeoutRetainsDestinationIdentity(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })
	previousMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error { return errors.New("force fallback") }
	t.Cleanup(func() { worktreeMoveFast = previousMove })
	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })
	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return context.DeadlineExceeded }
	t.Cleanup(func() { worktreeRepair = previousRepair })

	err := gw.MoveWorktree(dest)
	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	require.True(t, recovery.IdentityKnown,
		"after copy commits, a repair timeout must retain the copied directory identity rather than an unqualified path")
	destinationIdentity, identityErr := inspectRelocationPathIdentity(dest)
	require.NoError(t, identityErr)
	assert.True(t, recovery.identity().same(destinationIdentity),
		"the retained identity must describe the copied destination, whose inode differs from the source")
}

func TestRelocate_AnsweredRepairFailureRetainsDestinationIdentity(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return true, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })
	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return errors.New("registration repair answered with an error") }
	t.Cleanup(func() { worktreeRepair = previousRepair })

	err := gw.MoveWorktree(dest)
	require.ErrorIs(t, err, ErrRelocateStateUnknown,
		"a restore caller must persist the new location even when repair answered instead of timing out")
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok, "the committed destination must remain a retryable recovery handle")
	destinationIdentity, identityErr := inspectRelocationPathIdentity(dest)
	require.NoError(t, identityErr)
	assert.True(t, recovery.identity().same(destinationIdentity))
}

func TestRelocate_SubmoduleRepairTimeoutRetainsRecovery(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return true, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })
	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepair = previousRepair })
	previousSubmoduleRepair := worktreeRepairSubmodules
	worktreeRepairSubmodules = func(*GitWorktree, string) error { return context.DeadlineExceeded }
	t.Cleanup(func() { worktreeRepairSubmodules = previousSubmoduleRepair })

	err := gw.MoveWorktree(dest)
	require.ErrorIs(t, err, ErrRelocateStateUnknown,
		"a timed-out best-effort repair still proves the destination filesystem stalled")
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok, "a successful main repair must not clear the later submodule-repair stall")
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)

	submoduleRetryCalls := 0
	worktreeRepairSubmodules = func(*GitWorktree, string) error {
		submoduleRetryCalls++
		return nil
	}
	require.NoError(t, gw.MoveWorktree(dest))
	assert.Equal(t, 1, submoduleRetryCalls,
		"a retry which recovers bytes at the destination must finish the interrupted submodule repair")
}
