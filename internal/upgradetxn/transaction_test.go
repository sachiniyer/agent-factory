package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func prepareFixture(t *testing.T) (*Transaction, string, string) {
	t.Helper()
	home := t.TempDir()
	binDir := t.TempDir()
	executable := filepath.Join(binDir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("known-running-binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte("old-state"), 0o640))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "instances", "repo-a"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "instances", "repo-a", "instances.json"),
		[]byte("old-instances"), 0o600))

	txn, err := Prepare(Plan{
		ID:             "txn-2212",
		HomeDir:        home,
		ExecutablePath: executable,
		FromVersion:    "1.0.206",
		ToVersion:      "1.0.207",
		Candidate:      []byte("candidate-binary"),
		RecoveryJob:    RecoveryJob{Kind: RecoveryJobDetached},
		MetadataPaths: []string{
			"state.json",
			"tasks.json", // absent is a fact rollback must restore
			filepath.Join("instances", "repo-a", "instances.json"),
		},
	})
	require.NoError(t, err)
	return txn, home, executable
}

func TestRecoveryJobIdentityIsTransactionScopedAndJournaled(t *testing.T) {
	unitDir := t.TempDir()
	systemd, err := NewRecoveryJob(RecoveryJobSystemd, "txn-2212", unitDir)
	require.NoError(t, err)
	require.Equal(t, "agent-factory-upgrade-recovery-txn-2212.service", systemd.Name)
	require.Equal(t, filepath.Join(unitDir, systemd.Name), systemd.UnitPath)
	require.NoError(t, validateRecoveryJob("txn-2212", systemd))

	launchd, err := NewRecoveryJob(RecoveryJobLaunchd, "txn-2212", unitDir)
	require.NoError(t, err)
	require.Equal(t, "com.agent-factory.upgrade-recovery.txn-2212", launchd.Name)
	require.Equal(t, filepath.Join(unitDir, launchd.Name+".plist"), launchd.UnitPath)
	require.NoError(t, validateRecoveryJob("txn-2212", launchd))

	tampered := systemd
	tampered.Name = "agent-factory-upgrade-recovery-another.service"
	require.Error(t, validateRecoveryJob("txn-2212", tampered))
}

func TestRecoveryMutationAuthorityRequiresPreservedBinary(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.TryAcquireRecovery()
	if lease != nil {
		t.Cleanup(func() { require.NoError(t, lease.Release()) })
	}
	require.Error(t, err,
		"code running from any executable other than the preserved previous binary must not receive mutation authority")
	require.ErrorIs(t, err, ErrRecoveryActorMismatch)
}

func TestRollbackRestoresBinaryMetadataAndAbsence(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	journal := txn.Journal()
	require.Equal(t, PhasePrepared, journal.Phase)
	require.FileExists(t, journal.PreviousBinaryPath)
	require.FileExists(t, journal.CandidatePath)
	require.Equal(t, "txn-2212", journal.ID)

	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte("new-state"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "tasks.json"), []byte("new-tasks"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "instances", "repo-a", "instances.json"),
		[]byte("new-instances"), 0o600))

	require.NoError(t, lease.Rollback())
	require.Equal(t, PhaseRollbackRestored, txn.Journal().Phase)
	require.FileExists(t, journal.PreviousBinaryPath,
		"recovery artifacts remain until the previous daemon is healthy")

	got, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "known-running-binary", string(got))
	got, err = os.ReadFile(filepath.Join(home, "state.json"))
	require.NoError(t, err)
	require.Equal(t, "old-state", string(got))
	info, err := os.Stat(filepath.Join(home, "state.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	got, err = os.ReadFile(filepath.Join(home, "instances", "repo-a", "instances.json"))
	require.NoError(t, err)
	require.Equal(t, "old-instances", string(got))
	_, err = os.Stat(filepath.Join(home, "tasks.json"))
	require.ErrorIs(t, err, os.ErrNotExist,
		"a file created during probation must be removed when it was absent at prepare time")

	// Every recovery operation is resumable after actor loss.
	require.NoError(t, lease.Rollback())
}

func TestRollbackResumesFromDurablePerFileCheckpoints(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte("new-state"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "tasks.json"), []byte("new-tasks"), 0o600))

	checkpointErr := fmt.Errorf("simulated supervisor death: %w", errRecoveryCheckpointInterrupted)
	txn.afterRollbackCheckpoint = func(progress RollbackProgress) error {
		if progress.BinaryRestored && progress.MetadataRestored == 1 {
			return checkpointErr
		}
		return nil
	}
	err = lease.Rollback()
	require.ErrorIs(t, err, checkpointErr)
	require.Equal(t, PhaseRollingBack, txn.Journal().Phase)
	require.Equal(t, RollbackProgress{BinaryRestored: true, MetadataRestored: 1},
		txn.Journal().RollbackProgress)
	require.NoError(t, lease.Release())

	resumed, err := Load(home)
	require.NoError(t, err)
	takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, takeover.Rollback())
	require.Equal(t, PhaseRollbackRestored, resumed.Journal().Phase)
	require.NoError(t, takeover.Release())

	got, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "known-running-binary", string(got))
	got, err = os.ReadFile(filepath.Join(home, "state.json"))
	require.NoError(t, err)
	require.Equal(t, "old-state", string(got))
	_, err = os.Stat(filepath.Join(home, "tasks.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryLockIsTheDeathTest(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	live, err := txn.RecoveryActorLive()
	require.NoError(t, err)
	require.False(t, live)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))
	live, err = txn.RecoveryActorLive()
	require.NoError(t, err)
	require.True(t, live)
	status, err := ReadRecoveryStatus(home)
	require.NoError(t, err)
	require.Equal(t, txn.Journal().RecoveryNonce, status.Nonce)
	require.Equal(t, txn.Journal().PreviousBinaryPath, status.Executable)

	loaded, err := Load(home)
	require.NoError(t, err)
	second, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrRecoveryActive,
		"a fresh or stale timestamp cannot override a kernel-held recovery lock")

	require.NoError(t, lease.Release())
	live, err = txn.RecoveryActorLive()
	require.NoError(t, err)
	require.False(t, live)
	second, err = loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NotNil(t, second, "actor death/release makes the lock acquirable")
	require.NoError(t, second.Release())
}

