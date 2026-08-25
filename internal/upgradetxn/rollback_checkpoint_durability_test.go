package upgradetxn

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// stageInstalledCandidateAwaitingRollback reproduces the state #3453 is about: a
// candidate whose bytes ARE on the executable path, a journal that does not know
// it (CandidateInstalled=false), and a phase that rolls back. That combination is
// reachable because InstallCandidate's rename can land while the journal write
// that records it fails — which is exactly why restoreLocked re-reads the bytes
// and checkpoints the marker before overwriting them.
//
// It returns a transaction reloaded from that on-disk state, holding the recovery
// lease, plus the canonical upgrade root the journal is written into.
func stageInstalledCandidateAwaitingRollback(t *testing.T) (*RecoveryLease, *Transaction, string, string) {
	t.Helper()
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.Release())
	require.False(t, txn.Journal().CandidateInstalled,
		"precondition: PhaseDaemonStopped must not set CandidateInstalled")

	require.NoError(t, os.WriteFile(executable, []byte("candidate-binary"), 0o755),
		"the candidate is installed on disk while the journal still says it is not")

	journal := txn.Journal()
	journal.Phase = PhaseRollingBack
	data, err := json.MarshalIndent(journal, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(activeJournalPath(journal.HomeDir), append(data, '\n'), 0o600))

	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseRollingBack, loaded.Journal().Phase)
	require.False(t, loaded.Journal().CandidateInstalled)

	takeover, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = takeover.Release() })
	return takeover, loaded, home, upgradeRoot(loaded.Journal().HomeDir)
}

// failJournalDirectorySync makes the journal directory's fsync fail for the first
// `times` calls (-1 for every call), leaving every other directory alone.
//
// This is the failure durableAtomicWriteFile cannot make atomic: it renames FIRST
// and fsyncs the directory SECOND, so the caller gets an error over a file whose
// new content is already visible at the path.
func failJournalDirectorySync(t *testing.T, root string, times int, injected error) (heal func()) {
	t.Helper()
	previous := syncTransactionDirectory
	calls := 0
	syncTransactionDirectory = func(path string) error {
		if path == root {
			calls++
			if times < 0 || calls <= times {
				return injected
			}
		}
		return previous(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previous })
	return func() { syncTransactionDirectory = previous }
}

// #3453, the reported symptom. restoreLocked checkpoints CandidateInstalled=true;
// the directory sync fails after the rename, so the marker is ON DISK while the
// in-memory journal still says false; Rollback's error handler then writes
// PhaseRollbackFailed from that stale copy and ERASES the marker. The candidate
// had been installed and had run, and comes back eligible to be offered again —
// the one outcome the marker exists to prevent.
//
// Here the sync never heals, so the rollback still fails. What must not happen is
// losing the checkpoint on the way out.
func TestRollbackKeepsCandidateInstalledWhenCheckpointSyncNeverHeals(t *testing.T) {
	takeover, loaded, home, root := stageInstalledCandidateAwaitingRollback(t)
	injected := errors.New("injected checkpoint sync failure")
	_ = failJournalDirectorySync(t, root, -1, injected)

	err := takeover.Rollback()
	require.ErrorIs(t, err, injected)

	reloaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseRollbackFailed, reloaded.Journal().Phase,
		"precondition: the failed-rollback write is the one that used to overwrite the checkpoint")
	require.True(t, reloaded.Journal().CandidateInstalled,
		"the candidate was detected on disk and checkpointed, so the marker must survive the "+
			"PhaseRollbackFailed write — losing it lets an installed candidate escape disqualification")
	require.True(t, loaded.Journal().CandidateInstalled,
		"and the in-memory journal must have advanced to what is on disk, or the next write regresses it again")
}

