package upgradetxn

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrUpgradeRolledBack reports that candidate activation failed but the
	// previous binary, metadata, and daemon were restored and verified.
	ErrUpgradeRolledBack = errors.New("upgrade candidate was rolled back")
	// ErrUpgradeAborted reports that activation stopped before the previous
	// daemon exited; the installed binary and live metadata were untouched.
	ErrUpgradeAborted = errors.New("upgrade activation was aborted before daemon shutdown")
	// ErrActivationNotAuthorized means the previous daemon never acknowledged
	// the recovery actor's nonce handshake. The actor must abort without
	// touching live state.
	ErrActivationNotAuthorized = errors.New("upgrade supervisor handshake was not authorized")
	// ErrSupervisorInterrupted is returned only when a fault-injection hook
	// models actor death immediately after a durable boundary. Production
	// supervisors do not install such a hook.
	ErrSupervisorInterrupted = errors.New("upgrade supervisor interrupted after durable boundary")
	// ErrRollbackRecoveryFailed is terminal for unattended recovery. Artifacts
	// and the active journal remain for explicit repair while the persistent job
	// is disabled, so a corrupt snapshot cannot create a restart loop.
	ErrRollbackRecoveryFailed = errors.New("automatic upgrade rollback failed; recovery artifacts retained")

	errSupervisorInterruptedBeforeShutdown = errors.New("upgrade supervisor restarted before daemon shutdown")
	errSupervisorInterruptedBeforeCommit   = errors.New("upgrade supervisor restarted before candidate commit")
)

// StopOutcome distinguishes positive proof that the intended daemon stopped
// from proof it is still running and from an ambiguous observation. Only a
// confirmed stop authorizes a binary replacement or metadata restoration.
type StopOutcome uint8

const (
	StopUnknown StopOutcome = iota
	StopConfirmed
	StopStillRunning
)

// SupervisorOperations is the process/service-manager boundary around the
// durable state machine. AwaitActivation may only wait for the old daemon's
// approval; it must not quiesce or stop that daemon. StopPrevious is the first
// shutdown operation and runs only after daemon_stopping is durable.
// Implementations must scope stop operations to the exact version,
// transaction, boot, AF home, and captured owner in Journal; returning
// StopConfirmed is destructive authority.
type SupervisorOperations struct {
	AwaitActivation    func(context.Context, Journal) error
	StopPrevious       func(context.Context, Journal) (StopOutcome, error)
	StartCandidate     func(context.Context, Journal) error
	ValidateCandidate  func(context.Context, Journal) error
	ApproveCandidate   func(context.Context, Journal) error
	StopCandidate      func(context.Context, Journal) (StopOutcome, error)
	StartPrevious      func(context.Context, Journal) error
	ValidatePrevious   func(context.Context, Journal) error
	DisableRecoveryJob func(context.Context, Journal) error
}

// Supervisor is the previous-binary recovery engine. The RecoveryLease is
// the structural capability: Transaction.TryAcquireRecovery grants it only to
// the immutable previous-binary artifact, never to a candidate.
type Supervisor struct {
	Operations SupervisorOperations
	// AfterBoundary is a test-only actor-death injection. The boundary has
	// already been fsynced when it runs; returning an error must not trigger a
	// different recovery decision in the same actor.
	AfterBoundary func(Phase) error
	// PhaseDeadline is diagnostic heartbeat data used by the service manager's
	// hang watchdog. It never authorizes another process to take the flock.
	PhaseDeadline time.Duration
}

const defaultSupervisorPhaseDeadline = 5 * time.Minute

