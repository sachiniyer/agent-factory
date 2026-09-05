package upgradetxn

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stagedArtifactPlan(t *testing.T, executable string) Plan {
	t.Helper()
	return Plan{
		ID:             "upgrade-" + strings.Repeat("e", 32),
		HomeDir:        t.TempDir(),
		ExecutablePath: executable,
		FromVersion:    "1.0.100",
		ToVersion:      "1.0.300",
		Candidate:      []byte("candidate-af-binary"),
		Daemon: DaemonSnapshot{
			WasRunning: true,
			BootID:     "boot",
			Owner:      DaemonOwner{Kind: SupervisionAdHoc},
		},
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	}
}

// The defect this change exists to remove, end to end: a leftover preserved
// binary from a cleanup that died refused every future daemon-owned upgrade of
// this executable, and the unattended path had no override at all.
func TestPrepare_ClearsAnInertLeftoverInsteadOfRefusingForever(t *testing.T) {
	executable := lockTestExecutable(t)
	previous, _ := stageArtifactFor(t, executable, "upgrade-leftover", 48*time.Hour)

	txn, err := Prepare(stagedArtifactPlan(t, executable))
	require.NoError(t, err, "debris beside the executable must not refuse a new transaction")
	require.NotNil(t, txn)

	require.NoFileExists(t, previous)
	aside, err := filepath.Glob(previous + ".debris-*")
	require.NoError(t, err)
	require.Len(t, aside, 1, "the leftover is set aside, not destroyed")
}

// The interlock still does its job: a transaction another home still names in
// its active journal blocks, and says enough for a human to act on.
func TestPrepare_StillRefusesALiveForeignTransaction(t *testing.T) {
	executable := lockTestExecutable(t)
	previous, _ := stageArtifactFor(t, executable, "upgrade-live", 48*time.Hour)
	home := homeWithActiveTransaction(t, "upgrade-live")
	writeOwnerRecordFor(t, executable, "upgrade-live", home, 48*time.Hour)

	_, err := Prepare(stagedArtifactPlan(t, executable))
	require.Error(t, err, "a live transaction over this executable must still refuse a second one")
	require.ErrorContains(t, err, "upgrade-live", "the refusal must name the transaction")
	require.ErrorContains(t, err, previous, "the refusal must name the artifact, so a human can act on it")
	require.ErrorContains(t, err, home, "the refusal must say what evidence made it live")
	require.FileExists(t, previous, "a live transaction's rollback source must not be touched")
}

// The record is what makes a later scan able to ask a question instead of
// guessing from a filename.
func TestPrepare_WritesAnOwnerRecordForItsStagedBinaries(t *testing.T) {
	executable := lockTestExecutable(t)
	plan := stagedArtifactPlan(t, executable)

	txn, err := Prepare(plan)
	require.NoError(t, err)

	owner, err := readArtifactOwner(artifactOwnerPath(executable, plan.ID), plan.ID)
	require.NoError(t, err, "Prepare must leave a readable owner record beside the binaries")
	require.Equal(t, plan.ID, owner.TransactionID)
	require.Equal(t, txn.Journal().HomeDir, owner.HomeDir, "the record must name the home holding the journal")
	require.False(t, owner.StagedAt.IsZero())
}

// Removed with the artifacts it describes, and AFTER them: a crash between the
// two must leave a record with no binaries (which blocks nothing) rather than
// binaries with no record.
func TestAbortPrepared_RemovesTheOwnerRecordWithTheArtifacts(t *testing.T) {
	executable := lockTestExecutable(t)
	plan := stagedArtifactPlan(t, executable)
	txn, err := Prepare(plan)
	require.NoError(t, err)
	ownerPath := artifactOwnerPath(executable, plan.ID)
	require.FileExists(t, ownerPath)

	require.NoError(t, AbortPreparedTransaction(txn.Journal().HomeDir, plan.ID))

	require.NoFileExists(t, txn.Journal().PreviousBinaryPath)
	require.NoFileExists(t, ownerPath, "an owner record must not outlive the artifacts it describes")
}

// A transaction must never modify an artifact it did not create. Reusing an id
// whose record already exists overwrote another owner's record and then deleted
// it on the way out, leaving their binaries unattributable (#2984).
func TestPrepare_RefusesAnIDWhoseOwnerRecordAlreadyExists(t *testing.T) {
	executable := lockTestExecutable(t)
	plan := stagedArtifactPlan(t, executable)
	ownerPath := artifactOwnerPath(executable, plan.ID)
	somebodyElse := ArtifactOwner{
		SchemaVersion: artifactOwnerSchemaVersion,
		TransactionID: plan.ID,
		HomeDir:       t.TempDir(),
		StagedAt:      time.Now().UTC(),
	}
	require.NoError(t, writeArtifactOwner(ownerPath, somebodyElse))

	_, err := Prepare(plan)
	require.Error(t, err, "an id whose owner record is already on disk must be refused, not written over")
	require.ErrorContains(t, err, ownerPath)

	kept, err := readArtifactOwner(ownerPath, plan.ID)
	require.NoError(t, err, "the existing record must be left exactly as it was")
	require.Equal(t, somebodyElse.HomeDir, kept.HomeDir)
}

// Prepare holds both locks, so it may clear; the artifacts it clears are gone
// from the scan's view before it stages its own.
func TestPrepare_LeavesAYoungForeignArtifactAlone(t *testing.T) {
	executable := lockTestExecutable(t)
	previous, _ := stageArtifactFor(t, executable, "upgrade-inflight", 0)

	_, err := Prepare(stagedArtifactPlan(t, executable))
	require.Error(t, err, "an artifact staged moments ago may be a Prepare still running")
	require.FileExists(t, previous)
}
