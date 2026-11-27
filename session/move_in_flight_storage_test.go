package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
)

// These tests exercise the durable move record through the REAL storage
// projection — Instance.ToInstanceData() → ForStorage() → JSON — and back
// through the real restore path — RestoreArchiveRollbackFence /
// RestoreRelocationRecoveryOriginals / FromInstanceData — the way a daemon
// restart actually reads it (R1, R2). A record that loses its residue names
// or either endpoint identity across that boundary is a record that cannot
// name residue at all, which is the entire property #7819 adds.

// persistRowForTest runs the exact write sequence daemon.persistInstanceData
// runs — ForStorage() then a title+stable-id upsert inside
// config.UpdateRepoInstances — minus the daemon package's private wrapper.
func persistRowForTest(t *testing.T, repoID string, data InstanceData) {
	t.Helper()
	data = data.ForStorage()
	err := config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []InstanceData
		if len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, &existing))
		}
		for i := range existing {
			if existing[i].Title == data.Title && existing[i].ID == data.ID {
				existing[i] = data
				out, err := json.MarshalIndent(existing, "", "  ")
				require.NoError(t, err)
				return out, nil
			}
		}
		existing = append(existing, data)
		out, err := json.MarshalIndent(existing, "", "  ")
		require.NoError(t, err)
		return out, nil
	})
	require.NoError(t, err)
}

// loadStoredRowForTest reads one session's row back from the repo store in
// exactly the raw projected form a daemon restart unmarshals.
func loadStoredRowForTest(t *testing.T, repoID, title string) InstanceData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var rows []InstanceData
	require.NoError(t, json.Unmarshal(raw, &rows))
	for i := range rows {
		if rows[i].Title == title {
			return rows[i]
		}
	}
	t.Fatalf("no stored row for %q in repo %s", title, repoID)
	return InstanceData{}
}

// statDirIdentity returns the device/inode/file-type triple the relocation
// layer's pathIdentity records for a real on-disk directory — the values a
// record must carry before claim resolution can match that path.
func statDirIdentity(t *testing.T, path string) (device, inode uint64, fileType uint32) {
	t.Helper()
	var stat syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &stat))
	return uint64(stat.Dev), uint64(stat.Ino), uint32(stat.Mode) & syscall.S_IFMT
}

// TestMoveInFlightRecord_RoundTripsThroughRealStorage is the positive half of
// R1/R2: a move_in_flight record written through the real
// ToInstanceData/ForStorage/marshal boundary and reloaded through the real
// FromInstanceData path must come back as the identical in-memory record —
// every residue name and both endpoint identities intact — and claim
// resolution must be reachable from it. On the unfixed head the ForStorage
// rebuild dropped QuarantinePath/StagingPath and all Dest* fields, so the
// stored row read back as a move_in_flight RestoreRelocationRecovery refuses
// (residue names incomplete) — or, past that, as a record that can never
// resolve the published destination.
func TestMoveInFlightRecord_RoundTripsThroughRealStorage(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-roundtrip")
	gitOut(t, repoRoot, "worktree", "add", "-b", "af/roundtrip", wtPath)
	dest := filepath.Join(t.TempDir(), "wt-roundtrip-dest")
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("a", 32))
	staging := filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("b", 32))
	dev, ino, ft := statDirIdentity(t, wtPath)

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "roundtrip", "af/roundtrip", "id-roundtrip")
	inst.liveness = LiveArchived
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:             git.RelocationRecoveryMoveInFlight,
		AlternatePath:     dest,
		IdentityKnown:     true,
		Device:            dev,
		Inode:             ino,
		FileType:          ft,
		QuarantinePath:    quarantine,
		StagingPath:       staging,
		DestIdentityKnown: true,
		DestDevice:        dev,
		DestInode:         777001,
		DestFileType:      ft,
	}))

	persistRowForTest(t, "repo-roundtrip", inst.ToInstanceData())
	stored := loadStoredRowForTest(t, "repo-roundtrip", "roundtrip")

	// The stored row IS projected: a previous release must see a claim_stale it
	// understands. The carrier fields tell current readers what it really is.
	require.NotNil(t, stored.Worktree.RelocationRecovery)
	assert.Equal(t, git.RelocationRecoveryClaimStale, stored.Worktree.RelocationRecovery.State,
		"the stored row projects move_in_flight as claim_stale for previous releases")
	assert.Equal(t, git.RelocationRecoveryMoveInFlight, stored.Worktree.RelocationRecovery.CleanupLifecycle,
		"the lifecycle carrier names the real state for current readers")
	assert.Equal(t, quarantine, stored.Worktree.RelocationRecovery.QuarantinePath,
		"the quarantine name must survive the projection verbatim — it IS the record's proof")
	assert.Equal(t, staging, stored.Worktree.RelocationRecovery.StagingPath)
	assert.True(t, stored.Worktree.RelocationRecovery.DestIdentityKnown)
	assert.Equal(t, uint64(777001), stored.Worktree.RelocationRecovery.DestInode)

	// The real restore path must give back the identical in-memory record.
	restored, err := stored.RestoreRelocationRecoveryOriginals()
	require.NoError(t, err, "a projected move_in_flight row must restore, not error out")
	rec := restored.Worktree.RelocationRecovery
	require.NotNil(t, rec)
	assert.Equal(t, git.RelocationRecoveryMoveInFlight, rec.State)
	assert.Equal(t, dest, rec.AlternatePath)
	assert.Equal(t, quarantine, rec.QuarantinePath)
	assert.Equal(t, staging, rec.StagingPath)
	assert.Equal(t, dev, rec.Device)
	assert.Equal(t, ino, rec.Inode)
	assert.Equal(t, ft, rec.FileType)
	assert.True(t, rec.DestIdentityKnown)
	assert.Equal(t, dev, rec.DestDevice)
	assert.Equal(t, uint64(777001), rec.DestInode)
	assert.Equal(t, ft, rec.DestFileType)

	// And the full FromInstanceData path rebuilds a worktree whose record is
	// the identical record claim resolution can run against.
	inst2, err := FromInstanceData(stored)
	require.NoError(t, err)
	live, ok := inst2.WorktreeRelocationRecovery()
	require.True(t, ok, "the reloaded instance must carry the record in memory")
	assert.Equal(t, git.RelocationRecoveryMoveInFlight, live.State)
	assert.Equal(t, quarantine, live.QuarantinePath)
	assert.Equal(t, staging, live.StagingPath)
	assert.Equal(t, uint64(777001), live.DestInode)
	assert.True(t, live.DestIdentityKnown)

	// Claim resolution is reachable from the reloaded record: the source is
	// still at its recorded path carrying the recorded identity, so the claim
	// binds there. The held claim keeps projecting an unresolved record until
	// the operation retires it — that projection IS the durable fence a
	// mid-operation checkpoint persists.
	claim, err := inst2.gitWorktree.ClaimRelocationSource()
	require.NoError(t, err, "claim resolution must be reachable from the reloaded record")
	assert.Equal(t, wtPath, claim.Path)
}