// The transient half: the directory sync fails once, and persistJournal completes
// the barrier by hand — the same recovery publishActivationApproval has always
// made. The checkpoint is then genuinely durable, so the rollback is not failed by
// a barrier that did close, and the marker survives into the restored journal.
func TestRollbackCompletesCheckpointBarrierAfterATransientSyncFailure(t *testing.T) {
	takeover, _, home, root := stageInstalledCandidateAwaitingRollback(t)
	_ = failJournalDirectorySync(t, root, 1, errors.New("injected transient sync failure"))

	require.NoError(t, takeover.Rollback(),
		"a directory sync that succeeds on the completing retry must not fail the rollback")

	reloaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseRollbackRestored, reloaded.Journal().Phase)
	require.True(t, reloaded.Journal().CandidateInstalled,
		"the checkpoint landed and was confirmed, so the candidate stays disqualified")
}

// The other direction, so the recovery above cannot decay into "any failed write
// is accepted as visible". When the write never reaches the path at all — the
// rename itself failed — there is nothing to adopt: the error propagates and the
// in-memory journal must NOT advance past a record the disk does not have.
func TestPersistJournalRejectsAWriteThatNeverLanded(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	before := txn.Journal()

	previous := removeTransactionFile
	removeTransactionFile = func(string) error { return nil }
	t.Cleanup(func() { removeTransactionFile = previous })

	// A directory in place of the journal file: the rename cannot replace it, so
	// durableAtomicWriteFile fails BEFORE anything becomes visible.
	path := activeJournalPath(txn.Journal().HomeDir)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))

	journal := txn.Journal()
	journal.CandidateInstalled = true
	err := txn.persistJournalLocked(journal)
	require.Error(t, err)
	require.NotErrorIs(t, err, errJournalVisibleNotDurable,
		"a rename that never landed is a plain write failure, not a visible-but-undurable one")
	require.Equal(t, before.Phase, txn.Journal().Phase)
	require.False(t, txn.Journal().CandidateInstalled,
		"in-memory state must not advance past a record that is not on disk")
}

// #3453 review (Codex P1). Adopting a visible-but-unconfirmed journal keeps the
// landed FIELDS safe, but it must not make an unconfirmed PHASE look established.
// markRollbackFailed short-circuits on "already PhaseRollbackFailed" and returns
// success without another write, and finishFailedRollback reads that success as
// permission to run DisableRecoveryJob — over a terminal entry whose barrier never
// closed. A crash then loses the entry with the persistent actor already disarmed,
// stranding an earlier rollback phase with nothing left to finish it.
//
// finishTerminalRollbackFailure states the invariant directly: only the DURABLE
// rollback_failed phase is a circuit-breaker that may disable the persistent actor.
func TestRollbackFailureCircuitBreakerRequiresADurablePhase(t *testing.T) {
	takeover, loaded, _, root := stageInstalledCandidateAwaitingRollback(t)
	injected := errors.New("injected checkpoint sync failure")
	heal := failJournalDirectorySync(t, root, -1, injected)

	require.ErrorIs(t, takeover.Rollback(), injected)
	require.Equal(t, PhaseRollbackFailed, loaded.Journal().Phase,
		"precondition: the terminal phase was adopted from a write whose bytes are visible")

	require.ErrorIs(t, takeover.MarkRollbackFailed(), injected,
		"an unconfirmed terminal phase must not report the circuit breaker as established — "+
			"finishFailedRollback would let DisableRecoveryJob disarm the persistent actor")

	// And the retry is a real retry: once the barrier can close, the circuit
	// breaker establishes rather than failing forever.
	heal()
	require.NoError(t, takeover.MarkRollbackFailed())
	require.NoError(t, takeover.MarkRollbackFailed(), "and it is idempotent once durable")

	reloaded, err := Load(loaded.Journal().HomeDir)
	require.NoError(t, err)
	require.Equal(t, PhaseRollbackFailed, reloaded.Journal().Phase)
	require.True(t, reloaded.Journal().CandidateInstalled,
		"the checkpoint the whole fix exists for must still be there")
}
