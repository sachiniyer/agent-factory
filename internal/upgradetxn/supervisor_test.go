package upgradetxn

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeSupervisorRuntime struct {
	running             string
	candidateValid      bool
	startCandidateErr   error
	startPreviousErr    error
	validatePreviousErr error
	disableJobErr       error
	approved            bool
	jobDisabled         bool
	calls               []string
}

func (f *fakeSupervisorRuntime) operations() SupervisorOperations {
	return SupervisorOperations{
		AwaitActivation: func(_ context.Context, journal Journal) error {
			f.calls = append(f.calls, "await-activation")
			txn, err := Load(journal.HomeDir)
			if err != nil {
				return err
			}
			return txn.AuthorizeActivation(journal.ID, journal.RecoveryNonce)
		},
		StopPrevious: func(context.Context, Journal) (StopOutcome, error) {
			f.calls = append(f.calls, "stop-previous")
			if f.running != "previous" && f.running != "" {
				return StopUnknown, errors.New("a non-previous daemon is running")
			}
			f.running = ""
			return StopConfirmed, nil
		},
		StartCandidate: func(context.Context, Journal) error {
			f.calls = append(f.calls, "start-candidate")
			f.running = "candidate"
			return f.startCandidateErr
		},
		ValidateCandidate: func(context.Context, Journal) error {
			f.calls = append(f.calls, "validate-candidate")
			if f.running != "candidate" || !f.candidateValid {
				return errors.New("candidate did not become healthy")
			}
			return nil
		},
		ApproveCandidate: func(context.Context, Journal) error {
			f.calls = append(f.calls, "approve-candidate")
			if f.running != "candidate" {
				return errors.New("candidate is absent")
			}
			f.approved = true
			return nil
		},
		StopCandidate: func(context.Context, Journal) (StopOutcome, error) {
			f.calls = append(f.calls, "stop-candidate")
			if f.running != "candidate" && f.running != "" {
				return StopUnknown, errors.New("refusing to stop a daemon from another transaction")
			}
			f.running = ""
			return StopConfirmed, nil
		},
		StartPrevious: func(context.Context, Journal) error {
			f.calls = append(f.calls, "start-previous")
			if f.startPreviousErr != nil {
				return f.startPreviousErr
			}
			f.running = "previous"
			return nil
		},
		ValidatePrevious: func(context.Context, Journal) error {
			f.calls = append(f.calls, "validate-previous")
			if f.validatePreviousErr != nil {
				return f.validatePreviousErr
			}
			if f.running != "previous" {
				return errors.New("previous daemon is not healthy")
			}
			return nil
		},
		DisableRecoveryJob: func(context.Context, Journal) error {
			f.calls = append(f.calls, "disable-recovery-job")
			f.jobDisabled = true
			return f.disableJobErr
		},
	}
}

func TestPreviousBinarySupervisorCommitsOnlyAfterCandidateValidation(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}

	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
	require.NoError(t, err)
	require.NoError(t, lease.Release())
	require.True(t, runtime.approved)
	require.True(t, runtime.jobDisabled)
	require.Equal(t, "candidate", runtime.running)
	require.Equal(t, []string{
		"await-activation",
		"stop-previous",
		"start-candidate",
		"validate-candidate",
		"approve-candidate",
		"disable-recovery-job",
	}, runtime.calls)

	_, err = Load(home)
	require.ErrorIs(t, err, ErrNoActiveTransaction)
	installed, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "candidate-binary", string(installed))
}

func TestPreviousBinarySupervisorRollsBackAndVerifiesPreviousDaemon(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}

	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrUpgradeRolledBack)
	require.NoError(t, lease.Release())
	require.False(t, runtime.approved)
	require.True(t, runtime.jobDisabled)
	require.Equal(t, "previous", runtime.running)
	require.Equal(t, []string{
		"await-activation",
		"stop-previous",
		"start-candidate",
		"validate-candidate",
		"stop-candidate",
		"start-previous",
		"validate-previous",
		"disable-recovery-job",
	}, runtime.calls)

	_, loadErr := Load(home)
	require.ErrorIs(t, loadErr, ErrNoActiveTransaction)
	installed, readErr := os.ReadFile(executable)
	require.NoError(t, readErr)
	require.Equal(t, "known-running-binary", string(installed))
}