// Run resumes the journal's current phase until candidate commit+cleanup or a
// fully verified rollback. Every operation is idempotent because actor loss
// may repeat it after the last fsynced boundary.
func (s Supervisor) Run(ctx context.Context, txn *Transaction, lease *RecoveryLease) error {
	if txn == nil || lease == nil {
		return errors.New("upgrade supervisor requires a transaction and recovery lease")
	}
	if err := lease.validateSupervisorTransaction(txn); err != nil {
		return err
	}
	if err := s.validateOperations(); err != nil {
		return err
	}
	deadline := s.PhaseDeadline
	if deadline <= 0 {
		deadline = defaultSupervisorPhaseDeadline
	}
	var rollbackCause error
	var abortCause error
	var candidateValidatedThisRun bool
	var previousStartedThisRun bool
	var previousValidatedThisRun bool
	var takeoverFromStopIntent bool
	firstIteration := true
	if txn.Journal().Phase == PhaseSupervisorReady {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A new actor cannot inherit the prior actor's authorization
		// assumptions. Abort before publishing any readiness proof that the old
		// daemon could mistake for permission to begin shutdown.
		abortCause = errSupervisorInterruptedBeforeShutdown
		if err := lease.Abort(); err != nil {
			return errors.Join(abortCause, err)
		}
		if err := s.afterBoundary(PhaseAborted); err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		journal := txn.Journal()
		if err := lease.Heartbeat(journal.Phase, time.Now().Add(deadline)); err != nil {
			return err
		}
		if firstIteration {
			firstIteration = false
			switch journal.Phase {
			case PhaseDaemonStopping:
				// The prior actor durably recorded its authorized stop intent,
				// but may have died on either side of the stop operation. Repeat
				// the identity-scoped stop to resolve that ambiguity, then roll
				// back rather than letting a takeover continue the candidate.
				takeoverFromStopIntent = true
			case PhaseDaemonStopped, PhaseCandidateInstalled,
				PhaseCandidateStarting, PhaseCandidateValidating:
				// Before commit, actor loss is a durable negative verdict. A
				// takeover must not let the candidate re-run its own health exam.
				rollbackCause = errSupervisorInterruptedBeforeCommit
				if err := s.stopCandidateAndRestore(ctx, txn, lease); err != nil {
					return s.finishTerminalRollbackFailure(
						ctx, txn, lease, errors.Join(rollbackCause, err),
					)
				}
				if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
					return err
				}
				continue
			}
		}

		switch journal.Phase {
		case PhasePrepared:
			if err := lease.Advance(PhaseSupervisorReady); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseSupervisorReady); err != nil {
				return err
			}

		case PhaseSupervisorReady:
			activationErr := s.Operations.AwaitActivation(ctx, journal)
			if activationErr == nil {
				authorized, err := lease.ActivationAuthorized()
				if err != nil {
					return fmt.Errorf("validate upgrade activation handshake: %w", err)
				}
				if !authorized {
					activationErr = ErrActivationNotAuthorized
				}
			}
			if activationErr != nil {
				if !errors.Is(activationErr, ErrActivationNotAuthorized) {
					return fmt.Errorf("wait for upgrade activation handshake: %w", activationErr)
				}
				abortCause = activationErr
				if abortErr := lease.Abort(); abortErr != nil {
					return errors.Join(activationErr, abortErr)
				}
				if boundaryErr := s.afterBoundary(PhaseAborted); boundaryErr != nil {
					return boundaryErr
				}
				continue
			}
			if err := lease.Advance(PhaseDaemonStopping); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseDaemonStopping); err != nil {
				return err
			}

		case PhaseDaemonStopping:
			outcome, err := s.Operations.StopPrevious(ctx, journal)
			switch outcome {
			case StopConfirmed:
				// Positive stop proof is authoritative even if the RPC response
				// carried a diagnostic error after shutdown completed.
			case StopStillRunning:
				abortCause = err
				if abortErr := lease.Abort(); abortErr != nil {
					return errors.Join(err, abortErr)
				}
				if boundaryErr := s.afterBoundary(PhaseAborted); boundaryErr != nil {
					return boundaryErr
				}
				continue
			default:
				if err != nil {
					return fmt.Errorf("stop previous daemon: %w", err)
				}
				return fmt.Errorf("stop previous daemon was not confirmed (outcome %d)", outcome)
			}
			if err := lease.Advance(PhaseDaemonStopped); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseDaemonStopped); err != nil {
				return err
			}
			if takeoverFromStopIntent {
				rollbackCause = errSupervisorInterruptedBeforeCommit
				if err := s.stopCandidateAndRestore(ctx, txn, lease); err != nil {
					return s.finishTerminalRollbackFailure(
						ctx, txn, lease, errors.Join(rollbackCause, err),
					)
				}
				if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
					return err
				}
				continue
			}

		case PhaseDaemonStopped:
			if err := lease.InstallCandidate(); err != nil {
				rollbackCause = err
				if restoreErr := s.stopCandidateAndRestore(ctx, txn, lease); restoreErr != nil {
					return s.finishTerminalRollbackFailure(
						ctx, txn, lease, errors.Join(rollbackCause, restoreErr),
					)
				}
				if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
					return err
				}
				continue
			}
			if err := s.afterBoundary(PhaseCandidateInstalled); err != nil {
				return err
			}

		case PhaseCandidateInstalled:
			if err := lease.Advance(PhaseCandidateStarting); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseCandidateStarting); err != nil {
				return err
			}

		case PhaseCandidateStarting:
			if err := s.Operations.StartCandidate(ctx, journal); err != nil {
				rollbackCause = err
				if restoreErr := s.stopCandidateAndRestore(ctx, txn, lease); restoreErr != nil {
					return s.finishTerminalRollbackFailure(
						ctx, txn, lease, errors.Join(rollbackCause, restoreErr),
					)
				}
				if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
					return err
				}
				continue
			}
			if err := lease.Advance(PhaseCandidateValidating); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseCandidateValidating); err != nil {
				return err
			}

		case PhaseCandidateValidating:
			if err := s.Operations.ValidateCandidate(ctx, journal); err != nil {
				rollbackCause = err
				if restoreErr := s.stopCandidateAndRestore(ctx, txn, lease); restoreErr != nil {
					return s.finishTerminalRollbackFailure(
						ctx, txn, lease, errors.Join(rollbackCause, restoreErr),
					)
				}
				if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
					return err
				}
				continue
			}
			candidateValidatedThisRun = true
			if err := lease.Commit(); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseCommitted); err != nil {
				return err
			}

		case PhaseCommitted:
			// Commit is the irreversible verdict. A crash before approval or
			// cleanup retries those steps; it can never reinterpret success as a
			// rollback. Re-establish and revalidate the candidate first because
			// the daemon can have died with the prior supervisor after the commit
			// boundary. StartCandidate must be idempotent for an already-live
			// service; cleanup cannot discard recovery until service is live now.
			if !candidateValidatedThisRun {
				if err := s.Operations.StartCandidate(ctx, journal); err != nil {
					return fmt.Errorf("restart committed candidate: %w", err)
				}
				if err := s.Operations.ValidateCandidate(ctx, journal); err != nil {
					return fmt.Errorf("revalidate committed candidate: %w", err)
				}
			}
			if err := s.Operations.ApproveCandidate(ctx, journal); err != nil {
				return fmt.Errorf("approve committed candidate: %w", err)
			}
			if err := s.Operations.DisableRecoveryJob(ctx, journal); err != nil {
				return fmt.Errorf("disable committed recovery job: %w", err)
			}
			return lease.Cleanup()

		case PhaseAborted:
			if err := s.Operations.DisableRecoveryJob(ctx, journal); err != nil {
				return fmt.Errorf("disable aborted recovery job: %w", err)
			}
			if err := lease.Cleanup(); err != nil {
				return err
			}
			return errors.Join(ErrUpgradeAborted, abortCause)

		case PhaseRollingBack:
			if err := lease.Rollback(); err != nil {
				return s.finishTerminalRollbackFailure(ctx, txn, lease, err)
			}
			if err := s.afterBoundary(PhaseRollbackRestored); err != nil {
				return err
			}

		case PhaseRollbackFailed:
			if err := s.Operations.DisableRecoveryJob(ctx, journal); err != nil {
				return errors.Join(
					ErrRollbackRecoveryFailed,
					fmt.Errorf("disable failed rollback recovery job: %w", err),
				)
			}
			return ErrRollbackRecoveryFailed

		case PhaseRollbackRestored:
			if err := lease.Advance(PhasePreviousStarting); err != nil {
				return err
			}
			if err := s.afterBoundary(PhasePreviousStarting); err != nil {
				return err
			}

		case PhasePreviousStarting:
			if err := s.Operations.StartPrevious(ctx, journal); err != nil {
				return s.finishFailedRollback(
					ctx, txn, lease, fmt.Errorf("start previous daemon after rollback: %w", err),
				)
			}
			previousStartedThisRun = true
			if err := lease.Advance(PhasePreviousValidating); err != nil {
				return err
			}
			if err := s.afterBoundary(PhasePreviousValidating); err != nil {
				return err
			}

		case PhasePreviousValidating:
			// The previous daemon may have died with the actor after the
			// previous_validating boundary (especially across reboot). A
			// takeover must re-establish it before treating validation failure
			// as a terminal rollback failure.
			if !previousStartedThisRun {
				if err := s.Operations.StartPrevious(ctx, journal); err != nil {
					return s.finishFailedRollback(
						ctx, txn, lease, fmt.Errorf("restart previous daemon before validation: %w", err),
					)
				}
				previousStartedThisRun = true
			}
			if err := s.Operations.ValidatePrevious(ctx, journal); err != nil {
				return s.finishFailedRollback(
					ctx, txn, lease, fmt.Errorf("validate previous daemon after rollback: %w", err),
				)
			}
			previousValidatedThisRun = true
			if err := lease.Advance(PhaseRolledBack); err != nil {
				return err
			}
			if err := s.afterBoundary(PhaseRolledBack); err != nil {
				return err
			}

		case PhaseRolledBack:
			// The terminal rollback verdict proves the previous daemon was
			// healthy at the boundary, not that it survived the actor crash that
			// may have followed. Re-establish that invariant before cleanup.
			if !previousValidatedThisRun {
				if err := s.Operations.StartPrevious(ctx, journal); err != nil {
					return s.finishFailedRollback(
						ctx, txn, lease, fmt.Errorf("restart rolled-back previous daemon: %w", err),
					)
				}
				if err := s.Operations.ValidatePrevious(ctx, journal); err != nil {
					return s.finishFailedRollback(
						ctx, txn, lease, fmt.Errorf("revalidate rolled-back previous daemon: %w", err),
					)
				}
			}
			if err := s.Operations.DisableRecoveryJob(ctx, journal); err != nil {
				return fmt.Errorf("disable rolled-back recovery job: %w", err)
			}
			if err := lease.Cleanup(); err != nil {
				return err
			}
			if rollbackCause != nil {
				return fmt.Errorf("%w: %v", ErrUpgradeRolledBack, rollbackCause)
			}
			return ErrUpgradeRolledBack

		default:
			return fmt.Errorf("cannot supervise upgrade in phase %s", journal.Phase)
		}
	}
}

