package upgradetxn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupervisorLossBeforeActivationHandshakeAbortsWithoutStoppingPrevious(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
	interrupted := false

	err = (Supervisor{
		Operations: runtime.operations(),
		AfterBoundary: func(phase Phase) error {
			if !interrupted && phase == PhaseSupervisorReady {
				interrupted = true
				return errors.New("simulated actor loss before activation handshake")
			}
			return nil
		},
	}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrSupervisorInterrupted)
	require.True(t, interrupted, "fault injection must actually execute")
	require.Equal(t, PhaseSupervisorReady, txn.Journal().Phase)
	require.Empty(t, runtime.calls, "the old daemon must remain untouched before authorization")
	require.NoError(t, lease.Release())

	resumed, err := Load(home)
	require.NoError(t, err)
	takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	operations := runtime.operations()
	operations.AwaitActivation = func(context.Context, Journal) error {
		runtime.calls = append(runtime.calls, "await-activation")
		return ErrActivationNotAuthorized
	}
	err = (Supervisor{Operations: operations}).Run(context.Background(), resumed, takeover)
	require.ErrorIs(t, err, ErrUpgradeAborted)
	require.ErrorIs(t, err, ErrActivationNotAuthorized)
	require.NoError(t, takeover.Release())
	require.Equal(t, "previous", runtime.running)
	require.False(t, runtime.approved)
	require.True(t, runtime.jobDisabled)
	require.Equal(t, []string{"await-activation", "disable-recovery-job"}, runtime.calls)

	_, err = Load(home)
	require.ErrorIs(t, err, ErrNoActiveTransaction)
	installed, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "known-running-binary", string(installed))
}

func TestSupervisorRejectsLeaseFromAnotherTransactionBeforeMutation(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	other, _, _ := prepareFixture(t)
	otherLease, err := other.tryAcquireRecoveryAs(other.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherLease.Release()) })
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}

	err = (Supervisor{
		Operations: runtime.operations(),
		AfterBoundary: func(Phase) error {
			return errors.New("mismatched lease advanced a phase")
		},
	}).Run(context.Background(), txn, otherLease)
	require.ErrorContains(t, err, "recovery lease does not belong to the supplied transaction")
	require.Equal(t, PhasePrepared, txn.Journal().Phase)
	require.Equal(t, PhasePrepared, other.Journal().Phase)
	require.Empty(t, runtime.calls)
}

func TestSupervisorLossAtEveryFileRollbackCheckpointIsTakenOver(t *testing.T) {
	checkpoints := []struct {
		name     string
		progress RollbackProgress
	}{
		{name: "rollback-started"},
		{name: "binary-restored", progress: RollbackProgress{BinaryRestored: true}},
		{name: "metadata-1", progress: RollbackProgress{BinaryRestored: true, MetadataRestored: 1}},
		{name: "metadata-2", progress: RollbackProgress{BinaryRestored: true, MetadataRestored: 2}},
		{name: "metadata-3", progress: RollbackProgress{BinaryRestored: true, MetadataRestored: 3}},
	}
	for _, checkpoint := range checkpoints {
		checkpoint := checkpoint
		t.Run(checkpoint.name, func(t *testing.T) {
			txn, home, executable := prepareFixture(t)
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}
			operations := runtime.operations()
			operations.ValidateCandidate = func(context.Context, Journal) error {
				runtime.calls = append(runtime.calls, "validate-candidate")
				require.NoError(t, os.WriteFile(filepath.Join(home, "state.json"), []byte("candidate-state"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(home, "tasks.json"), []byte("candidate-task"), 0o600))
				require.NoError(t, os.WriteFile(
					filepath.Join(home, "instances", "repo-a", "instances.json"),
					[]byte("candidate-instances"), 0o600))
				return errors.New("candidate failed health validation")
			}
			injected := false
			txn.afterRollbackCheckpoint = func(got RollbackProgress) error {
				if !injected && got == checkpoint.progress {
					injected = true
					return fmt.Errorf("simulated supervisor death: %w", errRecoveryCheckpointInterrupted)
				}
				return nil
			}

			err = (Supervisor{Operations: operations}).Run(context.Background(), txn, lease)
			require.ErrorIs(t, err, errRecoveryCheckpointInterrupted)
			require.True(t, injected, "fault injection must actually execute")
			require.Equal(t, PhaseRollingBack, txn.Journal().Phase)
			require.Equal(t, checkpoint.progress, txn.Journal().RollbackProgress)
			require.NoError(t, lease.Release())

			resumed, err := Load(home)
			require.NoError(t, err)
			takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), resumed, takeover)
			require.ErrorIs(t, err, ErrUpgradeRolledBack)
			require.NoError(t, takeover.Release())
			require.Equal(t, "previous", runtime.running)

			_, err = Load(home)
			require.ErrorIs(t, err, ErrNoActiveTransaction)
			installed, err := os.ReadFile(executable)
			require.NoError(t, err)
			require.Equal(t, "known-running-binary", string(installed))
			state, err := os.ReadFile(filepath.Join(home, "state.json"))
			require.NoError(t, err)
			require.Equal(t, "old-state", string(state))
			instances, err := os.ReadFile(filepath.Join(home, "instances", "repo-a", "instances.json"))
			require.NoError(t, err)
			require.Equal(t, "old-instances", string(instances))
			_, err = os.Stat(filepath.Join(home, "tasks.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
