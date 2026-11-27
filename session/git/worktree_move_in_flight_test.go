package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the durable move_in_flight record of #7819: a cross-device
// move names its `.af-source-*` quarantine and `.af-copy-*` staging paths in a
// record that is persisted BEFORE any of those paths exist, carries the staged
// destination identity ahead of the quarantine rename, and retires only when
// the move finishes or a retry's claim resolution consumes it. None of this
// exists on master, where the same crash points leave residue no record names.
// The session-level crash matrix lives in session/move_in_flight_crash_test.go:
// only the session package can persist the record through the real
// ToInstanceData/ForStorage/FromInstanceData path it must survive.

// TestMoveInFlightRecord_NamesResidueBeforeItExists drives a real
// cross-device ArchiveWorktree and asserts, inside the installed durable
// checkpoint, that the record names BOTH residue paths while neither exists
// yet (the V1 write) and that the staged destination identity joins the
// record before the quarantine rename (the V2 write). The first-rename seam
// returns EXDEV so the whole move runs inside the test's temp dirs on one
// filesystem — the cross-device path is identical either way, EXDEV is just
// the trigger that selects it.
func TestMoveInFlightRecord_NamesResidueBeforeItExists(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "archived-mif")
	forceCrossDeviceForTest(t)

	type write struct {
		recovery           RelocationRecovery
		residueAtWriteTime []string
	}
	var writes []write
	restore := gw.SetRelocationRecordCheckpoint(func() error {
		_, recovery, ok := gw.RelocationSnapshot()
		require.True(t, ok, "the durable checkpoint must see the in-flight record")
		require.Equal(t, RelocationRecoveryMoveInFlight, recovery.State)
		residue, err := ListAFRelocationResidue(filepath.Dir(wtPath), destParent)
		require.NoError(t, err)
		writes = append(writes, write{recovery: recovery, residueAtWriteTime: residue})
		// At the V2 write the staged copy exists and the quarantine does not
		// yet: the record is durable ahead of the commit boundary it precedes.
		if recovery.DestIdentityKnown {
			staged, serr := os.Lstat(recovery.StagingPath)
			require.NoError(t, serr, "at the V2 write the staged copy must exist")
			require.True(t, staged.IsDir())
			assert.NoDirExists(t, recovery.QuarantinePath)
		}
		return nil
	})
	defer restore()

	require.NoError(t, gw.ArchiveWorktree(dest))
	require.Len(t, writes, 2,
		"a cross-device move must publish the in-flight record before the copy and again before the quarantine rename")

	// V1: both residue paths are named BEFORE either exists. This is the whole
	// durability property — a crash anywhere later leaves named residue.
	assert.Equal(t, RelocationRecoveryMoveInFlight, writes[0].recovery.State)
	assert.True(t, privateMoveResidueName(filepath.Base(writes[0].recovery.QuarantinePath)),
		"the V1 record must name the .af-source-* quarantine before it exists")
	assert.True(t, privateMoveResidueName(filepath.Base(writes[0].recovery.StagingPath)),
		"the V1 record must name the .af-copy-* staging before it exists")
	assert.Equal(t, filepath.Dir(wtPath), filepath.Dir(writes[0].recovery.QuarantinePath))
	assert.Equal(t, filepath.Dir(dest), filepath.Dir(writes[0].recovery.StagingPath))
	assert.Empty(t, writes[0].residueAtWriteTime,
		"the V1 write must land before ANY residue exists; nothing may be named retroactively")
	assert.Equal(t, dest, writes[0].recovery.AlternatePath)
	assert.False(t, writes[0].recovery.DestIdentityKnown)

	// V2: the staged destination's identity is durable before the quarantine
	// rename — the only proof a post-publish crash can resolve to dest, since
	// a copy's inode differs from the source's.
	assert.True(t, writes[1].recovery.DestIdentityKnown)
	assert.Equal(t, writes[0].recovery.QuarantinePath, writes[1].recovery.QuarantinePath)
	assert.Equal(t, writes[0].recovery.StagingPath, writes[1].recovery.StagingPath)
	// The V2 write still lands before the quarantine rename: the staged copy
	// is the only residue present at that point.
	assert.ElementsMatch(t, []string{writes[1].recovery.StagingPath}, writes[1].residueAtWriteTime)

	// Success retires the record entirely.
	_, _, unresolved := gw.RelocationSnapshot()
	assert.False(t, unresolved, "a completed move retires its in-flight record")
	assert.NoDirExists(t, writes[1].recovery.QuarantinePath)
	assert.NoDirExists(t, writes[1].recovery.StagingPath)
	assert.DirExists(t, dest)
}

