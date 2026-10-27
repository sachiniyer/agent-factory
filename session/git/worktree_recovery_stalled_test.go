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

// stalledRecoveryWorktree builds a GitWorktree bound to primary whose recorded
// worktree path is primary (no real git repository is required: the recovery
// probe only stats the recorded path), mirroring the presence-test fixture the
// existing identity-stall tests use.
func stalledRecoveryWorktree(t *testing.T, primary string) *GitWorktree {
	t.Helper()
	root := testguard.CanonicalTempDir(t)
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(root, "repo"), primary, "stall", "af/stall", "", false, true,
	)
	require.NoError(t, err)
	return gw
}

// installClaimStaleForFailedSettle drives an identity-qualified stalled record
// through the real claim -> failed-settle seam so the in-memory record becomes
// the ClaimStale fence the failed settle installs — the exact shape
// RestoreStalledFenceAfterFailedSettle consumes. probeFailPath is the claim's
// selected path, whose settle revalidation probe will time out. The returned
// function restores the identity-probe seam; t.Cleanup also guards it.
func installClaimStaleForFailedSettle(
	t *testing.T,
	gw *GitWorktree,
	stalled RelocationRecovery,
	probeFailPath string,
) func() {
	t.Helper()
	require.NoError(t, gw.RestoreRelocationRecovery(stalled))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "the stalled record's candidate must resolve so settle can fail to revalidate it")
	require.Equal(t, probeFailPath, claim.Path, "premise: the claim selected the path the settle revalidation will fail on")
	restore := SetRelocationIdentityErrorForTest(probeFailPath, context.DeadlineExceeded)
	t.Cleanup(restore)
	settleErr := gw.SettleRelocationClaim(claim)
	require.ErrorIs(t, settleErr, ErrRelocateStateUnknown,
		"a probe timeout during settle must surface as an unknown, fail-closed answer")
	// The failed settle installs an in-memory ClaimStale fence carrying the
	// identity and alternate the reclaim just resolved — the exact fence the
	// restore function exists to consume.
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	require.Equal(t, RelocationRecoveryClaimStale, recovery.State,
		"the failed settle must leave a claim_stale fence for the restore to consume")
	require.True(t, recovery.IdentityKnown,
		"the failed-settle fence preserves the identity the reclaim resolved")
	return restore
}

// TestRestoreStalledFenceAfterFailedSettle_IdentityQualifiedPreservesEvidence is
// the core regression: an identity-qualified stalled record carrying an
// alternate candidate, when reclaimed and then failed at settle by a probe
// timeout, must restore to a stalled record that still carries the identity
// and the alternate — never to the bare identity-unknown shape the bug
// installed. discharging the fence over an absent primary must stay closed.
func TestRestoreStalledFenceAfterFailedSettle_IdentityQualifiedPreservesEvidence(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")
	alternate := filepath.Join(root, "secondary")
	require.NoError(t, os.Mkdir(primary, 0o755))
	identity, err := inspectRelocationPathIdentity(primary)
	require.NoError(t, err)

	gw := stalledRecoveryWorktree(t, primary)
	stalled := RelocationRecovery{
		State:         RelocationRecoveryStalled,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
		AlternatePath: alternate,
	}
	installClaimStaleForFailedSettle(t, gw, stalled, primary)

	require.NoError(t, gw.RestoreStalledFenceAfterFailedSettle(),
		"restoring the reclaimable stalled fence must succeed for a claim_stale fence")

	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok, "the restored fence must remain so the next kill re-claims")
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)
	assert.True(t, recovery.IdentityKnown, "the restored record must keep the identity the reclaim resolved")
	assert.True(t, recovery.identity().same(identity), "the restored identity must match the recorded directory")
	assert.Equal(t, alternate, recovery.AlternatePath, "the restored record must keep the alternate candidate")

	// The restored identity-qualified record is precisely the shape the
	// identity-unknown ENOENT discharges are documented to exclude, so the
	// fence must not be dischargeable over an absent primary.
	gw.relocationMu.Lock()
	assert.False(t, gw.identityUnknownStallLocked(),
		"an identity-qualified restored fence must not match the identity-unknown discharge shape")
	gw.relocationMu.Unlock()
	assert.False(t, gw.SettleAbsentIdentityUnknownStall(),
		"the absent-stall discharge must not clear an identity-qualified fence")
	assert.True(t, gw.HasUnresolvedRelocation(), "the fence must remain after the refused discharge")
}

