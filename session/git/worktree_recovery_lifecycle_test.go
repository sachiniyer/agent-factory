package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

func TestClaimRelocationSource_StalledPrimaryGoneSelectsKnownAlternate(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "stalled-primary")
	alternate := filepath.Join(root, "known-alternate")
	require.NoError(t, os.Mkdir(alternate, 0o755))
	identity, err := inspectRelocationPathIdentity(alternate)
	require.NoError(t, err)

	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "repo"), primary, "fallback", "af/fallback", "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
		State:         RelocationRecoveryStalled,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
	}))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err,
		"a vanished stalled primary must not hide the identity-qualified alternate")
	assert.Equal(t, alternate, claim.Path)
	assert.Equal(t, primary, claim.AlternatePath)
	assert.Equal(t, alternate, gw.GetWorktreePath())
	require.NoError(t, gw.SettleRelocationClaim(claim))
	assert.False(t, gw.HasUnresolvedRelocation())
}

func TestClaimRelocationSource_PrimaryTimeoutSelectsKnownAlternate(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "timing-out-primary")
	alternate := filepath.Join(root, "known-alternate")
	require.NoError(t, os.Mkdir(alternate, 0o755))
	identity, err := inspectRelocationPathIdentity(alternate)
	require.NoError(t, err)

	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "repo"), primary, "fallback", "af/fallback", "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
		State:         RelocationRecoveryMoveUnknown,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
	}))

	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 100 * time.Millisecond
	previousIdentity := relocationPathIdentity
	releasePrimary := make(chan struct{})
	primaryDone := make(chan struct{})
	alternateProbes := 0
	relocationPathIdentity = func(path string) (pathIdentity, error) {
		if path == primary {
			<-releasePrimary
			close(primaryDone)
			return previousIdentity(path)
		}
		if path == alternate {
			alternateProbes++
		}
		return previousIdentity(path)
	}
	t.Cleanup(func() {
		close(releasePrimary)
		<-primaryDone
		relocationPathIdentity = previousIdentity
		relocationIdentityTimeout = previousTimeout
	})

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err,
		"a primary timeout must not suppress the separately bounded alternate probe; alternate probes = %d",
		alternateProbes)
	assert.Equal(t, 1, alternateProbes)
	assert.Equal(t, alternate, claim.Path)
	assert.Equal(t, primary, claim.AlternatePath)
	assert.Equal(t, alternate, gw.GetWorktreePath())
	require.NoError(t, gw.SettleRelocationClaim(claim))
	assert.False(t, gw.HasUnresolvedRelocation())
}

func TestClaimRelocationSource_PrimaryTimeoutPreservesUnprovenAlternate(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "timing-out-primary")
	alternate := filepath.Join(root, "replaced-alternate")
	require.NoError(t, os.Mkdir(alternate, 0o755))
	expected, err := inspectRelocationPathIdentity(alternate)
	require.NoError(t, err)
	require.NoError(t, os.Rename(alternate, alternate+"-captured"))
	require.NoError(t, os.Mkdir(alternate, 0o755))
	replacement, err := inspectRelocationPathIdentity(alternate)
	require.NoError(t, err)
	require.False(t, expected.same(replacement))

	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "repo"), primary, "fallback", "af/fallback", "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
		State:         RelocationRecoveryMoveUnknown,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        expected.device,
		Inode:         expected.inode,
		FileType:      expected.fileType,
	}))

	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 100 * time.Millisecond
	previousIdentity := relocationPathIdentity
	releasePrimary := make(chan struct{})
	primaryDone := make(chan struct{})
	alternateProbes := 0
	relocationPathIdentity = func(path string) (pathIdentity, error) {
		if path == primary {
			<-releasePrimary
			close(primaryDone)
			return previousIdentity(path)
		}
		if path == alternate {
			alternateProbes++
		}
		return previousIdentity(path)
	}
	t.Cleanup(func() {
		close(releasePrimary)
		<-primaryDone
		relocationPathIdentity = previousIdentity
		relocationIdentityTimeout = previousTimeout
	})

	_, err = gw.ClaimRelocationSource()
	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"trying the alternate must not discard the primary probe failure when neither candidate is established")
	assert.Equal(t, 1, alternateProbes,
		"the alternate must be probed, but its pathname alone cannot establish identity")
	path, recovery, ok := gw.RelocationSnapshot()
	require.True(t, ok, "neither inconclusive candidate may clear the recovery fence")
	assert.Equal(t, primary, path)
	assert.Equal(t, alternate, recovery.AlternatePath)
	assert.True(t, expected.same(recovery.identity()))
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
