package upgradetxn

import (
	"errors"
	"fmt"
	"path/filepath"
)

// AbortPreparedTransaction lets the INITIATING daemon (never a recovery actor) undo
// a transaction it published with Prepare but could not hand to an actor — e.g. when
// InstallAndStart or the supervisor_ready wait failed. Without it, a single transient
// service-manager failure strands active.json at PhasePrepared forever: every future
// Prepare fails "already active", and no actor exists to Abort it (a lease is granted
// only to the preserved previous binary, which the serving daemon is not).
//
// It is deliberately narrow so it can never race a live recovery: it serializes
// against Prepare on prepare.lock, acts ONLY while the journal is still PhasePrepared
// (any later phase means an actor took authority and owns teardown), and ONLY while no
// recovery actor holds the lock. Then it removes the authority marker and the inert
// artifacts (candidate, recovery unit, transaction directory, lock) through the exact
// path cleanup uses. Idempotent: a missing or now-different active transaction is a
// no-op success for this id.
func AbortPreparedTransaction(homeDir, transactionID string) error {
	prep, err := acquireFileLock(filepath.Join(upgradeRoot(homeDir), "prepare.lock"), false)
	if err != nil {
		return fmt.Errorf("lock upgrade preparation to abort: %w", err)
	}
	defer func() { _ = releaseFileLock(prep) }()

	txn, err := Load(homeDir)
	if errors.Is(err, ErrNoActiveTransaction) {
		return nil
	}
	if err != nil {
		return err
	}
	journal := txn.Journal()
	if journal.ID != transactionID {
		return nil // a different or newer transaction is active — not ours to abort
	}
	if journal.Phase != PhasePrepared {
		return fmt.Errorf(
			"cannot abort upgrade transaction %s from the initiating daemon in phase %s; a recovery actor owns it",
			journal.ID, journal.Phase)
	}
	// A held recovery lock means an actor took authority and will drive the
	// transaction; the initiating daemon must not remove it underneath.
	lock, err := acquireRecoveryLock(recoveryLockPath(homeDir, journal.ID), journal.RecoveryLockIdentity, true)
	if errors.Is(err, ErrRecoveryActive) {
		return fmt.Errorf("a recovery actor holds upgrade transaction %s; not aborting from the initiating daemon", journal.ID)
	}
	if err != nil {
		return fmt.Errorf("verify no recovery actor before aborting transaction %s: %w", journal.ID, err)
	}
	defer func() { _ = releaseFileLock(lock) }()

	if err := removeRequiredDurableFile(activeJournalPath(homeDir)); err != nil {
		return fmt.Errorf("remove active upgrade journal on abort: %w", err)
	}
	return txn.cleanupInactiveArtifacts()
}