func (l *RecoveryLease) validateSupervisorTransaction(txn *Transaction) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.file == nil || l.txn == nil {
		return errors.New("upgrade recovery lease is released")
	}
	if l.txn != txn {
		return errors.New("upgrade recovery lease does not belong to the supplied transaction")
	}
	return nil
}

func (s Supervisor) finishTerminalRollbackFailure(
	ctx context.Context,
	txn *Transaction,
	lease *RecoveryLease,
	cause error,
) error {
	if txn.Journal().Phase != PhaseRollbackFailed {
		// A crash-injection checkpoint deliberately leaves rolling_back so a
		// takeover can resume. Only the durable rollback_failed phase is a
		// circuit-breaker that may disable the persistent actor.
		return cause
	}
	return s.finishFailedRollback(ctx, txn, lease, cause)
}

func (s Supervisor) finishFailedRollback(
	ctx context.Context,
	txn *Transaction,
	lease *RecoveryLease,
	cause error,
) error {
	if err := lease.MarkRollbackFailed(); err != nil {
		// Do not disable the persistent actor unless the terminal circuit-breaker
		// is durable. A later takeover must still have a chance to finish it.
		return errors.Join(cause, fmt.Errorf("persist rollback failure: %w", err))
	}
	if err := s.Operations.DisableRecoveryJob(ctx, txn.Journal()); err != nil {
		return errors.Join(ErrRollbackRecoveryFailed, cause, fmt.Errorf("disable failed rollback recovery job: %w", err))
	}
	return errors.Join(ErrRollbackRecoveryFailed, cause)
}

