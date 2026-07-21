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
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
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

func TestRollbackRemovesAbsentMetadataUnderCandidatePrivateParent(t *testing.T) {
	home := t.TempDir()
	plan := basicPreparePlan(t, home, "candidate-private-parent")
	plan.MetadataPaths = []string{filepath.Join("candidate", "tasks.json")}
	txn, err := Prepare(plan)
	require.NoError(t, err)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())

	parent := filepath.Join(home, "candidate")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "tasks.json"), []byte("candidate-state"), 0o600))
	require.NoError(t, os.Chmod(parent, 0o500))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	require.NoError(t, lease.Rollback(),
		"rollback must temporarily make candidate-created parents writable to restore recorded absence")
	require.NoFileExists(t, filepath.Join(parent, "tasks.json"))
	info, err := os.Stat(parent)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o500), info.Mode().Perm(),
		"temporary rollback permissions must not broaden a surviving candidate-created directory")
}

func TestRollbackResyncsAlreadyAbsentMetadataBeforeCheckpoint(t *testing.T) {
	home := t.TempDir()
	plan := basicPreparePlan(t, home, "absence-retry-sync")
	plan.MetadataPaths = []string{"tasks.json"}
	txn, err := Prepare(plan)
	require.NoError(t, err)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, os.WriteFile(filepath.Join(home, "tasks.json"), []byte("candidate-state"), 0o600))

	previousSync := syncTransactionDirectory
	canonicalHome := txn.Journal().HomeDir
	syncTransactionDirectory = func(path string) error {
		if path == canonicalHome {
			return errRecoveryCheckpointInterrupted
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })

	err = lease.Rollback()
	require.ErrorIs(t, err, errRecoveryCheckpointInterrupted)
	require.NoFileExists(t, filepath.Join(home, "tasks.json"))
	require.Equal(t, PhaseRollingBack, txn.Journal().Phase)
	require.Equal(t, 0, txn.Journal().RollbackProgress.MetadataRestored)

	err = lease.Rollback()
	require.ErrorIs(t, err, errRecoveryCheckpointInterrupted,
		"a retry must make the already-observed absence durable before checkpointing it")
	require.Equal(t, PhaseRollingBack, txn.Journal().Phase)
	require.Equal(t, 0, txn.Journal().RollbackProgress.MetadataRestored)
}

func TestRollbackResumesFromDurablePerFileCheckpoints(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
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

func TestRecoveryLockReplacementCannotCreateSecondActor(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	stale, err := Load(home)
	require.NoError(t, err)
	first, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer first.Release()

	journal := txn.Journal()
	lockPath := first.file.Name()
	readyPath := filepath.Join(transactionDir(journal.HomeDir, journal.ID), "recovery.ready.lock")
	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, os.WriteFile(lockPath, []byte(journal.RecoveryNonce+"\n"), journalFileMode))
	require.NoError(t, os.Remove(readyPath))
	require.NoError(t, os.WriteFile(readyPath, nil, journalFileMode))

	second, err := stale.tryAcquireRecoveryAs(stale.Journal().PreviousBinaryPath)
	if second != nil {
		defer second.Release()
	}
	require.Error(t, err,
		"replacing lock pathnames must not create a second recovery authority over new inodes")
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
	authorized, err := lease.ActivationAuthorized()
	require.NoError(t, err)
	require.True(t, authorized)
	require.NoError(t, lease.Release())
}

func TestActivationApprovalCannotBeConsumedByTakeoverActor(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	first, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, first.Advance(PhaseSupervisorReady))
	require.NoError(t, first.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))
	require.NoError(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce))
	require.NoError(t, first.Release())

	takeover, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	defer takeover.Release()
	require.NoError(t, takeover.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))
	authorized, err := takeover.ActivationAuthorized()
	require.NoError(t, err)
	require.False(t, authorized)
	require.NoError(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce))
	authorized, err = takeover.ActivationAuthorized()
	require.NoError(t, err)
	require.True(t, authorized, "the old daemon can explicitly authorize the new lock owner")
}

func TestActivationApprovalRequiresConsumerDirectorySync(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	lease, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))

	injected := errors.New("injected approval directory sync failure")
	previousSync := syncTransactionDirectory
	txnDir := transactionDir(journal.HomeDir, journal.ID)
	syncTransactionDirectory = func(path string) error {
		if path == txnDir {
			return injected
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })

	err = txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce)
	require.ErrorIs(t, err, injected)
	authorized, readErr := lease.ActivationAuthorized()
	require.ErrorIs(t, readErr, injected,
		"a visible approval is not authority until its directory entry is durable")
	require.False(t, authorized)
}

