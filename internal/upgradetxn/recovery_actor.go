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

// RunRecoveryActor is the production entrypoint binding that runRecoveryActorWith
// deferred: it supplies the real recovery-authority acquisition
// ((*Transaction).TryAcquireRecovery, which derives identity from os.Executable)
// together with the caller's supervisor. main.go routes the internal
// __upgrade-recovery invocation here after ParseRecoveryInvocation, so the
// preserved previous binary — never a candidate — runs the recovery/rollback
// state machine. The supervisor's SupervisorOperations are injected by the
// daemon package, which cannot be imported here without a cycle.
//
// The returned Phase is the supervisor's terminal verdict (PhaseCommitted,
// PhaseRolledBack, PhaseAborted) preserved across the exit-0 nil-conversion the
// circuit breaker requires, so the daemon-layer caller can tell a real commit
// from a rollback/abort — three outcomes that all remove the journal and whose
// sentinels are all mapped to nil. It is empty ("") for every pre-supervise
// stand-down (no active transaction, a stale/foreign transaction, recovery
// already active): those never ran the supervisor, so there is no commit to
// hand off on.
func RunRecoveryActor(ctx context.Context, invocation RecoveryInvocation, supervisor Supervisor) (error, Phase) {
	return runRecoveryActorWith(ctx, invocation, (*Transaction).TryAcquireRecovery, supervisor.Run)
}

// runRecoveryActorWith is the runner core for the production entrypoint binding
// above. The binding supplies TryAcquireRecovery (which derives identity from
// os.Executable) and Supervisor.Run together.
//
// It returns the supervisor's terminal Phase alongside the exit-0 nil so the
// caller can distinguish the three outcomes the circuit breaker deliberately
// makes indistinguishable from the error alone: a real commit (nil + PhaseCommitted),
// a terminal rollback (nil + PhaseRolledBack), and a terminal abort (nil +
// PhaseAborted). All three remove the journal via lease.Cleanup(), so the
// "journal gone" signal the daemon layer previously gated on fired identically
// after all three. The phase is the durable verdict the supervisor persisted
// before Cleanup; it survives Cleanup in-memory (storage.go cleanup only
// removes files, it never mutates txn.journal.Phase), so it is a positive,
// outcome-specific signal. Every pre-supervise stand-down returns an empty
// phase: it never ran the supervisor, so there is no terminal verdict.
func runRecoveryActorWith(
	ctx context.Context,
	invocation RecoveryInvocation,
	acquire func(*Transaction) (*RecoveryLease, error),
	supervise func(context.Context, *Transaction, *RecoveryLease) error,
) (retErr error, terminalPhase Phase) {
	if acquire == nil || supervise == nil {
		return errors.New("upgrade recovery actor requires acquisition and supervision operations"), ""
	}
	txn, err := Load(invocation.HomeDir)
	if errors.Is(err, ErrNoActiveTransaction) {
		// A disabled job may receive one final runtime restart after cleanup
		// removed active.json. There is no recovery authority left; exit 0 so
		// Restart=on-failure cannot turn that harmless tail into a loop.
		return nil, ""
	}
	if err != nil {
		return err, ""
	}
	journal := txn.Journal()
	if journal.ID != invocation.TransactionID {
		// A stale transaction-named job has no authority over the newer active
		// transaction and must stand down cleanly instead of restart-looping.
		return nil, ""
	}
	lease, err := acquire(txn)
	if errors.Is(err, ErrRecoveryActive) {
		return nil, ""
	}
	if err != nil {
		return err, ""
	}
	if lease == nil {
		return errors.New("upgrade recovery acquisition returned no lease"), ""
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
	// Capture the supervisor's terminal verdict AFTER supervise. The in-memory
	// txn.journal.Phase is whatever the supervisor last persisted before it
	// returned (PhaseCommitted via lease.Commit, PhaseRolledBack via
	// lease.Advance, PhaseAborted via lease.Abort). cleanup() — which all three
	// terminal arms call before returning — only removes the on-disk journal and
	// artifacts; it never mutates txn.journal.Phase (storage.go cleanup), so the
	// phase survives the nil-conversion below as the one signal that still
	// distinguishes the three outcomes the circuit breaker erases from the error.
	terminalPhase = txn.Journal().Phase
	if err == nil {
		return nil, terminalPhase
	}
	if errors.Is(err, ErrRecoveryJobDisableFailed) {
		return err, terminalPhase
	}
	// Checked BEFORE the terminal list, because a terminal-looking sentinel is not
	// a statement that the work finished. A phase that ended before disarming the
	// persistent job still owes that work, and exiting 0 there is what stopped
	// systemd from retrying a transient ledger write until the next boot (#3098).
	if errors.Is(err, ErrRecoveryJobStillArmed) {
		return err, terminalPhase
	}
	if errors.Is(err, ErrUpgradeAborted) ||
		errors.Is(err, ErrUpgradeRolledBack) ||
		errors.Is(err, ErrRollbackRecoveryFailed) {
		// Each expected terminal result reaching HERE was returned after the
		// persistent job was disarmed — the still-armed cases are intercepted
		// above. Exit 0 so the loaded unit's runtime Restart policy cannot undo
		// that circuit breaker. The phase is returned alongside the nil so the
		// caller can tell a commit (PhaseCommitted) from a rollback/abort — the
		// three outcomes the nil-conversion deliberately makes indistinguishable
		// from the error alone, and which all remove the journal so "journal
		// gone" cannot tell them apart either.
		return nil, terminalPhase
	}
	return err, terminalPhase
}
