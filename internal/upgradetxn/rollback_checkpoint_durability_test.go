package upgradetxn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
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

// #3453 review (Codex P2). rollback()'s "already past the restore" arm returned a
// bare nil, so a retried Rollback on a transaction that ADOPTED an unconfirmed
// PhaseRollbackRestored reported the boundary as durable without another barrier
// attempt — the same skip as the lifecycle operations, in the one place that had
// been missed.
func TestRollbackRetryReaffirmsAnAdoptedRestoredPhase(t *testing.T) {
	txn, _, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Release() })
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	_ = executable

	// Fail only the LAST journal write of the rollback — the PhaseRollbackRestored
	// boundary — so the transaction lands in the adopted state under test.
	injected := errors.New("injected restored-boundary sync failure")
	root := upgradeRoot(txn.Journal().HomeDir)
	previous := syncTransactionDirectory
	failFrom := false
	syncTransactionDirectory = func(path string) error {
		if path == root && failFrom {
			return injected
		}
		return previous(path)
	}
	t.Cleanup(func() { syncTransactionDirectory = previous })
	// Read the count OUTSIDE the callback: afterRollbackCheckpoint runs with the
	// transaction mutex held, so txn.Journal() in there self-deadlocks.
	metadataCount := len(txn.Journal().Metadata)
	require.NotZero(t, metadataCount, "the fixture must restore some metadata for this to hook the last checkpoint")
	txn.afterRollbackCheckpoint = func(progress RollbackProgress) error {
		if progress.BinaryRestored && progress.MetadataRestored >= metadataCount {
			failFrom = true
		}
		return nil
	}

	require.ErrorIs(t, lease.Rollback(), injected)
	require.Equal(t, PhaseRollbackRestored, txn.Journal().Phase,
		"precondition: the restored boundary was adopted from a visible write")

	// The retry must re-attempt the barrier, not report the adopted phase as done.
	require.ErrorIs(t, lease.Rollback(), injected,
		"a retried rollback must retry the barrier rather than confirm an adopted boundary")

	failFrom = false
	require.NoError(t, lease.Rollback(), "and once the barrier can close, the retry settles it")
}

// #3453 review (Codex P1). Supervisor.Run reads Journal().Phase and acts on it
// directly — its PhaseCommitted arm approves the candidate, disables the recovery
// job, and starts cleanup without ever calling Commit. An ADOPTED PhaseCommitted
// would run those irreversible effects over a journal a crash can still lose,
// recovering the older candidate_validating entry with no actor left to finish it.
//
// So Run closes the barrier before reading the phase. Here it cannot close, and the
// assertion is that none of those effects ran.
func TestSupervisorRunRefusesToActOnAnAdoptedCommittedPhase(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Release() })
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))

	injected := errors.New("injected commit sync failure")
	_ = failJournalDirectorySync(t, upgradeRoot(txn.Journal().HomeDir), -1, injected)

	require.Error(t, lease.Commit())
	require.Equal(t, PhaseCommitted, txn.Journal().Phase,
		"precondition: the committed phase was adopted from a visible write")

	var approved, disabled int
	supervisor := Supervisor{Operations: SupervisorOperations{
		AwaitActivation:    func(context.Context, Journal) error { return nil },
		StopPrevious:       func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartCandidate:     func(context.Context, Journal) error { return nil },
		ValidateCandidate:  func(context.Context, Journal) error { return nil },
		ApproveCandidate:   func(context.Context, Journal) error { approved++; return nil },
		StopCandidate:      func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartPrevious:      func(context.Context, Journal) error { return nil },
		ValidatePrevious:   func(context.Context, Journal) error { return nil },
		DisableRecoveryJob: func(context.Context, Journal) error { disabled++; return nil },
	}}

	err = supervisor.Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, injected,
		"Run must refuse to act on a phase whose barrier it cannot close")
	assert.Zero(t, approved, "an adopted commit must not approve the candidate")
	assert.Zero(t, disabled, "and must not disarm the recovery job over a journal a crash can lose")
}