func TestActivationApprovalCompletesVisibleDirectorySync(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	lease, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))

	injected := errors.New("injected first approval directory sync failure")
	previousSync := syncTransactionDirectory
	txnDir := transactionDir(journal.HomeDir, journal.ID)
	approvalSyncs := 0
	syncTransactionDirectory = func(path string) error {
		if path == txnDir {
			approvalSyncs++
			if approvalSyncs == 1 {
				return injected
			}
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })

	require.NoError(t, txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce),
		"a visible exact approval should complete its interrupted durability barrier")
	authorized, err := lease.ActivationAuthorized()
	require.NoError(t, err)
	require.True(t, authorized)
	require.GreaterOrEqual(t, approvalSyncs, 3,
		"the writer retry and consumer must each confirm the approval directory")
}

func TestActivationRejectsExpiredSupervisorLease(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	lease, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(-time.Second)))

	err = txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce)
	require.Error(t, err,
		"a live-but-stuck actor whose own phase deadline expired is not ready to own shutdown")
	require.ErrorContains(t, err, "deadline expired")
	authorized, readErr := lease.ActivationAuthorized()
	require.NoError(t, readErr)
	require.False(t, authorized)
	require.NoError(t, lease.Release())
}

func TestTakeoverInvalidatesPreviousActorsSupervisorReadyHeartbeat(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	journal := txn.Journal()
	first, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, first.Advance(PhaseSupervisorReady))
	require.NoError(t, first.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Hour)))
	require.NoError(t, first.Release())

	takeover, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	defer takeover.Release()
	err = txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce)
	require.Error(t, err,
		"the new flock owner must publish its own heartbeat before it can authorize shutdown")
}

func TestTakeoverCannotBorrowStaleHeartbeatBeforeInvalidationFinishes(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	journal := txn.Journal()
	first, err := txn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, first.Advance(PhaseSupervisorReady))
	require.NoError(t, first.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Hour)))
	require.NoError(t, first.Release())
	takeoverTxn, err := Load(home)
	require.NoError(t, err)

	statusPath := filepath.Join(transactionDir(journal.HomeDir, journal.ID), "recovery.json")
	removeStarted := make(chan struct{})
	continueRemove := make(chan struct{})
	previousRemove := removeTransactionFile
	removeTransactionFile = func(path string) error {
		if path == statusPath {
			close(removeStarted)
			<-continueRemove
		}
		return previousRemove(path)
	}
	t.Cleanup(func() { removeTransactionFile = previousRemove })
	type acquisition struct {
		lease *RecoveryLease
		err   error
	}
	acquired := make(chan acquisition, 1)
	go func() {
		lease, acquireErr := takeoverTxn.tryAcquireRecoveryAs(journal.PreviousBinaryPath)
		acquired <- acquisition{lease: lease, err: acquireErr}
	}()
	select {
	case <-removeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("takeover did not reach stale-status invalidation")
	}
	authorizeErr := txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce)
	close(continueRemove)
	result := <-acquired
	require.NoError(t, result.err)
	require.NoError(t, result.lease.Release())
	require.Error(t, authorizeErr,
		"holding the death-test lock is insufficient until this actor publishes its own readiness proof")
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

func TestDaemonStopRequiresDurableIntentBoundary(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))

	err = lease.Advance(PhaseDaemonStopped)
	require.Error(t, err, "daemon shutdown cannot precede its durable intent boundary")
	require.Equal(t, PhaseSupervisorReady, txn.Journal().Phase)
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
}

func TestCommitIsDurableBeforeCleanup(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
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

func TestCommitRejectsCandidateWithChangedExecutableMode(t *testing.T) {
	txn, _, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, os.Chmod(executable, 0o644))

	err = lease.Commit()
	require.Error(t, err, "validation must not commit a candidate that cannot be executed on restart")
	require.Equal(t, PhaseCandidateValidating, txn.Journal().Phase)
	require.FileExists(t, txn.Journal().PreviousBinaryPath,
		"rollback material must remain after refusing the candidate")
}

