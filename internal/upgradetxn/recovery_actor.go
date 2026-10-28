package upgradetxn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
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

// RecoveryActorResult reports the outcome of a recovery run. Committed is the
// positive commit signal: it is true ONLY when the actor loaded OUR transaction
// (the journal.ID matched the invocation) AND Supervisor.Run returned nil, which
// happens exclusively on the PhaseCommitted path after lease.Cleanup durably
// removed the journal. Stand-downs that also return a nil error — a foreign/stale
// transaction, ErrRecoveryActive, an abort, a terminal rollback, or
// ErrNoActiveTransaction — report Committed=false, because no hand-off is owed.
//
// JournalID and ExecutablePath are captured from the SAME in-memory *Transaction
// the supervisor supervised, so a caller never needs a second, racy journal read
// to decide or parameterize the irreversible post-commit hand-off. A nil error
// does NOT imply Committed: the caller must gate on the signal, not on the error.
type RecoveryActorResult struct {
	Committed      bool
	JournalID      string
	ExecutablePath string
}

// RunRecoveryActor is the production entrypoint binding that runRecoveryActorWith
// deferred: it supplies the real recovery-authority acquisition
// ((*Transaction).TryAcquireRecovery, which derives identity from os.Executable)
// together with the caller's supervisor. main.go routes the internal
// __upgrade-recovery invocation here after ParseRecoveryInvocation, so the
// preserved previous binary — never a candidate — runs the recovery/rollback
// state machine. The supervisor's SupervisorOperations are injected by the
// daemon package, which cannot be imported here without a cycle. The returned
// RecoveryActorResult.Committed is the positive commit signal a caller uses to
// decide the irreversible post-commit hand-off; the error surfaces failures that
// must reach the process exit code.
func RunRecoveryActor(ctx context.Context, invocation RecoveryInvocation, supervisor Supervisor) (RecoveryActorResult, error) {
	return runRecoveryActorWith(ctx, invocation, (*Transaction).TryAcquireRecovery, supervisor.Run)
}

// runRecoveryActorWith is the runner core for the production entrypoint binding
// above. The binding supplies TryAcquireRecovery (which derives identity from
// os.Executable) and Supervisor.Run together. The returned RecoveryActorResult
// carries the positive commit signal derived from the supervisor's own
// already-loaded transaction, so a caller never has to re-derive commit by
// re-reading the journal (the non-durable reads that could silently strand a
// committed candidate on a transient I/O failure).
func runRecoveryActorWith(
	ctx context.Context,
	invocation RecoveryInvocation,
	acquire func(*Transaction) (*RecoveryLease, error),
	supervise func(context.Context, *Transaction, *RecoveryLease) error,
) (result RecoveryActorResult, retErr error) {
	if acquire == nil || supervise == nil {
		return RecoveryActorResult{}, errors.New("upgrade recovery actor requires acquisition and supervision operations")
	}
	txn, err := Load(invocation.HomeDir)
	if errors.Is(err, ErrNoActiveTransaction) {
		// A disabled job may receive one final runtime restart after cleanup
		// removed active.json. There is no recovery authority left; exit 0 so
		// Restart=on-failure cannot turn that harmless tail into a loop. Not a
		// commit: nothing was handed off before, nothing is owed now.
		return RecoveryActorResult{}, nil
	}
	if err != nil {
		return RecoveryActorResult{}, err
	}
	journal := txn.Journal()
	if journal.ID != invocation.TransactionID {
		// A stale transaction-named job has no authority over the newer active
		// transaction and must stand down cleanly instead of restart-looping.
		// Not a commit: it was never OUR transaction to hand off.
		return RecoveryActorResult{}, nil
	}
	lease, err := acquire(txn)
	if errors.Is(err, ErrRecoveryActive) {
		return RecoveryActorResult{}, nil
	}
	if err != nil {
		return RecoveryActorResult{}, err
	}
	if lease == nil {
		return RecoveryActorResult{}, errors.New("upgrade recovery acquisition returned no lease")
	}
	// Releasing the lease is CLEANUP, and cleanup that fails after the real work
	// succeeded must not be reported as the work failing. Joining it into retErr
	// turned a successful recovery into a non-zero exit — and this actor's exit
	// code is load-bearing: every terminal path below returns nil specifically so
	// the loaded unit's Restart=on-failure cannot undo the circuit breaker the
	// supervisor just set. A failed release would have restarted an actor for an
	// upgrade that had already committed or rolled back (#2960).
	//
	// The lease is an flock plus a file handle; both are released by the kernel
	// when this process exits moments later, so a release error costs nothing
	// beyond the diagnostic. It is logged rather than dropped so a genuinely
	// stuck lock is still visible.
	defer func() {
		if relErr := lease.Release(); relErr != nil {
			log.WarningLog.Printf("upgrade recovery: could not release the recovery lease for transaction %s (the recovery itself is unaffected): %v",
				invocation.TransactionID, relErr)
		}
	}()

	err = supervise(ctx, txn, lease)
	if err == nil {
		// POSITIVE COMMIT SIGNAL. Supervisor.Run returns nil ONLY on the
		// PhaseCommitted path, after lease.Cleanup durably removed the
		// journal. The identity proof is already encoded here: this branch is
		// reachable only because the authoritative Load above matched OUR
		// transaction (journal.ID == invocation.TransactionID), and the
		// supervisor then committed it. Capture the identity and executable
		// path from the SAME in-memory *Transaction the supervisor supervised
		// — not a second, non-durable journal read that a transient I/O
		// failure could turn into a silently stranded candidate. ID and
		// ExecutablePath are immutable from Prepare, so the committed snapshot
		// is authoritative even though Cleanup has removed the on-disk
		// journal: the in-memory copy the *Transaction holds is still valid.
		committed := txn.Journal()
		return RecoveryActorResult{
			Committed:      true,
			JournalID:      committed.ID,
			ExecutablePath: committed.ExecutablePath,
		}, nil
	}
	if errors.Is(err, ErrRecoveryJobDisableFailed) {
		return RecoveryActorResult{}, err
	}
	// Checked BEFORE the terminal list, because a terminal-looking sentinel is not
	// a statement that the work finished. A phase that ended before disarming the
	// persistent job still owes that work, and exiting 0 there is what stopped
	// systemd from retrying a transient ledger write until the next boot (#3098).
	if errors.Is(err, ErrRecoveryJobStillArmed) {
		return RecoveryActorResult{}, err
	}
	if errors.Is(err, ErrUpgradeAborted) ||
		errors.Is(err, ErrUpgradeRolledBack) ||
		errors.Is(err, ErrRollbackRecoveryFailed) {
		// Each expected terminal result reaching HERE was returned after the
		// persistent job was disarmed — the still-armed cases are intercepted
		// above. Exit 0 so the loaded unit's runtime Restart policy cannot undo
		// that circuit breaker. NOT a commit: an abort or a rollback leaves
		// nothing to hand off (a rollback restores the previous daemon under
		// its own owner; an abort touched nothing), so a caller must NOT arm a
		// post-upgrade daemon from this path.
		return RecoveryActorResult{}, nil
	}
	return RecoveryActorResult{}, err
}
