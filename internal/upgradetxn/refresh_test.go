package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// refreshFixture prepares a transaction over one metadata file and returns the
// transaction, the home, and the metadata path.
func refreshFixture(t *testing.T) (*Transaction, string, string) {
	t.Helper()
	home := t.TempDir()
	metadata := filepath.Join(home, "instances.json")
	require.NoError(t, os.WriteFile(metadata, []byte(`{"serving":"before"}`), 0o644))

	plan := basicPreparePlan(t, home, "txn-refresh-0001")
	plan.MetadataPaths = []string{"instances.json"}
	txn, err := Prepare(plan)
	require.NoError(t, err)
	return txn, home, metadata
}

// TestRefreshMetadataSnapshot_CapturesWritesMadeAfterPrepare is #2212 gate 2.
//
// Prepare snapshots the metadata, and the daemon then stays fully live through
// InstallAndStart and AwaitSupervisorReady — a 60s grace — before it stops
// admitting mutations. A rollback restores the snapshot, so every write in that
// window is silently discarded.
//
// The fixture writes in exactly that window, and the first assertion proves the
// window is real before the refresh is asked to close it: without that, a passing
// test could mean the refresh worked OR that the fixture never diverged.
func TestRefreshMetadataSnapshot_CapturesWritesMadeAfterPrepare(t *testing.T) {
	txn, _, metadata := refreshFixture(t)

	// The daemon is still serving here — this is the write that used to be lost.
	require.NoError(t, os.WriteFile(metadata, []byte(`{"serving":"after"}`), 0o644))

	staged := txn.Journal().Metadata
	require.Len(t, staged, 1)
	before, err := os.ReadFile(staged[0].SnapshotPath)
	require.NoError(t, err)
	require.JSONEq(t, `{"serving":"before"}`, string(before),
		"premise: the staged snapshot is stale, so a rollback here would discard the later write")

	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer func() { _ = lease.Release() }()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))

	require.NoError(t, txn.RefreshMetadataSnapshot())

	refreshed := txn.Journal().Metadata
	require.Len(t, refreshed, 1)
	after, err := os.ReadFile(refreshed[0].SnapshotPath)
	require.NoError(t, err)
	require.JSONEq(t, `{"serving":"after"}`, string(after),
		"the bytes the actor would restore must be the bytes that were actually serving")
	require.Equal(t, digest([]byte(`{"serving":"after"}`)), refreshed[0].SHA256,
		"the recorded digest must describe the refreshed bytes, or the rollback integrity check refuses them")

	// The journal on disk must carry the refresh, not just the in-memory copy: the
	// actor reads the journal, never this process's struct.
	reloaded, err := readJournal(activeJournalPath(txn.Journal().HomeDir))
	require.NoError(t, err)
	require.Equal(t, refreshed[0].SHA256, reloaded.Metadata[0].SHA256)
}

// TestRefreshMetadataSnapshot_WritesANewGenerationAndKeepsTheOld pins the
// crash-safety property. Rewriting the snapshot in place would leave a window where
// the journal's recorded digests describe bytes that are no longer on disk, and the
// rollback path's integrity check would then refuse to restore — a crash mid-upgrade
// becoming an unrecoverable home, which is worse than the staleness being fixed.
func TestRefreshMetadataSnapshot_WritesANewGenerationAndKeepsTheOld(t *testing.T) {
	txn, _, metadata := refreshFixture(t)
	original := txn.Journal().Metadata[0].SnapshotPath
	require.NoError(t, os.WriteFile(metadata, []byte(`{"serving":"after"}`), 0o644))

	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer func() { _ = lease.Release() }()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, txn.RefreshMetadataSnapshot())

	refreshedPath := txn.Journal().Metadata[0].SnapshotPath
	require.NotEqual(t, original, refreshedPath,
		"the refresh must write a new generation rather than overwrite the set the journal names")

	superseded, err := os.ReadFile(original)
	require.NoError(t, err, "the superseded generation must survive, so a journal naming it stays honourable")
	require.JSONEq(t, `{"serving":"before"}`, string(superseded))
}

// TestRefreshMetadataSnapshot_RefusesOutsideTheReadyWindow is the safety argument,
// asserted rather than asserted-in-a-comment. Before supervisor_ready no actor is
// proven to exist; after authorization the actor may commit or roll back at any
// moment, and rewriting the snapshot directory underneath a running restore is the
// race this window exists to avoid.
func TestRefreshMetadataSnapshot_RefusesOutsideTheReadyWindow(t *testing.T) {
	txn, _, _ := refreshFixture(t)

	require.Error(t, txn.RefreshMetadataSnapshot(),
		"at PhasePrepared no actor has proven readiness; refreshing is not licensed yet")

	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer func() { _ = lease.Release() }()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, txn.RefreshMetadataSnapshot(), "the ready window is the one place it is allowed")

	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.Error(t, txn.RefreshMetadataSnapshot(),
		"past the ready window the actor owns the transaction and may be restoring; refuse")
}

// TestNextMetadataGeneration_ReadsTheGenerationFromDisk pins that the counter comes
// from the journal's own paths. A Transaction can be reconstructed from disk by a
// later process, so an in-memory counter would restart at zero and hand back a
// directory that is already populated.
func TestNextMetadataGeneration_ReadsTheGenerationFromDisk(t *testing.T) {
	require.Equal(t, 1, nextMetadataGeneration(nil), "no snapshots yet means the first refresh is generation 1")
	require.Equal(t, 1, nextMetadataGeneration([]MetadataSnapshot{
		{SnapshotPath: filepath.Join("/txn", metadataDirName, "0000.snapshot")},
	}), "Prepare's directory is generation 0")
	require.Equal(t, 3, nextMetadataGeneration([]MetadataSnapshot{
		{SnapshotPath: filepath.Join("/txn", metadataDirName+"-2", "0000.snapshot")},
	}))
	require.Equal(t, 1, nextMetadataGeneration([]MetadataSnapshot{
		{Path: "gone.json"}, // never existed, so it carries no snapshot path to read
	}))
}