func TestTerminalCleanupRecoversAfterTransactionDirectoryDisappears(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
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
	for _, artifact := range []string{
		journal.PreviousBinaryPath,
		journal.CandidatePath,
		recoveryLockPath(journal.HomeDir, journal.ID),
	} {
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

func TestPrepareRejectsByteIdenticalCandidateIdentity(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("same-binary"), 0o755))

	_, err := Prepare(Plan{
		ID: "identical", HomeDir: home, ExecutablePath: executable,
		FromVersion: "1", ToVersion: "2", Candidate: []byte("same-binary"),
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	})
	require.Error(t, err,
		"previous and candidate roles must remain structurally distinguishable by digest")
	require.ErrorContains(t, err, "byte-identical")
}

func TestRollbackRefusesBeforeDaemonStop(t *testing.T) {
	for _, test := range []struct {
		name  string
		ready bool
	}{
		{name: "prepared"},
		{name: "supervisor ready", ready: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			txn, home, _ := prepareFixture(t)
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			defer lease.Release()
			if test.ready {
				require.NoError(t, lease.Advance(PhaseSupervisorReady))
			}
			statePath := filepath.Join(home, "state.json")
			require.NoError(t, os.WriteFile(statePath, []byte("live-daemon-write"), 0o640))

			err = lease.Rollback()
			require.Error(t, err, "rollback must not overwrite metadata before daemon shutdown is proven")
			got, readErr := os.ReadFile(statePath)
			require.NoError(t, readErr)
			require.Equal(t, "live-daemon-write", string(got))
		})
	}
}

func TestCleanupKeepsPreviousActorUntilActiveJournalIsRemoved(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, lease.Commit())

	injected := errors.New("injected active journal removal failure")
	activePath := activeJournalPath(txn.Journal().HomeDir)
	previousRemove := removeTransactionFile
	removeTransactionFile = func(path string) error {
		if path == activePath {
			return injected
		}
		return previousRemove(path)
	}
	t.Cleanup(func() { removeTransactionFile = previousRemove })
	err = lease.Cleanup()
	require.ErrorIs(t, err, injected, "the injected active-journal removal failure must be observed")
	require.DirExists(t, transactionDir(home, txn.Journal().ID),
		"the flock inode must not be unlinked while active.json can still authorize takeover")
	require.FileExists(t, txn.Journal().PreviousBinaryPath,
		"the only executable authorized to resume terminal cleanup must remain while active.json exists")
}

func TestPrepareRejectsSymlinkedUpgradeStorage(t *testing.T) {
	for _, test := range []struct {
		name string
		link func(t *testing.T, home, external string)
	}{
		{
			name: "upgrade root",
			link: func(t *testing.T, home, external string) {
				require.NoError(t, os.Symlink(external, upgradeRoot(home)))
			},
		},
		{
			name: "transactions root",
			link: func(t *testing.T, home, external string) {
				require.NoError(t, os.Mkdir(upgradeRoot(home), 0o700))
				require.NoError(t, os.Symlink(external, filepath.Join(upgradeRoot(home), "transactions")))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			external := t.TempDir()
			test.link(t, home, external)
			plan := basicPreparePlan(t, home, "symlink-storage")

			_, err := Prepare(plan)
			require.Error(t, err, "upgrade transaction storage must remain inside the canonical AF home")
		})
	}
}

func TestCleanupRejectsStorageRootReplacedBySymlink(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, lease.Commit())

	journal := txn.Journal()
	root := upgradeRoot(journal.HomeDir)
	realRoot := root + ".real"
	require.NoError(t, os.Rename(root, realRoot))
	external := t.TempDir()
	externalTxn := filepath.Join(external, "transactions", journal.ID)
	require.NoError(t, os.MkdirAll(externalTxn, 0o700))
	sentinel := filepath.Join(externalTxn, "must-survive")
	require.NoError(t, os.WriteFile(sentinel, []byte("foreign"), 0o600))
	require.NoError(t, os.Symlink(external, root))

	err = lease.Cleanup()
	require.Error(t, err)
	require.FileExists(t, sentinel, "cleanup must never traverse a symlinked storage ancestor")
	require.FileExists(t, journal.PreviousBinaryPath)
}