// TestMoveInFlightRecord_PersistFailureAbortsBeforeResidue pins the abort: a
// failed durable write at V1 fails the move before a single byte is staged,
// because residue without its record is exactly the gap this state closes —
// and the record that never armed is dropped (R3), so the failure keeps
// master's plain error class instead of latching Unknown.
func TestMoveInFlightRecord_PersistFailureAbortsBeforeResidue(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "archived-mif")
	forceCrossDeviceForTest(t)

	restore := gw.SetRelocationRecordCheckpoint(func() error {
		return os.ErrPermission
	})
	defer restore()

	err := gw.ArchiveWorktree(dest)
	require.Error(t, err, "the move must fail when its record cannot be persisted")
	assert.Contains(t, err.Error(), "could not durably record the in-flight move")
	assert.False(t, errors.Is(err, ErrRelocateStateUnknown),
		"a record that never armed is discarded — the failure keeps master's plain error class (R3)")

	// Nothing moved and nothing was stranded: src is intact, dest never
	// existed, and no residue name appears beside either endpoint.
	assert.DirExists(t, wtPath)
	assert.NoDirExists(t, dest)
	residue, rerr := ListAFRelocationResidue(filepath.Dir(wtPath), destParent)
	require.NoError(t, rerr)
	assert.Empty(t, residue)

	// The unarmed in-flight record is gone: whatever the ordinary claim
	// preserve leaves (master's parity for a mid-move failure), it must not
	// be a move_in_flight record naming residue that never existed — and the
	// next claim resolves the live source normally.
	_, rec, _ := gw.RelocationSnapshot()
	assert.NotEqual(t, RelocationRecoveryMoveInFlight, rec.State,
		"an unarmed move_in_flight record is discarded, not kept")
	claim, claimErr := gw.ClaimRelocationSource()
	require.NoError(t, claimErr)
	assert.Equal(t, wtPath, claim.Path)
}

// TestMoveInFlightRecord_CopyFailureClearsUnarmedRecord covers the error —
// not crash — mid-copy case: the copier removes its own partial staging on
// failure, so the record's named residue provably does not exist and the
// record is dropped (R3), keeping the plain error class.
func TestMoveInFlightRecord_CopyFailureClearsUnarmedRecord(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "archived-mif")
	forceCrossDeviceForTest(t)

	wrote := false
	restore := gw.SetRelocationRecordCheckpoint(func() error {
		wrote = true
		return nil
	})
	defer restore()
	prevCreate := copyTreeAfterDestCreate
	copyTreeAfterDestCreate = func(string) error { return os.ErrPermission }
	defer func() { copyTreeAfterDestCreate = prevCreate }()

	err := gw.ArchiveWorktree(dest)
	require.Error(t, err)
	assert.True(t, wrote, "the V1 record write must have been durable before the copy began")
	assert.False(t, errors.Is(err, ErrRelocateStateUnknown),
		"the failed copy left no residue, so its record is unarmed and the error stays plain")
	_, rec, _ := gw.RelocationSnapshot()
	assert.NotEqual(t, RelocationRecoveryMoveInFlight, rec.State)
	assert.DirExists(t, wtPath)
	residue, rerr := ListAFRelocationResidue(filepath.Dir(wtPath), destParent)
	require.NoError(t, rerr)
	assert.Empty(t, residue, "the copier removed its partial staging; nothing is stranded")
}