// #3453 review (cross-process takeover). The test above proves Supervisor.Run
// refuses to act on an ADOPTED PhaseCommitted whose barrier never closed, but
// only because the SAME process that saw the visible-but-undurable write still
// holds journalUnconfirmed=true.
//
// A fresh process that wins recovery after the writer died has no such memory:
// it goes through Load() (journalUnconfirmed defaults to false) and then
// tryAcquireRecoveryAs, which reads the journal from disk. Before the fix that
// read adopted the bytes without marking them unconfirmed, so
// reaffirmDurableJournal became a no-op and the PhaseCommitted arm approved the
// candidate and disarmed the recovery job over a journal a crash can still lose —
// stranding the candidate_validating entry with no actor left to finish it.
//
// A takeover has strictly LESS information about durability than the writer, so
// it must be at least as conservative: mark every disk read in takeover as
// unconfirmed so the barrier is re-closed before any irreversible arm runs.
func TestCrossProcessTakeoverMustReaffirmUnconfirmedJournal(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))

	injected := errors.New("injected commit sync failure")
	home := txn.Journal().HomeDir
	_ = failJournalDirectorySync(t, upgradeRoot(home), -1, injected)

	require.Error(t, lease.Commit())
	require.Equal(t, PhaseCommitted, txn.Journal().Phase,
		"precondition: the committed phase was adopted from a visible write")

	// The writer died here. Release the lease and reload from disk so the
	// takeover process has no in-memory memory of the unconfirmed write.
	require.NoError(t, lease.Release())

	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseCommitted, loaded.Journal().Phase,
		"precondition: the on-disk journal is the visible-but-undurable commit")

	takeover, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = takeover.Release() })

	var approved, disabled int
	supervisor := Supervisor{Operations: SupervisorOperations{
		AwaitActivation:    func(context.Context, Journal) error { return nil },
		StopPrevious:       func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartCandidate:     func(context.Context, Journal) error { return nil },
		ValidateCandidate:  func(context.Context, Journal) error { return nil },
		ApproveCandidate:   func(context.Context, Journal) error { approved++; return nil },
		StopCandidate:      func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartPrevious:      func(context.Context, Journal) error { return nil },
		ValidatePrevious:   func(context.Context, Journal) error { return nil },
		DisableRecoveryJob: func(context.Context, Journal) error { disabled++; return nil },
	}}

	err = supervisor.Run(context.Background(), loaded, takeover)
	require.ErrorIs(t, err, injected,
		"a takeover must refuse to act on a phase whose durability it cannot prove")
	assert.Zero(t, approved, "an adopted commit must not approve the candidate — even across processes")
	assert.Zero(t, disabled, "and must not disarm the recovery job over a journal a crash can lose — even across processes")
}

// #3453 review (cross-process takeover, transient failure). The same takeover
// as the test above, but the directory sync heals before the takeover process
// runs. The takeover's re-persistence then closes the barrier and the
// PhaseCommitted arm settles exactly once — the fix must not make a healthy
// takeover fail forever, nor run the irreversible effects twice.
//
// This is the cross-process counterpart of
// TestRollbackCompletesCheckpointBarrierAfterATransientSyncFailure: an unconfirmed
// phase adopted from disk becomes durable the moment the barrier can close, and
// the recovery proceeds rather than choking on a barrier the prior writer left
// open.
func TestCrossProcessTakeoverReaffirmsAndThenActsWhenSyncHeals(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.InstallCandidate())
	require.NoError(t, lease.Advance(PhaseCandidateStarting))
	require.NoError(t, lease.Advance(PhaseCandidateValidating))

	injected := errors.New("injected commit sync failure")
	home := txn.Journal().HomeDir
	root := upgradeRoot(home)
	// Always fail during the in-process commit so the journal lands on disk
	// visible but undurable, exactly the state a takeover inherits.
	heal := failJournalDirectorySync(t, root, -1, injected)

	require.Error(t, lease.Commit())
	require.Equal(t, PhaseCommitted, txn.Journal().Phase,
		"precondition: the committed phase was adopted from a visible write")

	require.NoError(t, lease.Release())

	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseCommitted, loaded.Journal().Phase)

	// The directory sync heals before the takeover runs, so the takeover's
	// re-persistence closes the barrier and the committed arm settles once.
	heal()

	takeover, err := loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = takeover.Release() })

	var approved, disabled int
	supervisor := Supervisor{Operations: SupervisorOperations{
		AwaitActivation:    func(context.Context, Journal) error { return nil },
		StopPrevious:       func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartCandidate:     func(context.Context, Journal) error { return nil },
		ValidateCandidate:  func(context.Context, Journal) error { return nil },
		ApproveCandidate:   func(context.Context, Journal) error { approved++; return nil },
		StopCandidate:      func(context.Context, Journal) (StopOutcome, error) { return StopConfirmed, nil },
		StartPrevious:      func(context.Context, Journal) error { return nil },
		ValidatePrevious:   func(context.Context, Journal) error { return nil },
		DisableRecoveryJob: func(context.Context, Journal) error { disabled++; return nil },
	}}

	require.NoError(t, supervisor.Run(context.Background(), loaded, takeover),
		"once the barrier closes, the takeover must settle the committed phase")
	assert.Equal(t, 1, approved, "the committed candidate is approved exactly once")
	assert.Equal(t, 1, disabled, "the recovery job is disarmed exactly once")
}