func TestRecoveryLoadsJournalWhenCandidateReplacesMetadataParentWithSymlink(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Release())

	instancesDir := filepath.Join(home, "instances")
	require.NoError(t, os.RemoveAll(instancesDir))
	external := t.TempDir()
	sentinel := filepath.Join(external, "must-survive")
	require.NoError(t, os.WriteFile(sentinel, []byte("foreign"), 0o600))
	require.NoError(t, os.Symlink(external, instancesDir))

	loaded, err := Load(home)
	require.NoError(t, err,
		"mutable candidate state must not prevent the previous binary from loading its recovery journal")
	recovery, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	err = recovery.Rollback()
	require.Error(t, err, "rollback must reject rather than traverse the candidate-created symlink")
	require.NoError(t, recovery.Release())
	require.Equal(t, PhaseRollbackFailed, loaded.Journal().Phase)
	require.FileExists(t, sentinel)
	got, readErr := os.ReadFile(executable)
	require.NoError(t, readErr)
	require.Equal(t, "known-running-binary", string(got))
}

func TestPrepareRejectsRecoveryJobMismatchedToDaemonOwner(t *testing.T) {
	for _, owner := range []DaemonOwner{
		{Kind: SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
		{Kind: SupervisionLaunchd, ServiceName: "com.agent-factory.daemon"},
	} {
		t.Run(string(owner.Kind), func(t *testing.T) {
			home := t.TempDir()
			plan := basicPreparePlan(t, home, "owner-job-mismatch")
			plan.Daemon = DaemonSnapshot{WasRunning: true, BootID: "boot-1", Owner: owner}

			_, err := Prepare(plan)
			require.Error(t, err, "service-managed daemons require the matching persistent recovery job kind")
		})
	}
}

func TestRollbackRestoresPrivateMetadataParentModes(t *testing.T) {
	home := t.TempDir()
	privateDir := filepath.Join(home, "private")
	nestedDir := filepath.Join(privateDir, "nested")
	require.NoError(t, os.Mkdir(privateDir, 0o710))
	require.NoError(t, os.Mkdir(nestedDir, 0o700))
	metadataPath := filepath.Join(nestedDir, "state.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte("private-state"), 0o600))
	plan := basicPreparePlan(t, home, "parent-modes")
	plan.MetadataPaths = []string{filepath.Join("private", "nested", "state.json")}
	txn, err := Prepare(plan)
	require.NoError(t, err)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, os.RemoveAll(privateDir))

	require.NoError(t, lease.Rollback())
	privateInfo, err := os.Stat(privateDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o710), privateInfo.Mode().Perm())
	nestedInfo, err := os.Stat(nestedDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), nestedInfo.Mode().Perm())
}

func TestPersistJournalReturnsDirectorySyncFailure(t *testing.T) {
	injected := errors.New("injected directory sync failure")
	previousSync := syncTransactionDirectory
	syncTransactionDirectory = func(string) error { return injected }
	t.Cleanup(func() { syncTransactionDirectory = previousSync })

	// Production journal paths are derived from Prepare's canonical AF home.
	// Mirror that boundary because macOS exposes its temp root through /var,
	// which is a symlink to /private/var.
	directory, err := canonicalExistingDir(t.TempDir())
	require.NoError(t, err)
	err = persistJournal(filepath.Join(directory, "active.json"), Journal{ID: "durability"})
	require.ErrorIs(t, err, injected)
}

func TestPrepareFsyncsCreatedTransactionDirectoriesBeforePublishing(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func(home string) string
	}{
		{
			name: "transaction directory entry",
			target: func(home string) string {
				return filepath.Join(upgradeRoot(home), "transactions")
			},
		},
		{
			name: "metadata directory entry",
			target: func(home string) string {
				return transactionDir(home, "directory-fsync")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			canonicalHome, err := canonicalExistingDir(home)
			require.NoError(t, err)
			injected := errors.New("injected directory creation sync failure")
			target := test.target(canonicalHome)
			previousSync := syncTransactionDirectory
			targetSyncs := 0
			syncTransactionDirectory = func(path string) error {
				if path == target {
					targetSyncs++
					if targetSyncs == 2 {
						return injected
					}
				}
				return nil
			}
			t.Cleanup(func() { syncTransactionDirectory = previousSync })

			_, err = Prepare(basicPreparePlan(t, home, "directory-fsync"))
			require.ErrorIs(t, err, injected)
			require.Equal(t, 2, targetSyncs,
				"the newly-created child must be made durable through its parent after that parent is durable itself")
			_, statErr := os.Stat(activeJournalPath(canonicalHome))
			require.ErrorIs(t, statErr, os.ErrNotExist,
				"the transaction must not be published after a directory durability failure")
		})
	}
}