// TestRestoreStalledFenceAfterFailedSettle_BareStalledPreservesDurableIdentityUnknown
// is the #5555 review regression: a durable identity-unknown stalled fence
// (the shape a timed-out relocation probe installs) is reclaimed by a claim
// whose retry probe resolves a fresh identity from the recorded path, then the
// settle revalidation times out and installs an in-memory claim_stale fence
// carrying that freshly observed identity. Restoring from the claim_stale
// fence would install an identity-known stalled record that diverges from the
// durable identity-unknown record on disk and closes the identity-unknown
// ENOENT discharge the next same-process retry needs when the archived path
// disappeared between claim and settle. The restore must instead retain the
// pre-claim fence shape — the bare identity-unknown stall — so a later same-
// process retry still discharges over a conclusively absent path instead of
// repeatedly failing in ClaimRelocationSource until a restart reloads the
// durable record.
func TestRestoreStalledFenceAfterFailedSettle_BareStalledPreservesDurableIdentityUnknown(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")
	require.NoError(t, os.Mkdir(primary, 0o755))
	identity, err := inspectRelocationPathIdentity(primary)
	require.NoError(t, err)

	gw := stalledRecoveryWorktree(t, primary)
	// The durable record is the bare identity-unknown stall a timed-out probe
	// installs: no identity, no alternate.
	require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
		State: RelocationRecoveryStalled,
	}))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "the recorded path exists, so the claim's retry probe resolves a fresh identity")
	require.Equal(t, primary, claim.Path, "the claim selects the recorded path")
	require.True(t, claim.identity.same(identity),
		"the claim acquires the identity the durable record never had")

	// Fail only the settle revalidation probe: the claim's resolution probe
	// already consumed the record, so the settle runs and fails to revalidate.
	restoreSeam := SetRelocationIdentityErrorForTest(primary, context.DeadlineExceeded)
	t.Cleanup(restoreSeam)
	require.ErrorIs(t, gw.SettleRelocationClaim(claim), ErrRelocateStateUnknown,
		"a probe timeout during settle must surface as an unknown, fail-closed answer")

	// The failed settle installs a claim_stale fence carrying the identity the
	// claim's retry probe observed — the durable record never had it.
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	require.Equal(t, RelocationRecoveryClaimStale, recovery.State,
		"the failed settle must leave a claim_stale fence for the restore to consume")
	require.True(t, recovery.IdentityKnown,
		"the failed-settle fence carries the identity the claim observed, not the durable record's")

	require.NoError(t, gw.RestoreStalledFenceAfterFailedSettle(),
		"restoring the reclaimable stalled fence must succeed for a claim_stale fence")

	// The restored record must be the bare identity-unknown stall the durable
	// record carried, not the identity the claim's retry probe observed.
	recovery, ok = gw.GetRelocationRecovery()
	require.True(t, ok, "the restored fence must remain so the next kill re-claims")
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)
	assert.False(t, recovery.IdentityKnown,
		"the restored record must keep the durable record's identity-unknown shape, not the claim's observed identity")
	assert.Empty(t, recovery.AlternatePath, "a bare stalled fence has no alternate candidate")

	// The restored identity-unknown record is exactly the shape the
	// identity-unknown ENOENT discharge is documented to clear, so removing
	// the recorded path must discharge the fence — the next same-process retry
	// must not be stranded the way restoring the claim's identity would.
	gw.relocationMu.Lock()
	assert.True(t, gw.identityUnknownStallLocked(),
		"the restored bare fence must match the identity-unknown discharge shape")
	gw.relocationMu.Unlock()
	require.NoError(t, os.RemoveAll(primary),
		"the recorded path disappears between the failed settle and the next retry")
	assert.True(t, gw.SettleAbsentIdentityUnknownStall(),
		"the next same-process retry must discharge the bare fence over the gone path, not strand on the claim's identity")
	assert.False(t, gw.HasUnresolvedRelocation(), "the discharged fence must clear")
}