func TestSupervisorAbortsWithoutRestoringOverAConfirmedLivePreviousDaemon(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
	operations := runtime.operations()
	operations.StopPrevious = func(context.Context, Journal) (StopOutcome, error) {
		runtime.calls = append(runtime.calls, "stop-previous")
		return StopStillRunning, errors.New("previous daemon refused graceful shutdown")
	}

	err = (Supervisor{Operations: operations}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrUpgradeAborted)
	require.NoError(t, lease.Release())
	require.True(t, runtime.jobDisabled)
	require.Equal(t, "previous", runtime.running)
	require.Equal(t, []string{"await-activation", "stop-previous", "disable-recovery-job"}, runtime.calls)

	_, loadErr := Load(home)
	require.ErrorIs(t, loadErr, ErrNoActiveTransaction)
	installed, readErr := os.ReadFile(executable)
	require.NoError(t, readErr)
	require.Equal(t, "known-running-binary", string(installed))
}

func TestSupervisorRefusesCallbackWithoutActorBoundApproval(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
	operations := runtime.operations()
	operations.AwaitActivation = func(context.Context, Journal) error {
		runtime.calls = append(runtime.calls, "await-activation")
		return nil
	}

	err = (Supervisor{Operations: operations}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrUpgradeAborted)
	require.ErrorIs(t, err, ErrActivationNotAuthorized)
	require.NoError(t, lease.Release())
	require.Equal(t, "previous", runtime.running)
	require.Equal(t, []string{"await-activation", "disable-recovery-job"}, runtime.calls,
		"a callback cannot bypass the actor-bound approval capability")

	_, err = Load(home)
	require.ErrorIs(t, err, ErrNoActiveTransaction)
	installed, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.Equal(t, "known-running-binary", string(installed))
}

func TestSupervisorLossAtEveryActivationBoundaryIsTakenOver(t *testing.T) {
	tests := []struct {
		boundary     Phase
		wantErr      error
		wantRunning  string
		wantBinary   string
		wantApproved bool
	}{
		{
			boundary:    PhaseSupervisorReady,
			wantErr:     ErrUpgradeAborted,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:    PhaseDaemonStopping,
			wantErr:     ErrUpgradeRolledBack,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:    PhaseDaemonStopped,
			wantErr:     ErrUpgradeRolledBack,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:    PhaseCandidateInstalled,
			wantErr:     ErrUpgradeRolledBack,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:    PhaseCandidateStarting,
			wantErr:     ErrUpgradeRolledBack,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:    PhaseCandidateValidating,
			wantErr:     ErrUpgradeRolledBack,
			wantRunning: "previous",
			wantBinary:  "known-running-binary",
		},
		{
			boundary:     PhaseCommitted,
			wantRunning:  "candidate",
			wantBinary:   "candidate-binary",
			wantApproved: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.boundary), func(t *testing.T) {
			txn, home, executable := prepareFixture(t)
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
			injected := false
			supervisor := Supervisor{
				Operations: runtime.operations(),
				AfterBoundary: func(got Phase) error {
					if !injected && got == test.boundary {
						injected = true
						return errors.New("simulated actor loss")
					}
					return nil
				},
			}

			err = supervisor.Run(context.Background(), txn, lease)
			require.ErrorIs(t, err, ErrSupervisorInterrupted)
			require.True(t, injected, "fault injection must actually execute")
			require.Equal(t, test.boundary, txn.Journal().Phase)
			if test.boundary == PhaseDaemonStopping {
				require.Equal(t, "previous", runtime.running,
					"stop intent must be durable before the destructive stop call")
				require.Equal(t, []string{"await-activation"}, runtime.calls)
			}
			require.NoError(t, lease.Release())

			resumed, err := Load(home)
			require.NoError(t, err)
			takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), resumed, takeover)
			if test.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.wantErr)
			}
			require.NoError(t, takeover.Release())
			require.Equal(t, test.wantApproved, runtime.approved)
			require.Equal(t, test.wantRunning, runtime.running)

			_, err = Load(home)
			require.ErrorIs(t, err, ErrNoActiveTransaction)
			installed, err := os.ReadFile(executable)
			require.NoError(t, err)
			require.Equal(t, test.wantBinary, string(installed))
		})
	}
}

