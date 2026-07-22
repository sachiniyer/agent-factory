package upgradetxn

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEntrypointGateProceedsWhenNoTransactionExists(t *testing.T) {
	home := t.TempDir()
	wakeCalls := 0
	gate := EntrypointGate{
		WakeRecovery: func(context.Context, *Transaction) error {
			wakeCalls++
			return nil
		},
	}

	require.NoError(t, gate.Check(context.Background(), home))
	require.Zero(t, wakeCalls)
}

func TestEntrypointGateProceedsBeforeAFirstRunHomeExists(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created-yet")
	wakeCalls := 0
	gate := EntrypointGate{
		WakeRecovery: func(context.Context, *Transaction) error {
			wakeCalls++
			return nil
		},
	}

	require.NoError(t, gate.Check(context.Background(), home))
	require.Zero(t, wakeCalls)
}

func TestEntrypointGateReportsFreshLiveRecoveryWithoutWakingARival(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	now := time.Date(2026, 7, 22, 1, 2, 3, 0, time.UTC)
	require.NoError(t, lease.Heartbeat(PhasePrepared, now.Add(time.Minute)))
	wakeCalls := 0
	gate := EntrypointGate{
		Now: func() time.Time { return now },
		WakeRecovery: func(context.Context, *Transaction) error {
			wakeCalls++
			return nil
		},
	}

	err = gate.Check(context.Background(), home)
	var inProgress *UpgradeInProgressError
	require.ErrorAs(t, err, &inProgress)
	require.Equal(t, PhasePrepared, inProgress.Phase)
	require.Equal(t, txn.Journal().ToVersion, inProgress.ToVersion)
	require.Zero(t, wakeCalls)
}

func TestEntrypointGateTreatsFlockAsLifeEvenAfterHeartbeatDeadline(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	now := time.Date(2026, 7, 22, 1, 2, 3, 0, time.UTC)
	require.NoError(t, lease.Heartbeat(PhaseCandidateValidating, now.Add(-time.Second)))
	wakeCalls := 0
	gate := EntrypointGate{
		Now: func() time.Time { return now },
		WakeRecovery: func(context.Context, *Transaction) error {
			wakeCalls++
			return nil
		},
	}

	err = gate.Check(context.Background(), home)
	var blocked *UpgradeRecoveryBlockedError
	require.ErrorAs(t, err, &blocked)
	require.ErrorContains(t, err, "still owns the recovery lock")
	require.Zero(t, wakeCalls,
		"an expired timestamp is never authority to start or signal another actor")
}

func TestEntrypointGateWakesPreviousBinaryAfterKernelProvesActorDeath(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	var takeover *RecoveryLease
	wakeCalls := 0
	gate := EntrypointGate{
		Now: func() time.Time { return time.Now().UTC() },
		WakeRecovery: func(_ context.Context, loaded *Transaction) error {
			wakeCalls++
			var err error
			takeover, err = loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
			if err != nil {
				return err
			}
			return takeover.Heartbeat(loaded.Journal().Phase, time.Now().Add(time.Minute))
		},
	}
	defer func() {
		if takeover != nil {
			require.NoError(t, takeover.Release())
		}
	}()

	err := gate.Check(context.Background(), home)
	var inProgress *UpgradeInProgressError
	require.ErrorAs(t, err, &inProgress)
	require.Equal(t, 1, wakeCalls)
	require.Equal(t, txn.Journal().ID, inProgress.TransactionID)
}

func TestEntrypointGateLosingTakeoverRaceObservesWinner(t *testing.T) {
	_, home, _ := prepareFixture(t)
	var winner *RecoveryLease
	wakeCalls := 0
	gate := EntrypointGate{
		WakeRecovery: func(_ context.Context, loaded *Transaction) error {
			wakeCalls++
			var err error
			winner, err = loaded.tryAcquireRecoveryAs(loaded.Journal().PreviousBinaryPath)
			if err != nil {
				return err
			}
			return winner.Heartbeat(loaded.Journal().Phase, time.Now().Add(time.Minute))
		},
	}
	defer func() {
		if winner != nil {
			require.NoError(t, winner.Release())
		}
	}()

	err := gate.Check(context.Background(), home)
	require.Error(t, err)
	require.True(t, errors.As(err, new(*UpgradeInProgressError)))
	require.Equal(t, 1, wakeCalls)
	// A second entrant sees the winner's flock and never launches again.
	err = gate.Check(context.Background(), home)
	require.Error(t, err)
	require.Equal(t, 1, wakeCalls)
}

func TestEntrypointGateDoesNotRestartTerminalRollbackFailure(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Advance(PhaseDaemonStopping))
	require.NoError(t, lease.Advance(PhaseDaemonStopped))
	require.NoError(t, lease.Rollback())
	require.NoError(t, lease.MarkRollbackFailed())
	require.NoError(t, lease.Release())
	wakeCalls := 0
	gate := EntrypointGate{
		WakeRecovery: func(context.Context, *Transaction) error {
			wakeCalls++
			return nil
		},
	}

	err = gate.Check(context.Background(), home)
	var blocked *UpgradeRecoveryBlockedError
	require.ErrorAs(t, err, &blocked)
	require.ErrorContains(t, err, "rollback_failed")
	require.ErrorContains(t, err, "af doctor --fix")
	require.Zero(t, wakeCalls, "the terminal circuit breaker must stop automatic recovery")
}

func TestEntrypointGateFailsClosedWhenWakeDoesNotTakeTheLock(t *testing.T) {
	_, home, _ := prepareFixture(t)
	gate := EntrypointGate{
		TakeoverTimeout: time.Nanosecond,
		WakeRecovery:    func(context.Context, *Transaction) error { return nil },
	}

	err := gate.Check(context.Background(), home)
	var blocked *UpgradeRecoveryBlockedError
	require.ErrorAs(t, err, &blocked)
	require.ErrorContains(t, err, "did not acquire the recovery lock")
}