func TestActivationRequiresLivePreviousBinaryNonceHandshake(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	require.Error(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce),
		"a journal without a live recovery actor is not a handshake")

	lease, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.Error(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce),
		"the actor must publish its exact supervisor_ready status before authorization")
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))
	require.Error(t, txn.AuthorizeActivation(journal.ID, "wrong-nonce"))
	require.NoError(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce))
	authorized, err := txn.ActivationAuthorized()
	require.NoError(t, err)
	require.True(t, authorized)
	require.NoError(t, lease.Release())
}

func TestRecoveryTakeoverReloadsJournalAfterActorDeath(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	stale, err := Load(home)
	require.NoError(t, err)

	first, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, first.Advance(PhaseSupervisorReady))
	require.NoError(t, first.Release())

	second, err := stale.tryAcquireRecoveryAs(stale.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.Equal(t, PhaseSupervisorReady, stale.Journal().Phase,
		"a takeover must resume the durable phase, not overwrite it from a stale pre-lock read")
	require.NoError(t, second.Release())
}

func TestReleasedRecoveryLeaseCannotMutateTransaction(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Release())

	err = lease.Advance(PhaseSupervisorReady)
	require.Error(t, err)
	loaded, loadErr := Load(home)
	require.NoError(t, loadErr)
	require.Equal(t, PhasePrepared, loaded.Journal().Phase,
		"releasing the kernel death-test lock must revoke phase mutation authority")
}

func TestCommitIsDurableBeforeCleanup(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, lease.Commit())

	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseCommitted, loaded.Journal().Phase,
		"a crash before cleanup must resume cleanup, never rollback")
	require.NoError(t, lease.Cleanup())

	_, err = Load(home)
	require.True(t, errors.Is(err, ErrNoActiveTransaction))
	got, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "candidate-binary", string(got),
		"cleanup after commit must never restore the previous binary")
}

func TestTerminalCleanupRecoversAfterTransactionDirectoryDisappears(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, lease.Commit())
	require.NoError(t, lease.Release())

	journal := txn.Journal()
	require.NoError(t, os.RemoveAll(transactionDir(home, journal.ID)),
		"simulate actor death after terminal transaction-directory cleanup but before active.json removal")
	loaded, err := Load(home)
	require.NoError(t, err)
	recovery, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, recovery.Cleanup())
	require.NoError(t, recovery.Release())

	_, err = Load(home)
	require.ErrorIs(t, err, ErrNoActiveTransaction)
	got, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "candidate-binary", string(got))
	for _, artifact := range []string{journal.PreviousBinaryPath, journal.CandidatePath} {
		_, err = os.Stat(artifact)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestPrepareRefusesEscapingMetadataPath(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("old"), 0o755))
	_, err := Prepare(Plan{
		ID: "escape", HomeDir: home, ExecutablePath: executable,
		FromVersion: "1", ToVersion: "2", Candidate: []byte("new"),
		MetadataPaths: []string{"../foreign"},
	})
	require.Error(t, err)
}

func TestPrepareNeverDeletesPreexistingRecoveryArtifact(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("old"), 0o755))
	previousPath, _ := binaryArtifactPaths(executable, "collision")
	require.NoError(t, os.WriteFile(previousPath, []byte("owned-by-an-earlier-recovery"), 0o600))

	_, err := Prepare(Plan{
		ID: "collision", HomeDir: home, ExecutablePath: executable,
		FromVersion: "1", ToVersion: "2", Candidate: []byte("new"),
	})
	require.Error(t, err)
	got, readErr := os.ReadFile(previousPath)
	require.NoError(t, readErr)
	require.Equal(t, "owned-by-an-earlier-recovery", string(got),
		"refusing a collision must never clean up an artifact this call did not create")
}