// forceCrossDeviceForTest drives the engine down its real cross-device path
// without a second filesystem: worktreeMoveFast answers with the same error
// a cross-device `git worktree move` returns, which selects the copy
// fallback, and renamePath answers EXDEV at the fallback's first
// same-filesystem rename attempt. Every write stays inside the test's own
// temp dirs (host safety: no /dev/shm, /run, /var/tmp or loopback mounts).
func forceCrossDeviceForTest(t *testing.T) {
	t.Helper()
	prevFast := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("invalid cross-device link")
	}
	prevRename := renamePath
	renamePath = func(string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { worktreeMoveFast = prevFast; renamePath = prevRename })
}

// moveInFlightRecordFor builds the durable record an interrupted cross-device
// move would have persisted: src's identity plus both generated residue names.
func moveInFlightRecordFor(t *testing.T, src, dest string) RelocationRecovery {
	t.Helper()
	identity, err := boundedRelocationPathIdentity(src)
	require.NoError(t, err)
	return RelocationRecovery{
		State:             RelocationRecoveryMoveInFlight,
		AlternatePath:     dest,
		IdentityKnown:     true,
		Device:            identity.device,
		Inode:             identity.inode,
		FileType:          identity.fileType,
		QuarantinePath:    filepath.Join(filepath.Dir(src), ".af-source-"+strings.Repeat("a", 32)),
		StagingPath:       filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("b", 32)),
		DestIdentityKnown: false,
	}
}

// TestMoveInFlightRecord_ResolutionQuarantineRefuses pins the refuse-with-exit
// rule: a surviving `.af-source-*` named by this session's own record is the
// only complete copy of the session's work, so resolution keeps the record
// and refuses, naming the quarantine and the operator exit.
func TestMoveInFlightRecord_ResolutionQuarantineRefuses(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	recovery := moveInFlightRecordFor(t, wtPath, dest)
	require.NoError(t, gw.RestoreRelocationRecovery(recovery))
	require.NoError(t, os.Rename(wtPath, recovery.QuarantinePath))

	_, claimErr := gw.ClaimRelocationSource()
	require.Error(t, claimErr)
	assert.True(t, errors.Is(claimErr, ErrRelocateStateUnknown),
		"the refusal keeps the record so the operator can fix and retry: %v", claimErr)
	assert.Contains(t, claimErr.Error(), recovery.QuarantinePath)
	assert.Contains(t, claimErr.Error(), "move it back to "+wtPath,
		"the refusal must carry the actionable exit, not just a state name")

	// The operator exit: move the quarantine back to the recorded path and
	// retry — resolution then claims the live source.
	require.NoError(t, os.Rename(recovery.QuarantinePath, wtPath))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	assert.Equal(t, wtPath, claim.Path)
	_, snap, unresolved := gw.RelocationSnapshot()
	assert.True(t, unresolved,
		"an activated claim projects as unresolved until it is settled or preserved")
	assert.Equal(t, RelocationRecoveryClaimStale, snap.State)
}

// TestMoveInFlightRecord_ResolutionSourceMatch covers the crash-before-rename
// window: the quarantine rename never ran, src still carries the recorded
// identity, and resolution simply claims it again.
func TestMoveInFlightRecord_ResolutionSourceMatch(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	assert.Equal(t, wtPath, claim.Path)
}