// TestMoveInFlightRecord_ArchiveReportLatchKeepsRecord covers the row shape
// that hides the record deepest: a session whose report is non-empty gets its
// worktree recovery field REPLACED by the synthetic kill fence, and the real
// record survives only inside RollbackFence. Both the raw row and the arbiter
// must still see the record's residue names (R1).
func TestMoveInFlightRecord_ArchiveReportLatchKeepsRecord(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-latch")
	gitOut(t, repoRoot, "worktree", "add", "-b", "af/latch", wtPath)
	dest := filepath.Join(t.TempDir(), "wt-latch-dest")
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("c", 32))
	staging := filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("d", 32))
	dev, ino, ft := statDirIdentity(t, wtPath)

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "latch", "af/latch", "id-latch")
	inst.liveness = LiveArchived
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:          git.RelocationRecoveryMoveInFlight,
		AlternatePath:  dest,
		IdentityKnown:  true,
		Device:         dev,
		Inode:          ino,
		FileType:       ft,
		QuarantinePath: quarantine,
		StagingPath:    staging,
	}))
	gw.RestoreArchiveReport(git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{
		{Path: wtPath + "-retained", Device: dev, Inode: 555, FileType: ft},
	}})

	persistRowForTest(t, "repo-latch", inst.ToInstanceData())
	stored := loadStoredRowForTest(t, "repo-latch", "latch")

	// The raw row's recovery field is the synthetic latch — not the record.
	latch := stored.Worktree.RelocationRecovery
	require.NotNil(t, latch)
	assert.Equal(t, git.RelocationRecoveryClaimStale, latch.State)
	assert.Empty(t, latch.QuarantinePath,
		"the latch never names residue; the real record lives in the fence")
	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence)
	fenceRec := stored.ArchiveReport.RollbackFence.OriginalRelocationRecovery
	require.NotNil(t, fenceRec, "the real record must ride the rollback fence")
	assert.Equal(t, quarantine, fenceRec.QuarantinePath,
		"the fence carrier must keep the residue names verbatim")
	assert.Equal(t, staging, fenceRec.StagingPath)

	// The real restore path reveals the record again — the same reversal the
	// arbiter's enumeration must perform before it can read residue names.
	revealed := stored.RestoreArchiveRollbackFence()
	restored, err := revealed.RestoreRelocationRecoveryOriginals()
	require.NoError(t, err)
	rec := restored.Worktree.RelocationRecovery
	require.NotNil(t, rec)
	assert.Equal(t, git.RelocationRecoveryMoveInFlight, rec.State)
	assert.Equal(t, quarantine, rec.QuarantinePath)
	assert.Equal(t, staging, rec.StagingPath)

	// And the arbiter — asked about residue only this session's fence-hidden
	// record names — must report it foreign to any other session rather than
	// falling through to unattributed, which would let leaf inference rule on
	// a path a durable record already owns.
	other, _ := arbitrationInstance(t, repoRoot, filepath.Join(wtParent, "wt-other"), "other", "af/latch", "id-other")
	arbiter := NewResidueArbiter(other)
	assert.Equal(t, ResidueForeignSession, arbiter.Classify(quarantine),
		"a record hidden behind the report latch must still attribute its residue")
	assert.Equal(t, ResidueForeignSession, arbiter.Classify(staging))
}