// MarkRollbackFailed closes the persistent recovery loop after the one exact
// previous-daemon recovery attempt fails. Artifacts remain for explicit repair.
func (l *RecoveryLease) MarkRollbackFailed() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.markRollbackFailed() })
}

// markRollbackFailed is the terminal circuit breaker after the previous
// binary was restored but its daemon could not be started or validated. A
// persistent recovery job must not retry that exact recovery forever.
func (t *Transaction) markRollbackFailed() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == PhaseRollbackFailed {
		return nil
	}
	switch t.journal.Phase {
	case PhaseRollbackRestored,
		PhasePreviousStarting,
		PhasePreviousValidating,
		PhaseRolledBack:
		return t.persistPhaseLocked(PhaseRollbackFailed)
	default:
		return fmt.Errorf("cannot mark rollback failed from phase %s", t.journal.Phase)
	}
}

func (s Supervisor) stopCandidateAndRestore(
	ctx context.Context,
	txn *Transaction,
	lease *RecoveryLease,
) error {
	journal := txn.Journal()
	outcome, err := s.Operations.StopCandidate(ctx, journal)
	switch outcome {
	case StopConfirmed:
		// Positive identity-scoped stop proof is authoritative even if the
		// transport reports a trailing diagnostic after shutdown completed.
	case StopStillRunning:
		if err != nil {
			return fmt.Errorf("stop failed candidate: %w", err)
		}
		return errors.New("failed candidate is still running")
	default:
		if err != nil {
			return fmt.Errorf("stop failed candidate: %w", err)
		}
		return fmt.Errorf("failed candidate stop was not confirmed (outcome %d)", outcome)
	}
	if err := lease.Rollback(); err != nil {
		return fmt.Errorf("restore previous binary and metadata: %w", err)
	}
	return nil
}