// TestMoveInFlightRecord_ResolutionDestMatch covers the post-publish crash:
// src and quarantine are gone (the copy completed and the staging published
// at dest), and the record's dest identity — captured before the quarantine
// rename — resolves the move forward to the destination.
func TestMoveInFlightRecord_ResolutionDestMatch(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	recovery := moveInFlightRecordFor(t, wtPath, dest)

	// Model the post-publish footprint: the staged copy published at dest and
	// the source is gone. The dest identity in the record is what resolves it.
	require.NoError(t, os.Rename(wtPath, dest))
	destIdentity, err := boundedRelocationPathIdentity(dest)
	require.NoError(t, err)
	recovery.DestIdentityKnown = true
	recovery.DestDevice = destIdentity.device
	recovery.DestInode = destIdentity.inode
	recovery.DestFileType = destIdentity.fileType
	require.NoError(t, gw.RestoreRelocationRecovery(recovery))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "the dest identity must resolve the post-publish crash forward")
	assert.Equal(t, dest, claim.Path)
	path, _, unresolved := gw.RelocationSnapshot()
	assert.Equal(t, dest, path, "the recorded worktree path relocates with the resolution")
	assert.True(t, unresolved,
		"the activated claim projects as unresolved until the move settles it")
}

// TestMoveInFlightRecord_ResolutionUnresolvableRefuses covers the case where
// neither endpoint answers the record: src gone, dest absent or mismatched,
// quarantine gone. The record stays and the claim refuses rather than
// guessing.
func TestMoveInFlightRecord_ResolutionUnresolvableRefuses(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	_, claimErr := gw.ClaimRelocationSource()
	require.Error(t, claimErr)
	assert.True(t, errors.Is(claimErr, ErrRelocateStateUnknown))
	_, _, unresolved := gw.RelocationSnapshot()
	assert.True(t, unresolved, "an unresolvable in-flight record is kept, never cleared")
}

// TestMoveInFlightRecord_DoesNotFenceCleanup pins the maintainer's kill
// carve-out: kill deletes the worktree path and the owned branch, never the
// `.af-source-*`/`.af-copy-*` siblings the record names, so an in-flight
// record must not add a cleanup refusal master does not have.
func TestMoveInFlightRecord_DoesNotFenceCleanup(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	state, err := gw.cleanup(false)
	assert.Equal(t, CleanupSettled, state,
		"cleanup must proceed on a move_in_flight record exactly as on master's record-free row (err=%v)", err)
	if err != nil {
		assert.NotContains(t, err.Error(), "refusing to inspect or clean up worktree recovery state")
	}
}

// TestMoveInFlightRecord_PresenceStaysUnknown keeps the conservative fence for
// routes that DO arbitrate: while the record is live the recorded path's
// presence is Unknown, so rebuild/adopt consumers cannot treat the missing
// path as deleted-outside-af.
func TestMoveInFlightRecord_PresenceStaysUnknown(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	assert.Equal(t, WorktreePresenceUnknown, gw.ProbeWorktreePresence(),
		"the in-flight record owns the path question until resolution retires it")
}

// TestCandidateRecordClaimPreservesResidueNames is the #7819 round-two carry
// a post-publication record needs: a move_unknown or preserved claim_stale
// still names the residue it owns, and the claim resolving it must carry
// those names — a preserve that re-materializes the record without them
// leaves the surviving quarantine attributed to nothing durable, exactly
// the unnamed-residue degradation the record exists to prevent.
func TestCandidateRecordClaimPreservesResidueNames(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-0123456789abcdef0123456789abcdef")
	staging := filepath.Join(t.TempDir(), ".af-copy-0123456789abcdef0123456789abcdef")
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(wtPath, &st))
	require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{
		State:          RelocationRecoveryClaimStale,
		IdentityKnown:  true,
		Device:         uint64(st.Dev),
		Inode:          uint64(st.Ino),
		FileType:       uint32(st.Mode & syscall.S_IFMT),
		QuarantinePath: quarantine,
		StagingPath:    staging,
	}))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	gw.PreserveRelocationClaim(claim)

	recovery, unresolved := gw.GetRelocationRecovery()
	require.True(t, unresolved, "the preserved claim re-materializes the record")
	assert.Equal(t, quarantine, recovery.QuarantinePath,
		"the claim must carry the quarantine name the record held")
	assert.Equal(t, staging, recovery.StagingPath,
		"the claim must carry the staging name the record held")
}
