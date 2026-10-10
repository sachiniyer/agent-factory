package upgradetxn

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoveryInvocationParserAcceptsOnlyExactInternalShape(t *testing.T) {
	invocation, matched, err := ParseRecoveryInvocation([]string{
		recoveryModeArgument,
		"--home", "/tmp/af home",
		"--transaction", "txn-2212",
	})
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, RecoveryInvocation{
		HomeDir:       "/tmp/af home",
		TransactionID: "txn-2212",
	}, invocation)

	_, matched, err = ParseRecoveryInvocation([]string{"sessions", "list"})
	require.NoError(t, err)
	require.False(t, matched)

	for _, args := range [][]string{
		{recoveryModeArgument},
		{recoveryModeArgument, "--home", "/tmp/home", "--transaction", "../escape"},
		{recoveryModeArgument, "--transaction", "txn", "--home", "/tmp/home"},
		{recoveryModeArgument, "--home", "", "--transaction", "txn"},
	} {
		_, matched, err = ParseRecoveryInvocation(args)
		require.True(t, matched)
		require.Error(t, err)
	}
}

func TestRecoveryActorRunnerStandsDownWhenAnotherActorWon(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
	superviseCalls := 0

	err, _ := runRecoveryActorWith(
		context.Background(), invocation,
		func(*Transaction) (*RecoveryLease, error) { return nil, ErrRecoveryActive },
		func(context.Context, *Transaction, *RecoveryLease) error {
			superviseCalls++
			return nil
		},
	)

	require.NoError(t, err,
		"a service-manager duplicate must exit successfully instead of entering Restart=on-failure")
	require.Zero(t, superviseCalls)
}

func TestRecoveryActorRunnerStandsDownForStaleTransactionBeforeAcquiring(t *testing.T) {
	_, home, _ := prepareFixture(t)
	acquireCalls := 0
	err, _ := runRecoveryActorWith(
		context.Background(),
		RecoveryInvocation{HomeDir: home, TransactionID: "different-transaction"},
		func(*Transaction) (*RecoveryLease, error) {
			acquireCalls++
			return nil, nil
		},
		func(context.Context, *Transaction, *RecoveryLease) error { return nil },
	)

	require.NoError(t, err,
		"a stale transaction job must not enter its service manager's restart policy")
	require.Zero(t, acquireCalls)
}

func TestRecoveryActorRunnerExitsCleanlyAfterJournalCleanup(t *testing.T) {
	home := t.TempDir()
	acquireCalls := 0
	err, _ := runRecoveryActorWith(
		context.Background(),
		RecoveryInvocation{HomeDir: home, TransactionID: "already-cleaned"},
		func(*Transaction) (*RecoveryLease, error) {
			acquireCalls++
			return nil, nil
		},
		func(context.Context, *Transaction, *RecoveryLease) error { return nil },
	)

	require.NoError(t, err)
	require.Zero(t, acquireCalls)
}

func TestRecoveryActorRunnerMapsOnlyDisarmedTerminalOutcomesToCleanExit(t *testing.T) {
	tests := []struct {
		name      string
		runErr    error
		wantError bool
	}{
		{name: "commit", runErr: nil},
		{name: "abort", runErr: ErrUpgradeAborted},
		{name: "rollback", runErr: ErrUpgradeRolledBack},
		{name: "rollback failed circuit breaker", runErr: ErrRollbackRecoveryFailed},
		{
			name:      "circuit breaker could not disable job",
			runErr:    errors.Join(ErrRollbackRecoveryFailed, ErrRecoveryJobDisableFailed),
			wantError: true,
		},
		{name: "ordinary supervisor failure", runErr: errors.New("health probe failed"), wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			txn, home, _ := prepareFixture(t)
			invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)

			err, _ = runRecoveryActorWith(
				context.Background(), invocation,
				func(*Transaction) (*RecoveryLease, error) { return lease, nil },
				func(context.Context, *Transaction, *RecoveryLease) error { return tc.runErr },
			)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			live, liveErr := txn.RecoveryActorLive()
			require.NoError(t, liveErr)
			require.False(t, live, "runner must release the flock on every exit path")
		})
	}
}