// TestRestoreStalledFenceAfterFailedSettle_AlternateSelectedPreservesPrimaryAsAlternate
// covers the symmetric reclaim where the primary is gone and the claim selects
// the alternate, moving the recorded worktree path there. The failed settle
// then fails to revalidate the alternate, and the restore must keep the primary
// as the new alternate candidate so a later retry can still arbitrate it.
func TestRestoreStalledFenceAfterFailedSettle_AlternateSelectedPreservesPrimaryAsAlternate(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")
	alternate := filepath.Join(root, "secondary")
	require.NoError(t, os.Mkdir(alternate, 0o755))
	identity, err := inspectRelocationPathIdentity(alternate)
	require.NoError(t, err)

	gw := stalledRecoveryWorktree(t, primary)
	stalled := RelocationRecovery{
		State:         RelocationRecoveryStalled,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
		AlternatePath: alternate,
	}
	require.NoError(t, gw.RestoreRelocationRecovery(stalled))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "a vanished primary must let the identity-qualified alternate resolve")
	require.Equal(t, alternate, claim.Path)
	require.Equal(t, alternate, gw.GetWorktreePath(),
		"the reclaim moved the recorded worktree path to the selected alternate")

	restore := SetRelocationIdentityErrorForTest(alternate, context.DeadlineExceeded)
	t.Cleanup(restore)
	require.ErrorIs(t, gw.SettleRelocationClaim(claim), ErrRelocateStateUnknown)

	require.NoError(t, gw.RestoreStalledFenceAfterFailedSettle())
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	assert.Equal(t, RelocationRecoveryStalled, recovery.State)
	assert.True(t, recovery.IdentityKnown)
	assert.True(t, recovery.identity().same(identity))
	assert.Equal(t, primary, recovery.AlternatePath,
		"the primary must survive as the alternate so a later retry can arbitrate it")
}

// TestRestoreStalledFenceAfterFailedSettle_Guards pins the precondition gate:
// the restore refuses and changes nothing when there is no claim_stale fence
// from a failed settle — no recovery record, a different state, or an active
// relocation claim that still owns the directory.
func TestRestoreStalledFenceAfterFailedSettle_Guards(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")

	t.Run("no recovery record", func(t *testing.T) {
		gw := stalledRecoveryWorktree(t, primary)
		err := gw.RestoreStalledFenceAfterFailedSettle()
		require.Error(t, err)
		assert.False(t, gw.HasUnresolvedRelocation(), "a refusal must not fabricate a fence")
	})

	t.Run("wrong state is not a failed settle", func(t *testing.T) {
		gw := stalledRecoveryWorktree(t, primary)
		require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
			State: RelocationRecoveryStalled, IdentityKnown: true, Device: 1, Inode: 2, FileType: 3,
		}))
		err := gw.RestoreStalledFenceAfterFailedSettle()
		require.Error(t, err)
		recovery, ok := gw.GetRelocationRecovery()
		require.True(t, ok)
		assert.Equal(t, RelocationRecoveryStalled, recovery.State,
			"a refusal must leave the existing record untouched")
		assert.True(t, recovery.IdentityKnown)
	})

	t.Run("an active relocation claim blocks the restore", func(t *testing.T) {
		gw := stalledRecoveryWorktree(t, primary)
		// Install a claim_stale fence alongside an active claim, the shape a
		// same-process retry stranded before a tombstone would see.
		gw.relocationMu.Lock()
		gw.relocationRecovery = &RelocationRecovery{
			State: RelocationRecoveryClaimStale, IdentityKnown: true, Device: 1, Inode: 2, FileType: 3,
		}
		gw.activeRelocationClaim = &RelocationClaim{Path: primary, claimID: 1}
		gw.relocationMu.Unlock()

		err := gw.RestoreStalledFenceAfterFailedSettle()
		require.Error(t, err)
		gw.relocationMu.Lock()
		assert.NotNil(t, gw.relocationRecovery, "a refusal must not drop the fence while a claim is active")
		assert.Equal(t, RelocationRecoveryClaimStale, gw.relocationRecovery.State,
			"a refusal must not mutate the existing fence")
		assert.NotNil(t, gw.activeRelocationClaim)
		gw.relocationMu.Unlock()
	})
}