func TestSupervisorLossAtEveryRollbackBoundaryIsTakenOver(t *testing.T) {
	boundaries := []Phase{
		PhaseRollbackRestored,
		PhasePreviousStarting,
		PhasePreviousValidating,
		PhaseRolledBack,
	}
	for _, boundary := range boundaries {
		t.Run(string(boundary), func(t *testing.T) {
			txn, home, executable := prepareFixture(t)
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}
			injected := false
			supervisor := Supervisor{
				Operations: runtime.operations(),
				AfterBoundary: func(got Phase) error {
					if !injected && got == boundary {
						injected = true
						return errors.New("simulated actor loss")
					}
					return nil
				},
			}

			err = supervisor.Run(context.Background(), txn, lease)
			require.ErrorIs(t, err, ErrSupervisorInterrupted)
			require.True(t, injected, "fault injection must actually execute")
			require.Equal(t, boundary, txn.Journal().Phase)
			require.NoError(t, lease.Release())

			resumed, err := Load(home)
			require.NoError(t, err)
			takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), resumed, takeover)
			require.ErrorIs(t, err, ErrUpgradeRolledBack)
			require.NoError(t, takeover.Release())
			require.False(t, runtime.approved)
			require.Equal(t, "previous", runtime.running)

			_, err = Load(home)
			require.ErrorIs(t, err, ErrNoActiveTransaction)
			installed, err := os.ReadFile(executable)
			require.NoError(t, err)
			require.Equal(t, "known-running-binary", string(installed))
		})
	}
}

func TestCommittedTakeoverRestartsAndRevalidatesCandidateBeforeCleanup(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: true}
	supervisor := Supervisor{
		Operations: runtime.operations(),
		AfterBoundary: func(phase Phase) error {
			if phase == PhaseCommitted {
				return errors.New("simulated actor loss")
			}
			return nil
		},
	}

	err = supervisor.Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrSupervisorInterrupted)
	require.NoError(t, lease.Release())
	// The committed candidate can die in the same crash that killed its
	// supervisor. A takeover must restore service before discarding rollback
	// artifacts, not assume the old validation is still a liveness verdict.
	runtime.running = ""

	resumed, err := Load(home)
	require.NoError(t, err)
	takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, (Supervisor{Operations: runtime.operations()}).Run(context.Background(), resumed, takeover))
	require.NoError(t, takeover.Release())
	require.Equal(t, "candidate", runtime.running)
	require.Equal(t, []string{
		"await-activation",
		"stop-previous",
		"start-candidate",
		"validate-candidate",
		"start-candidate",
		"validate-candidate",
		"approve-candidate",
		"disable-recovery-job",
	}, runtime.calls)
}

func TestRolledBackTakeoverRestartsAndRevalidatesPreviousBeforeCleanup(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}
	supervisor := Supervisor{
		Operations: runtime.operations(),
		AfterBoundary: func(phase Phase) error {
			if phase == PhaseRolledBack {
				return errors.New("simulated actor loss")
			}
			return nil
		},
	}

	err = supervisor.Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrSupervisorInterrupted)
	require.NoError(t, lease.Release())
	runtime.running = ""

	resumed, err := Load(home)
	require.NoError(t, err)
	takeover, err := resumed.tryAcquireRecoveryAs(resumed.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), resumed, takeover)
	require.ErrorIs(t, err, ErrUpgradeRolledBack)
	require.NoError(t, takeover.Release())
	require.Equal(t, "previous", runtime.running)
	require.Equal(t, []string{
		"await-activation",
		"stop-previous",
		"start-candidate",
		"validate-candidate",
		"stop-candidate",
		"start-previous",
		"validate-previous",
		"start-previous",
		"validate-previous",
		"disable-recovery-job",
	}, runtime.calls)
}

