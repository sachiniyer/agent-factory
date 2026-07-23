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

	err := runRecoveryActorWith(
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
	err := runRecoveryActorWith(
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
	err := runRecoveryActorWith(
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

			err = runRecoveryActorWith(
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