func TestCleanupKeepsRecoveryAuthorityWhenActiveRemovalSyncFails(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))
	require.NoError(t, lease.Commit())

	injected := errors.New("injected active removal sync failure")
	previousSync := syncTransactionDirectory
	root := upgradeRoot(txn.Journal().HomeDir)
	syncTransactionDirectory = func(path string) error {
		if path == root {
			return injected
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })
	err = lease.Cleanup()
	require.ErrorIs(t, err, injected)
	require.DirExists(t, transactionDir(home, txn.Journal().ID))
	require.FileExists(t, txn.Journal().PreviousBinaryPath)

	err = lease.Cleanup()
	require.ErrorIs(t, err, injected,
		"a retry must durably confirm active.json absence before deleting recovery authority")
	require.DirExists(t, transactionDir(home, txn.Journal().ID))
	require.FileExists(t, txn.Journal().PreviousBinaryPath)

	syncTransactionDirectory = previousSync
	require.NoError(t, lease.Cleanup())
}

func TestPrepareRetainsRecoveryInputsWhenJournalPublishSyncFails(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := canonicalExistingDir(home)
	require.NoError(t, err)
	plan := basicPreparePlan(t, home, "publish-sync-failure")
	injected := errors.New("injected active journal sync failure")
	previousSync := syncTransactionDirectory
	activePath := activeJournalPath(canonicalHome)
	publishSyncs := 0
	syncTransactionDirectory = func(path string) error {
		if path == upgradeRoot(canonicalHome) {
			if _, statErr := os.Lstat(activePath); statErr == nil {
				publishSyncs++
				return injected
			}
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })

	_, err = Prepare(plan)
	require.ErrorIs(t, err, injected)
	require.Equal(t, 1, publishSyncs)
	journal, readErr := readJournal(activePath)
	require.NoError(t, readErr, "a visible journal must retain everything needed for later recovery")
	require.FileExists(t, journal.PreviousBinaryPath)
	require.FileExists(t, journal.CandidatePath)
	require.DirExists(t, transactionDir(home, journal.ID))
}

func TestMetadataParentModesAreRestoredEvenWhenRollbackFails(t *testing.T) {
	home := t.TempDir()
	privateDir := filepath.Join(home, "private")
	nestedDir := filepath.Join(privateDir, "nested")
	require.NoError(t, os.Mkdir(privateDir, 0o710))
	require.NoError(t, os.Mkdir(nestedDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "state.json"), []byte("state"), 0o600))
	plan := basicPreparePlan(t, home, "parent-mode-failure")
	plan.MetadataPaths = []string{filepath.Join("private", "nested", "state.json")}
	txn, err := Prepare(plan)
	require.NoError(t, err)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, os.RemoveAll(privateDir))

	injected := errors.New("injected recreated parent sync failure")
	previousSync := syncTransactionDirectory
	canonicalPrivateDir := filepath.Join(txn.Journal().HomeDir, "private")
	syncTransactionDirectory = func(path string) error {
		if path == canonicalPrivateDir {
			return injected
		}
		return previousSync(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previousSync })
	err = lease.Rollback()
	require.ErrorIs(t, err, injected)
	info, statErr := os.Stat(privateDir)
	require.NoError(t, statErr)
	require.Equal(t, os.FileMode(0o710), info.Mode().Perm(),
		"a failed rollback must not leave private parents broadened")
}

func TestPrepareAcceptsRecoveryJobMatchingDaemonOwner(t *testing.T) {
	for _, test := range []struct {
		owner DaemonOwner
		kind  RecoveryJobKind
	}{
		{
			owner: DaemonOwner{Kind: SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
			kind:  RecoveryJobSystemd,
		},
		{
			owner: DaemonOwner{Kind: SupervisionLaunchd, ServiceName: "com.agent-factory.daemon"},
			kind:  RecoveryJobLaunchd,
		},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			home := t.TempDir()
			plan := basicPreparePlan(t, home, "matching-owner-job")
			plan.Daemon = DaemonSnapshot{WasRunning: true, BootID: "boot-1", Owner: test.owner}
			job, err := NewRecoveryJob(test.kind, plan.ID, t.TempDir())
			require.NoError(t, err)
			plan.RecoveryJob = job

			_, err = Prepare(plan)
			require.NoError(t, err)
		})
	}
}

func basicPreparePlan(t *testing.T, home, id string) Plan {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("known-running"), 0o755))
	return Plan{
		ID: id, HomeDir: home, ExecutablePath: executable,
		FromVersion: "1", ToVersion: "2", Candidate: []byte("candidate"),
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	}
}