// TestRestoreStalledFenceAfterFailedSettle_PreservesArbitrationAfterInterruptedRename
// is the end-to-end regression from the bug report: an identity-qualified
// stalled record whose alternate later receives the archived bytes (the
// interrupted move completing) must still arbitrate the alternate after the
// failed settle + restore. With the bug, the restore wiped the identity and
// alternate, the next claim's identity-unknown discharge cleared the fence
// over the gone primary, and the claim returned a plain ENOENT — orphaning
// the directory now sitting at the alternate. With the fix, the fence
// retains, the next claim probes both candidates, and the alternate is
// selected.
func TestRestoreStalledFenceAfterFailedSettle_PreservesArbitrationAfterInterruptedRename(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")
	alternate := filepath.Join(root, "secondary")
	require.NoError(t, os.Mkdir(primary, 0o755))
	identity, err := inspectRelocationPathIdentity(primary)
	require.NoError(t, err)

	gw := stalledRecoveryWorktree(t, primary)
	stalled := RelocationRecovery{
		State:         RelocationRecoveryStalled,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
		AlternatePath: alternate,
	}
	// Reclaim selects the primary; the settle revalidation probe times out
	// and installs the identity-qualified ClaimStale fence. Restore the seam
	// before the interrupted rename so the next claim's probes observe the
	// real filesystem instead of the injected timeout.
	restoreSeam := installClaimStaleForFailedSettle(t, gw, stalled, primary)
	require.NoError(t, gw.RestoreStalledFenceAfterFailedSettle())

	restored, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	require.True(t, restored.IdentityKnown, "premise: the fix preserved the identity")
	require.Equal(t, alternate, restored.AlternatePath, "premise: the fix preserved the alternate")

	// The interrupted move completes hours or days later: the archived bytes
	// move from the primary to the recorded alternate.
	restoreSeam()
	require.NoError(t, os.Rename(primary, alternate))
	// The primary is now gone; the alternate holds the exact protected
	// identity the fence carries. Prove the discharge stays closed.
	assert.False(t, gw.SettleAbsentIdentityUnknownStall(),
		"the fence must not discharge over the gone primary while the alternate still holds the identity")
	assert.True(t, gw.HasUnresolvedRelocation(), "the fence must remain armed for the next claim to arbitrate")
	assert.False(t, gw.AbsentStallSettled(), "no discharge proof may be recorded")

	// The fresh claim must arbitrate: probe the gone primary, then the
	// identity-matching alternate, and select the alternate.
	secondClaim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "the restored fence must still arbitrate the alternate after the interrupted rename")
	assert.Equal(t, alternate, secondClaim.Path,
		"the claim must select the alternate that now holds the protected identity")
	assert.Equal(t, primary, secondClaim.AlternatePath,
		"the gone primary is recorded as the alternate candidate for the next retry")
	assert.Equal(t, alternate, gw.GetWorktreePath(),
		"the worktree location must follow the arbitrated candidate")

	require.NoError(t, gw.SettleRelocationClaim(secondClaim),
		"the arbitrated alternate must settle against the present directory")
	assert.False(t, gw.HasUnresolvedRelocation(), "the fence must clear once the alternate settles")
}

// TestRestoreStalledFenceAfterFailedSettle_DoesNotEraseEvidenceOnPersistShape
// guards the durable-erasure half of the bug: the restored record must carry
// the identity and alternate into the persistence snapshot (the shape the
// daemon writes on the retry path) so a later persist writes the evidence
// back intact instead of overwriting the durable identity-qualified record
// with a bare one.
func TestRestoreStalledFenceAfterFailedSettle_DoesNotEraseEvidenceOnPersistShape(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	primary := filepath.Join(root, "primary")
	alternate := filepath.Join(root, "secondary")
	require.NoError(t, os.Mkdir(primary, 0o755))
	identity, err := inspectRelocationPathIdentity(primary)
	require.NoError(t, err)

	gw := stalledRecoveryWorktree(t, primary)
	stalled := RelocationRecovery{
		State:         RelocationRecoveryStalled,
		IdentityKnown: true,
		Device:        identity.device,
		Inode:         identity.inode,
		FileType:      identity.fileType,
		AlternatePath: alternate,
	}
	installClaimStaleForFailedSettle(t, gw, stalled, primary)
	require.NoError(t, gw.RestoreStalledFenceAfterFailedSettle())

	_, snapshot, hasRecovery, _ := gw.PersistenceSnapshot()
	require.True(t, hasRecovery)
	assert.Equal(t, RelocationRecoveryStalled, snapshot.State)
	assert.True(t, snapshot.IdentityKnown, "the persistence snapshot must carry the identity so a retry writes it back")
	assert.True(t, snapshot.identity().same(identity))
	assert.Equal(t, alternate, snapshot.AlternatePath,
		"the persistence snapshot must carry the alternate so a restart can still arbitrate")
}
