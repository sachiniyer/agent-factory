package upgradetxn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// RecoveryInvocation is the strict internal command rendered into persistent
// recovery jobs. HomeDir and TransactionID are both checked against the active
// journal before the process attempts to acquire recovery authority.
type RecoveryInvocation struct {
	HomeDir       string
	TransactionID string
}

// ParseRecoveryInvocation recognizes only the exact argument vector emitted
// by recoveryCommand. matched is false for every ordinary af invocation, so a
// caller can perform this check before Cobra or config startup without
// reinterpreting public commands.
func ParseRecoveryInvocation(args []string) (invocation RecoveryInvocation, matched bool, err error) {
	if len(args) == 0 || args[0] != recoveryModeArgument {
		return RecoveryInvocation{}, false, nil
	}
	if len(args) != 5 || args[1] != "--home" || args[3] != "--transaction" {
		return RecoveryInvocation{}, true, errors.New("invalid internal upgrade recovery arguments")
	}
	home := args[2]
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return RecoveryInvocation{}, true, errors.New("internal upgrade recovery home must be absolute and canonical")
	}
	if err := validateTransactionID(args[4]); err != nil {
		return RecoveryInvocation{}, true, err
	}
	return RecoveryInvocation{HomeDir: home, TransactionID: args[4]}, true, nil
}

// RunRecoveryActor loads the named transaction, obtains recovery authority as
// os.Executable (which TryAcquireRecovery requires to be the immutable
// previous-binary artifact), and runs the supervisor. A duplicate launcher
// exits successfully after losing the flock so Restart=on-failure does not
// turn an ordinary service-manager race into a process storm.
func RunRecoveryActor(ctx context.Context, invocation RecoveryInvocation, supervisor Supervisor) error {
	return runRecoveryActorWith(
		ctx,
		invocation,
		func(txn *Transaction) (*RecoveryLease, error) { return txn.TryAcquireRecovery() },
		supervisor.Run,
	)
}

func runRecoveryActorWith(
	ctx context.Context,
	invocation RecoveryInvocation,
	acquire func(*Transaction) (*RecoveryLease, error),
	supervise func(context.Context, *Transaction, *RecoveryLease) error,
) (retErr error) {
	if acquire == nil || supervise == nil {
		return errors.New("upgrade recovery actor requires acquisition and supervision operations")
	}
	txn, err := Load(invocation.HomeDir)
	if errors.Is(err, ErrNoActiveTransaction) {
		// A disabled job may receive one final runtime restart after cleanup
		// removed active.json. There is no recovery authority left; exit 0 so
		// Restart=on-failure cannot turn that harmless tail into a loop.
		return nil
	}
	if err != nil {
		return err
	}
	journal := txn.Journal()
	if journal.ID != invocation.TransactionID {
		// A stale transaction-named job has no authority over the newer active
		// transaction and must stand down cleanly instead of restart-looping.
		return nil
	}
	lease, err := acquire(txn)
	if errors.Is(err, ErrRecoveryActive) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease == nil {
		return errors.New("upgrade recovery acquisition returned no lease")
	}
	defer func() { retErr = errors.Join(retErr, lease.Release()) }()

	err = supervise(ctx, txn, lease)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRecoveryJobDisableFailed) {
		return err
	}
	if errors.Is(err, ErrUpgradeAborted) ||
		errors.Is(err, ErrUpgradeRolledBack) ||
		errors.Is(err, ErrRollbackRecoveryFailed) {
		// Each expected terminal result is returned by Supervisor only after
		// the persistent job was disarmed. Exit 0 so the loaded unit's runtime
		// Restart policy cannot undo that circuit breaker.
		return nil
	}
	return err
}