func (s Supervisor) afterBoundary(phase Phase) error {
	if s.AfterBoundary == nil {
		return nil
	}
	if err := s.AfterBoundary(phase); err != nil {
		return fmt.Errorf("%w at %s: %v", ErrSupervisorInterrupted, phase, err)
	}
	return nil
}

func (s Supervisor) validateOperations() error {
	missing := ""
	switch {
	case s.Operations.AwaitActivation == nil:
		missing = "AwaitActivation"
	case s.Operations.StopPrevious == nil:
		missing = "StopPrevious"
	case s.Operations.StartCandidate == nil:
		missing = "StartCandidate"
	case s.Operations.ValidateCandidate == nil:
		missing = "ValidateCandidate"
	case s.Operations.ApproveCandidate == nil:
		missing = "ApproveCandidate"
	case s.Operations.StopCandidate == nil:
		missing = "StopCandidate"
	case s.Operations.StartPrevious == nil:
		missing = "StartPrevious"
	case s.Operations.ValidatePrevious == nil:
		missing = "ValidatePrevious"
	case s.Operations.DisableRecoveryJob == nil:
		missing = "DisableRecoveryJob"
	}
	if missing != "" {
		return fmt.Errorf("upgrade supervisor operation %s is unavailable", missing)
	}
	return nil
}