// Cleanup that fails after the real work succeeded must not be reported as the
// work failing. This actor's exit code is load-bearing: every terminal path
// returns nil precisely so the loaded unit's Restart=on-failure cannot undo the
// circuit breaker the supervisor just set. Joining a lease-release error into
// the return turned a committed upgrade into a non-zero exit, and the unit would
// then restart an actor for an upgrade that had already finished (#2960).
func TestRunRecoveryActor_LeaseReleaseFailureDoesNotFailTheRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runErr error
	}{
		{name: "commit", runErr: nil},
		{name: "abort", runErr: ErrUpgradeAborted},
		{name: "rollback", runErr: ErrUpgradeRolledBack},
		{name: "rollback failed circuit breaker", runErr: ErrRollbackRecoveryFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			txn, home, _ := prepareFixture(t)
			invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)

			// Close the handle out from under the lease so the actor's own
			// deferred Release fails for real. Deliberately NOT calling Release
			// here first: Release nils the handles, so a pre-call would leave the
			// actor releasing nothing and the test would pass against the very
			// bug it exists to catch. TestRecoveryLease_ReleaseFailsOnAClosedHandle
			// proves this fixture actually breaks Release.
			require.NoError(t, lease.file.Close())

			err, _ = runRecoveryActorWith(
				context.Background(), invocation,
				func(*Transaction) (*RecoveryLease, error) { return lease, nil },
				func(context.Context, *Transaction, *RecoveryLease) error { return tc.runErr },
			)
			require.NoError(t, err,
				"a failed lease release must not turn a completed recovery into a non-zero exit")
		})
	}
}

// The converse: a genuine supervision failure still fails, so the change did not
// make the actor swallow outcomes that must reach the exit code.
func TestRunRecoveryActor_SupervisionFailureStillFails(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)

	err, _ = runRecoveryActorWith(
		context.Background(), invocation,
		func(*Transaction) (*RecoveryLease, error) { return lease, nil },
		func(context.Context, *Transaction, *RecoveryLease) error {
			return errors.New("health probe failed")
		},
	)
	require.Error(t, err)
}

// Proves the fixture the test above relies on: a closed handle really does make
// Release fail. Kept separate because consuming the failure inside that test
// would neutralise it — Release nils the handles, so the actor would then have
// nothing left to fail on.
func TestRecoveryLease_ReleaseFailsOnAClosedHandle(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)

	require.NoError(t, lease.file.Close())
	require.Error(t, lease.Release(), "closing the handle must make Release fail")
}

// TestRecoveryActorRetriesWhenThePhaseEndedWithTheJobStillArmed is the #3098
// regression, at the layer that decides the process exit code.
//
// ErrRollbackRecoveryFailed is returned BOTH after the recovery job was disarmed
// (nothing is owed — exit 0 so Restart=on-failure cannot undo the circuit
// breaker) and from a record-rejection failure reached BEFORE the disarm, where
// the job is still enabled and a restart is exactly what should happen. The
// actor read one value for two outcomes and exited 0 for both, so a transient
// ledger write error waited for the next boot instead of the next second.
func TestRecoveryActorRetriesWhenThePhaseEndedWithTheJobStillArmed(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}

	stillArmed := errors.Join(
		ErrRollbackRecoveryFailed,
		ErrRecoveryJobStillArmed,
		errors.New("record the rolled-back candidate as rejected: disk full"),
	)

	err, _ := runRecoveryActorWith(
		context.Background(), invocation,
		func(t *Transaction) (*RecoveryLease, error) {
			return t.tryAcquireRecoveryAs(t.Journal().PreviousBinaryPath)
		},
		func(context.Context, *Transaction, *RecoveryLease) error { return stillArmed },
	)

	require.Error(t, err,
		"the phase ended before the recovery job was disarmed, so the actor still owes that work; "+
			"exiting 0 here is what stopped systemd from retrying it")
	require.ErrorIs(t, err, ErrRecoveryJobStillArmed)
}

// The other half, and the reason the check is ordered before the terminal list:
// a genuinely finished rollback failure — one that DID disarm the job — must
// still exit 0, or the unit restart-loops against a circuit breaker that is
// already in place (#2960).
func TestRecoveryActorStillExitsZeroOnceTheJobIsDisarmed(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}

	err, _ := runRecoveryActorWith(
		context.Background(), invocation,
		func(t *Transaction) (*RecoveryLease, error) {
			return t.tryAcquireRecoveryAs(t.Journal().PreviousBinaryPath)
		},
		func(context.Context, *Transaction, *RecoveryLease) error { return ErrRollbackRecoveryFailed },
	)

	require.NoError(t, err,
		"a terminal rollback failure reached AFTER the disarm must not restart-loop the unit")
}