// TestMoveUnknownRecord_RoundTripsWithResidueNames covers the move_unknown
// half of R2: a post-publish record carries the residue names forward while
// cleanup is outstanding, and those names must survive the same projection
// boundary or the record's arbitration loses its proof.
func TestMoveUnknownRecord_RoundTripsWithResidueNames(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-unknown")
	gitOut(t, repoRoot, "worktree", "add", "-b", "af/unknown", wtPath)
	dest := filepath.Join(t.TempDir(), "wt-unknown-dest")
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("e", 32))
	staging := filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("f", 32))
	dev, ino, ft := statDirIdentity(t, wtPath)

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "unknown", "af/unknown", "id-unknown")
	inst.liveness = LiveArchived
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:          git.RelocationRecoveryMoveUnknown,
		AlternatePath:  dest,
		IdentityKnown:  true,
		Device:         dev,
		Inode:          ino,
		FileType:       ft,
		QuarantinePath: quarantine,
		StagingPath:    staging,
	}))

	persistRowForTest(t, "repo-unknown", inst.ToInstanceData())
	stored := loadStoredRowForTest(t, "repo-unknown", "unknown")
	restored, err := stored.RestoreRelocationRecoveryOriginals()
	require.NoError(t, err)
	rec := restored.Worktree.RelocationRecovery
	require.NotNil(t, rec)
	assert.Equal(t, git.RelocationRecoveryMoveUnknown, rec.State,
		"move_unknown keeps its own state — it is not lifecycle-projected")
	assert.Equal(t, quarantine, rec.QuarantinePath,
		"move_unknown's residue names must round-trip too")
	assert.Equal(t, staging, rec.StagingPath)
}

// TestRenameArchived_RecordCheckpointSnapshotsUnderHeldMu is the R4 property:
// RenameArchived is the one relocate route that runs the cross-device move
// while holding i.mu, so its durable checkpoint must be handed the row
// snapshot rather than take i.mu itself. On this head an ordinary checkpoint
// callback that called ToInstanceData would self-deadlock the daemon under
// reserveCreate's m.mu; the Locked variant proves the snapshot is produced
// under the already-held lock — a revert wedges this test outright (it hangs
// inside RenameArchived) rather than merely failing an assertion.
func TestRenameArchived_RecordCheckpointSnapshotsUnderHeldMu(t *testing.T) {
	repoRoot := residueTestRepo(t)
	wtParent := t.TempDir()
	src := filepath.Join(wtParent, "wt-rename")
	gitOut(t, repoRoot, "worktree", "add", "-b", "af/rename-mu", src)
	dest := filepath.Join(t.TempDir(), "renamed-archive")

	inst, _ := arbitrationInstance(t, repoRoot, src, "old-title", "af/rename-mu", "id-rename")
	inst.liveness = LiveArchived

	// Force the cross-device path inside the locked relocate, same as the
	// crash matrix — every write stays inside this test's temp dirs.
	restoreSeams := git.SetMoveDirCrashSeamsForTest(git.MoveDirCrashSeams{
		FastMove: func(*git.GitWorktree, string, string) error {
			return errors.New("invalid cross-device link")
		},
		FirstRename: func(string, string) error { return syscall.EXDEV },
	})
	defer restoreSeams()

	var snapshots []InstanceData
	restoreCheckpoint := inst.SetRelocationRecordCheckpointLocked(func(data InstanceData) error {
		snapshots = append(snapshots, data)
		return nil
	})
	defer restoreCheckpoint()

	require.NoError(t, inst.RenameArchived("new-title", dest, ""))
	require.NotEmpty(t, snapshots,
		"the locked checkpoint must have produced a row snapshot mid-move")
	first := snapshots[0].Worktree.RelocationRecovery
	require.NotNil(t, first, "the V1 snapshot carries the in-flight record")
	assert.Equal(t, git.RelocationRecoveryMoveInFlight, first.State)
	assert.NotEmpty(t, first.QuarantinePath)
	assert.NotEmpty(t, first.StagingPath)
	assert.Equal(t, "new-title", inst.Title)
	assert.DirExists(t, dest)
}