func TestRollbackFailedDisablesRecoveryLoopAndRetainsArtifacts(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	txn.mu.Lock()
	require.NoError(t, txn.persistPhaseLocked(PhaseRollbackFailed))
	txn.mu.Unlock()
	runtime := &fakeSupervisorRuntime{}

	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrRollbackRecoveryFailed)
	require.True(t, runtime.jobDisabled)
	require.Equal(t, []string{"disable-recovery-job"}, runtime.calls)
	require.NoError(t, lease.Release())

	retained, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseRollbackFailed, retained.Journal().Phase)
	require.FileExists(t, retained.Journal().PreviousBinaryPath)
	require.FileExists(t, retained.Journal().CandidatePath)
}

func TestPreviousRecoveryFailureIsTerminalInsteadOfRestartLooping(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeSupervisorRuntime)
	}{
		{
			name: "previous start",
			configure: func(runtime *fakeSupervisorRuntime) {
				runtime.startPreviousErr = errors.New("service manager unavailable")
			},
		},
		{
			name: "previous validation",
			configure: func(runtime *fakeSupervisorRuntime) {
				runtime.validatePreviousErr = errors.New("previous daemon never became healthy")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			txn, home, _ := prepareFixture(t)
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}
			test.configure(runtime)

			err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
			require.ErrorIs(t, err, ErrRollbackRecoveryFailed)
			require.True(t, runtime.jobDisabled,
				"the persistent recovery job must be disabled before the actor exits")
			require.NoError(t, lease.Release())

			retained, loadErr := Load(home)
			require.NoError(t, loadErr)
			require.Equal(t, PhaseRollbackFailed, retained.Journal().Phase)
			require.FileExists(t, retained.Journal().PreviousBinaryPath)
			require.FileExists(t, retained.Journal().CandidatePath)
		})
	}
}

func TestRollbackRestorationFailureDisablesJobWithoutOneMoreActorRestart(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	// Corrupt the immutable input only after the lease has proved this actor's
	// identity. Candidate installation and the ensuing rollback must both fail
	// closed, persist rollback_failed, and disable the persistent job now.
	require.NoError(t, os.WriteFile(txn.Journal().PreviousBinaryPath, []byte("corrupt"), 0o755))
	runtime := &fakeSupervisorRuntime{running: "previous", candidateValid: false}

	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrRollbackRecoveryFailed)
	require.True(t, runtime.jobDisabled,
		"a known terminal rollback failure must not require another service-manager restart to disable the job")
	require.NoError(t, lease.Release())

	retained, loadErr := Load(home)
	require.NoError(t, loadErr)
	require.Equal(t, PhaseRollbackFailed, retained.Journal().Phase)
	require.FileExists(t, retained.Journal().CandidatePath)
}

func TestRollbackFailedRemainsTerminalWhenJobDisableReportsAnError(t *testing.T) {
	txn, _, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	txn.mu.Lock()
	require.NoError(t, txn.persistPhaseLocked(PhaseRollbackFailed))
	txn.mu.Unlock()
	runtime := &fakeSupervisorRuntime{disableJobErr: errors.New("service manager unavailable")}

	err = (Supervisor{Operations: runtime.operations()}).Run(context.Background(), txn, lease)
	require.ErrorIs(t, err, ErrRollbackRecoveryFailed,
		"the recovery launcher must still recognize a terminal failure and avoid restart-looping")
	require.ErrorContains(t, err, "service manager unavailable")
	require.NoError(t, lease.Release())
}