// TestRunRecoveryActorWith_RealRollbackProducesCommitSignal drives the REAL
// Supervisor.Run (with stubbed SupervisorOperations) through a failed
// candidate validation → stopCandidateAndRestore → PhaseRollbackRestored →
// PhasePreviousStarting → PhasePreviousValidating → PhaseRolledBack. It
// verifies that runRecoveryActorWith returns nil AND PhaseRolledBack AND the
// journal is gone — the exact "nil + journal gone" signal the old daemon-layer
// predicate treated as proof of commit. The fix keys on the phase instead, so
// the returned PhaseRolledBack is the load-bearing property: it survives
// lease.Cleanup() in-memory (storage.go cleanup never mutates
// txn.journal.Phase) and is what tells RunUpgradeRecoveryActor this was a
// rollback, not a commit.
func TestRunRecoveryActorWith_RealRollbackProducesCommitSignal(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}
	supervisor := Supervisor{Operations: runtime.operations()}

	var capturedTxn *Transaction
	err, phase := runRecoveryActorWith(
		context.Background(), invocation,
		func(t *Transaction) (*RecoveryLease, error) {
			capturedTxn = t
			return t.tryAcquireRecoveryAs(t.Journal().PreviousBinaryPath)
		},
		supervisor.Run,
	)

	require.NoError(t, err,
		"a successful rollback must exit 0 so Restart=on-failure cannot undo the circuit breaker")
	require.Equal(t, PhaseRolledBack, phase,
		"the returned phase must be rolled_back, not committed — the signal the hand-off must gate on")
	require.Equal(t, PhaseRolledBack, capturedTxn.Journal().Phase,
		"the in-memory phase must survive lease.Cleanup() so runRecoveryActorWith can return it")

	_, loadErr := Load(home)
	require.ErrorIs(t, loadErr, ErrNoActiveTransaction,
		"the journal must be gone after rollback, identical to a committed transaction — "+
			"the signal the old journal-gone predicate could not distinguish from commit")
}

// TestRunRecoveryActorWith_RealAbortProducesCommitSignal drives the REAL
// Supervisor.Run through the PhaseSupervisorReady → ErrActivationNotAuthorized
// → PhaseAborted arm (the same pattern as
// TestSupervisorRefusesCallbackWithoutActorBoundApproval: the AwaitActivation
// override returns nil without authorizing, so the supervisor's
// ActivationAuthorized check fails and the previous daemon is never signalled
// to stop). It verifies runRecoveryActorWith returns nil AND PhaseAborted AND
// the journal is gone — the same indistinguishable-from-commit signal a
// rollback produces, but for the abort path. Gating on phase == PhaseCommitted
// excludes this, so no "committed upgrade candidate" WARNING is emitted after
// an abort.
func TestRunRecoveryActorWith_RealAbortProducesCommitSignal(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	invocation := RecoveryInvocation{HomeDir: home, TransactionID: txn.Journal().ID}
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
	operations := runtime.operations()
	operations.AwaitActivation = func(context.Context, Journal) error {
		runtime.calls = append(runtime.calls, "await-activation")
		return nil
	}
	supervisor := Supervisor{Operations: operations}

	var capturedTxn *Transaction
	err, phase := runRecoveryActorWith(
		context.Background(), invocation,
		func(t *Transaction) (*RecoveryLease, error) {
			capturedTxn = t
			return t.tryAcquireRecoveryAs(t.Journal().PreviousBinaryPath)
		},
		supervisor.Run,
	)

	require.NoError(t, err,
		"a successful abort must exit 0 so Restart=on-failure cannot undo the circuit breaker")
	require.Equal(t, PhaseAborted, phase,
		"the returned phase must be aborted, not committed — the signal the hand-off must gate on")
	require.Equal(t, PhaseAborted, capturedTxn.Journal().Phase,
		"the in-memory phase must survive lease.Cleanup() so runRecoveryActorWith can return it")

	_, loadErr := Load(home)
	require.ErrorIs(t, loadErr, ErrNoActiveTransaction,
		"the journal must be gone after abort, identical to a committed transaction — "+
			"the signal the old journal-gone predicate could not distinguish from commit")
}
