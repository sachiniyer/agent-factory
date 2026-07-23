package upgradetxn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	defaultEntrypointTakeoverTimeout = 5 * time.Second
	defaultEntrypointPollInterval    = 50 * time.Millisecond
)

// UpgradeInProgressError is the typed result for a kernel-live recovery
// actor. Normal entrypoints must not start a daemon over this transaction.
type UpgradeInProgressError struct {
	TransactionID string
	ToVersion     string
	Phase         Phase
	Deadline      time.Time
}

func (e *UpgradeInProgressError) Error() string {
	if e.Deadline.IsZero() {
		return fmt.Sprintf(
			"daemon upgrade to %s is in progress (transaction %s, phase %s; recovery actor is starting)",
			e.ToVersion, e.TransactionID, e.Phase,
		)
	}
	return fmt.Sprintf(
		"daemon upgrade to %s is in progress (transaction %s, phase %s, recovery deadline %s)",
		e.ToVersion, e.TransactionID, e.Phase, e.Deadline.Format(time.RFC3339Nano),
	)
}

// UpgradeRecoveryBlockedError is a hard fail-closed result. It prevents an
// ordinary daemon from starting when recovery identity, takeover, or the
// rollback circuit breaker cannot be proven safe.
type UpgradeRecoveryBlockedError struct {
	TransactionID string
	ToVersion     string
	Phase         Phase
	Reason        string
}

func (e *UpgradeRecoveryBlockedError) Error() string {
	return fmt.Sprintf(
		"daemon upgrade recovery for %s is blocked (transaction %s, phase %s): %s; "+
			"no daemon was started; run af doctor --fix",
		e.ToVersion, e.TransactionID, e.Phase, e.Reason,
	)
}

// EntrypointGate is the all-entrypoint decision boundary for an active
// transaction. It never acquires recovery authority itself. If the flock is
// free, it wakes the immutable previous binary and waits only for that actor
// to take the flock or finish cleanup.
type EntrypointGate struct {
	WakeRecovery    func(context.Context, *Transaction) error
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
	TakeoverTimeout time.Duration
	PollInterval    time.Duration
}

// Check returns nil only when no active transaction remains. Every other
// result forbids normal command/daemon startup until the previous-binary actor
// commits, rolls back, or cleans up the journal.
func (g EntrypointGate) Check(ctx context.Context, homeDir string) error {
	if _, err := os.Stat(homeDir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect AF home before upgrade entrypoint gate: %w", err)
	}
	txn, err := Load(homeDir)
	if errors.Is(err, ErrNoActiveTransaction) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect active daemon upgrade before entrypoint startup: %w", err)
	}
	journal := txn.Journal()
	if journal.Phase == PhaseRollbackFailed {
		return blockedRecovery(journal, "automatic rollback reached the rollback_failed circuit breaker")
	}

	live, err := txn.RecoveryActorLive()
	if err != nil {
		return blockedRecovery(journal, fmt.Sprintf("cannot prove recovery actor liveness: %v", err))
	}
	if live {
		return g.liveRecoveryResult(journal)
	}

	wake := g.WakeRecovery
	if wake == nil {
		controller := RecoveryJobController{}
		wake = controller.Wake
	}
	if err := wake(ctx, txn); err != nil && !errors.Is(err, ErrRecoveryActive) {
		return blockedRecovery(journal, fmt.Sprintf("could not wake the preserved previous binary: %v", err))
	}

	timeout := g.TakeoverTimeout
	if timeout <= 0 {
		timeout = defaultEntrypointTakeoverTimeout
	}
	deadline := g.now().Add(timeout)
	for {
		current, loadErr := Load(homeDir)
		if errors.Is(loadErr, ErrNoActiveTransaction) {
			return nil
		}
		if loadErr != nil {
			return blockedRecovery(journal, fmt.Sprintf("cannot reload recovery journal after wake: %v", loadErr))
		}
		currentJournal := current.Journal()
		if currentJournal.ID != journal.ID {
			return blockedRecovery(currentJournal, "active transaction changed during recovery takeover")
		}
		if currentJournal.Phase == PhaseRollbackFailed {
			return blockedRecovery(currentJournal,
				"automatic rollback reached the rollback_failed circuit breaker")
		}
		live, liveErr := current.RecoveryActorLive()
		if liveErr != nil {
			return blockedRecovery(currentJournal,
				fmt.Sprintf("cannot prove recovery actor liveness after wake: %v", liveErr))
		}
		if live {
			return g.liveRecoveryResult(currentJournal)
		}
		if !g.now().Before(deadline) {
			return blockedRecovery(currentJournal,
				"the preserved previous binary did not acquire the recovery lock")
		}
		if err := g.sleep(ctx, g.pollInterval()); err != nil {
			return err
		}
	}
}

func (g EntrypointGate) liveRecoveryResult(journal Journal) error {
	status, err := ReadRecoveryStatus(journal.HomeDir)
	if err != nil {
		if errors.Is(err, ErrNoActiveTransaction) {
			return nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return &UpgradeInProgressError{
				TransactionID: journal.ID,
				ToVersion:     journal.ToVersion,
				Phase:         journal.Phase,
			}
		}
		return blockedRecovery(journal,
			fmt.Sprintf("a live recovery actor has an invalid identity record: %v", err))
	}
	if status.TransactionID != journal.ID || status.Nonce != journal.RecoveryNonce {
		return blockedRecovery(journal,
			"the live recovery identity changed while the entrypoint was checking it")
	}
	if !g.now().Before(status.Deadline) {
		return blockedRecovery(journal, fmt.Sprintf(
			"the recovery deadline %s expired but its actor still owns the recovery lock",
			status.Deadline.Format(time.RFC3339Nano),
		))
	}
	return &UpgradeInProgressError{
		TransactionID: journal.ID,
		ToVersion:     journal.ToVersion,
		Phase:         status.Phase,
		Deadline:      status.Deadline,
	}
}

func blockedRecovery(journal Journal, reason string) error {
	return &UpgradeRecoveryBlockedError{
		TransactionID: journal.ID,
		ToVersion:     journal.ToVersion,
		Phase:         journal.Phase,
		Reason:        reason,
	}
}

func (g EntrypointGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now().UTC()
}

func (g EntrypointGate) pollInterval() time.Duration {
	if g.PollInterval > 0 {
		return g.PollInterval
	}
	return defaultEntrypointPollInterval
}

func (g EntrypointGate) sleep(ctx context.Context, duration time.Duration) error {
	if g.Sleep != nil {
		return g.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
