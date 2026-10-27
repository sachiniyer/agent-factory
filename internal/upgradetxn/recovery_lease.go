// Recovery-authority layer for the upgrade transaction, split from
// transaction.go when that file approached the 1000-line limit (#1145).
//
// TryAcquireRecovery is the cross-process single-actor rule: the winner holds
// a kernel death-test flock plus a readiness flock and publishes a heartbeat
// the rest of the package validates against the journal. This file holds that
// layer end to end — the RecoveryLease type and the RecoveryStatus handshake
// it writes, the read-only probes for that handshake (ReadRecoveryStatus,
// RecoveryActorLive), acquisition and the lock-directory repair it may have
// to perform first, the lease methods that drive the transaction state
// machine under that authority, and release.
package upgradetxn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecoveryLease holds the kernel death-test flock and a second readiness flock.
// Acquisition first deletes stale status; Heartbeat then publishes this actor's
// status. Authorization requires both flocks and that current publication.
type RecoveryLease struct {
	mu         sync.Mutex
	file       *os.File
	readyFile  *os.File
	path       string
	txnID      string
	nonce      string
	actorID    string
	executable string
	txn        *Transaction
	released   bool
}

// RecoveryStatus is the diagnostic handshake written by the live previous-
// binary actor. The flock, not these fields, decides whether that actor is
// alive; callers validate both independently.
type RecoveryStatus struct {
	SchemaVersion int       `json:"schema_version"`
	TransactionID string    `json:"transaction_id"`
	Nonce         string    `json:"nonce"`
	ActorID       string    `json:"actor_id"`
	PID           int       `json:"pid"`
	ProcessStart  string    `json:"process_start,omitempty"`
	BootID        string    `json:"boot_id,omitempty"`
	Executable    string    `json:"executable"`
	Phase         Phase     `json:"phase"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
	Deadline      time.Time `json:"deadline"`
}

// ReadRecoveryStatus validates the live actor's diagnostic handshake. It does
// not claim the actor is alive; callers must separately observe the kernel
// lock through RecoveryActorLive because timestamps and PIDs are never death
// authority.
func ReadRecoveryStatus(homeDir string) (RecoveryStatus, error) {
	txn, err := Load(homeDir)
	if err != nil {
		return RecoveryStatus{}, err
	}
	journal := txn.Journal()
	return readRecoveryStatusForJournal(journal)
}

func readRecoveryStatusForJournal(journal Journal) (RecoveryStatus, error) {
	path := filepath.Join(transactionDir(journal.HomeDir, journal.ID), "recovery.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return RecoveryStatus{}, fmt.Errorf("read upgrade recovery status: %w", err)
	}
	var status RecoveryStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return RecoveryStatus{}, fmt.Errorf("decode upgrade recovery status: %w", err)
	}
	if status.SchemaVersion != journalSchemaVersion ||
		status.TransactionID != journal.ID ||
		status.Nonce != journal.RecoveryNonce ||
		!validDigest(status.ActorID) ||
		status.PID <= 0 ||
		!validPhase(status.Phase) ||
		status.HeartbeatAt.IsZero() ||
		status.Deadline.IsZero() {
		return RecoveryStatus{}, errors.New("upgrade recovery status does not match the active transaction")
	}
	executable, err := canonicalExistingFile(status.Executable)
	if err != nil || executable != journal.PreviousBinaryPath {
		return RecoveryStatus{}, errors.New("upgrade recovery status is not from the preserved previous binary")
	}
	return status, nil
}

// RecoveryActorLive reports only the kernel lock fact. A false result is a
// point-in-time observation; the caller must still race safely when waking a
// previous-binary takeover actor.
func (t *Transaction) RecoveryActorLive() (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureRecoveryLockDirectoryLocked(); err != nil {
		return false, err
	}
	lockPath := recoveryLockPath(t.journal.HomeDir, t.journal.ID)
	file, err := acquireRecoveryLock(lockPath, t.journal.RecoveryLockIdentity, true)
	if errors.Is(err, ErrRecoveryActive) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe upgrade recovery lock: %w", err)
	}
	return false, releaseFileLock(file)
}

// TryAcquireRecovery obtains the only authority to advance or recover this
// transaction. It never waits: callers that lose the race must leave the live
// supervisor alone.
func (t *Transaction) TryAcquireRecovery() (*RecoveryLease, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve upgrade recovery executable: %w", err)
	}
	return t.tryAcquireRecoveryAs(executable)
}

// tryAcquireRecoveryAs is the test seam for the structural actor check.
// Production has only TryAcquireRecovery, which supplies os.Executable and
// therefore cannot nominate some other path on the candidate's behalf.
func (t *Transaction) tryAcquireRecoveryAs(actorExecutable string) (*RecoveryLease, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureRecoveryLockDirectoryLocked(); err != nil {
		return nil, err
	}
	lockPath := recoveryLockPath(t.journal.HomeDir, t.journal.ID)
	file, err := acquireRecoveryLock(lockPath, t.journal.RecoveryLockIdentity, true)
	if err != nil {
		if errors.Is(err, ErrRecoveryActive) {
			return nil, err
		}
		return nil, fmt.Errorf("acquire upgrade recovery lock: %w", err)
	}
	// Load happened before the flock and may be arbitrarily stale. Refresh only
	// after becoming the single recovery actor so a takeover resumes the last
	// fsynced phase rather than publishing an older in-memory phase over it.
	current, err := readJournal(activeJournalPath(t.journal.HomeDir))
	if err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("reload upgrade journal after recovery lock: %w", err)
	}
	if err := validateJournal(t.journal.HomeDir, current); err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("validate upgrade journal after recovery lock: %w", err)
	}
	if current.ID != t.journal.ID {
		_ = releaseFileLock(file)
		return nil, errors.New("active upgrade transaction changed before recovery lock acquisition")
	}
	t.journal = current
	// readJournal returns bytes, not a durability guarantee. A takeover process
	// has no way to know whether the prior writer observed errJournalVisibleNotDurable
	// (rename landed, directory sync never closed) and died before re-persisting, so
	// it must be at least as conservative as the writer: mark the adopted journal
	// unconfirmed and let reaffirmDurableJournal close the barrier before any
	// irreversible phase arm acts on it (#3453). The redundant write is the cost a
	// takeover pays for the same guarantee a same-process adoption already enforces.
	t.journalUnconfirmed = true
	actorPath, err := canonicalExistingFile(actorExecutable)
	if err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("validate upgrade recovery actor: %w", err)
	}
	if actorPath != t.journal.PreviousBinaryPath {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("%w: got %s", ErrRecoveryActorMismatch, actorPath)
	}
	if _, err := readAndVerify(actorPath, t.journal.PreviousBinarySHA256); err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("%w: %v", ErrRecoveryActorMismatch, err)
	}
	statusPath := filepath.Join(transactionDir(t.journal.HomeDir, t.journal.ID), "recovery.json")
	// recovery.json belongs to the former flock owner. Remove it while holding
	// the newly acquired lock so no caller can combine this actor's liveness
	// with a dead actor's future-dated supervisor_ready heartbeat.
	if err := removeDurableFile(statusPath); err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("invalidate previous upgrade recovery status: %w", err)
	}
	readyPath := filepath.Join(transactionDir(t.journal.HomeDir, t.journal.ID), "recovery.ready.lock")
	readyFile, err := acquireFileLock(readyPath, true)
	if err != nil {
		_ = releaseFileLock(file)
		return nil, fmt.Errorf("acquire upgrade recovery readiness lock: %w", err)
	}
	actorID, err := newRecoveryActorID()
	if err != nil {
		_ = releaseFileLock(readyFile)
		_ = releaseFileLock(file)
		return nil, err
	}
	return &RecoveryLease{
		file:       file,
		readyFile:  readyFile,
		path:       statusPath,
		txnID:      t.journal.ID,
		nonce:      t.journal.RecoveryNonce,
		actorID:    actorID,
		executable: actorPath,
		txn:        t,
	}, nil
}

// ensureRecoveryLockDirectoryLocked makes terminal cleanup recoverable across
// the narrow crash boundary where a previous actor removed the transaction
// directory but died before removing active.json. Nonterminal transactions
// never recreate missing rollback storage: losing that storage is a hard error.
func (t *Transaction) ensureRecoveryLockDirectoryLocked() error {
	txnDir := transactionDir(t.journal.HomeDir, t.journal.ID)
	info, err := os.Lstat(txnDir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("upgrade transaction lock path is not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect upgrade transaction lock directory: %w", err)
	}

	current, err := readJournal(activeJournalPath(t.journal.HomeDir))
	if err != nil {
		return fmt.Errorf("read terminal journal before recreating recovery lock: %w", err)
	}
	if err := validateJournal(t.journal.HomeDir, current); err != nil {
		return fmt.Errorf("validate terminal journal before recreating recovery lock: %w", err)
	}
	if current.ID != t.journal.ID {
		return errors.New("active upgrade transaction changed before recovery lock recreation")
	}
	if current.Phase != PhaseCommitted && current.Phase != PhaseRolledBack && current.Phase != PhaseAborted {
		return fmt.Errorf("recovery storage is missing in nonterminal phase %s", current.Phase)
	}
	parent := filepath.Dir(txnDir)
	if err := validateDirectoryNoSymlink(parent); err != nil {
		return fmt.Errorf("validate transactions root for terminal cleanup: %w", err)
	}
	if err := createDurableDirectory(parent, txnDir, transactionDirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("recreate terminal transaction lock directory: %w", err)
	}
	info, err = os.Lstat(txnDir)
	if err != nil {
		return fmt.Errorf("inspect recreated transaction lock directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("recreated upgrade transaction lock path is not a directory")
	}
	t.journal = current
	return nil
}

// Advance records a state-machine boundary while this lease owns recovery.
func (l *RecoveryLease) Advance(next Phase) error {
	return l.withTransaction(func(txn *Transaction) error { return txn.advance(next) })
}

// InstallCandidate atomically replaces the executable after DaemonStopped.
func (l *RecoveryLease) InstallCandidate() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.installCandidate() })
}

// Commit persists the candidate validation verdict before cleanup.
func (l *RecoveryLease) Commit() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.commit() })
}

// Rollback restores the previous binary and metadata snapshots.
func (l *RecoveryLease) Rollback() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.rollback() })
}

// Cleanup removes terminal transaction artifacts.
func (l *RecoveryLease) Cleanup() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.cleanup() })
}

// Abort terminates activation before the previous daemon stopped, without
// restoring binary or metadata over that still-live owner.
func (l *RecoveryLease) Abort() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.abort() })
}

// reaffirmDurableJournal closes the barrier on a journal this process ADOPTED
// from a write whose bytes were visible but whose directory sync never completed
// (#3453). It is a no-op — no write at all — whenever the journal is already
// durable, which is every ordinary call.
//
// The internal lifecycle skips reaffirm themselves, but they only protect callers
// that go THROUGH them. Supervisor.Run does not: it reads Journal().Phase and acts
// on it directly, and its PhaseCommitted arm approves the candidate, disables the
// recovery job, and starts cleanup without ever calling Commit. An adopted
// PhaseCommitted there would run those irreversible effects over a journal a crash
// can still lose, recovering the older candidate_validating entry with no actor
// left to finish it (#3453 review). So the phase is made durable before it is
// acted on, at the one place that reads it.
func (l *RecoveryLease) reaffirmDurableJournal() error {
	return l.withTransaction(func(txn *Transaction) error { return txn.reaffirmDurableJournal() })
}

func (t *Transaction) reaffirmDurableJournal() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reaffirmPhaseLocked(t.journal.Phase)
}

func (l *RecoveryLease) withTransaction(action func(*Transaction) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.file == nil || l.txn == nil {
		return errors.New("upgrade recovery lease is released")
	}
	return action(l.txn)
}

// Heartbeat records the lock owner's diagnostics and deadline. The readiness
// flock was acquired only after stale status was durably invalidated.
func (l *RecoveryLease) Heartbeat(phase Phase, deadline time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.file == nil || l.readyFile == nil {
		return errors.New("upgrade recovery lease is released")
	}
	heartbeat := RecoveryStatus{
		SchemaVersion: journalSchemaVersion,
		TransactionID: l.txnID,
		Nonce:         l.nonce,
		ActorID:       l.actorID,
		PID:           os.Getpid(),
		ProcessStart:  processStartIdentity(),
		BootID:        kernelBootID(),
		Executable:    l.executable,
		Phase:         phase,
		HeartbeatAt:   time.Now().UTC(),
		Deadline:      deadline.UTC(),
	}
	data, err := json.MarshalIndent(heartbeat, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recovery heartbeat: %w", err)
	}
	data = append(data, '\n')
	if err := durableAtomicWriteFile(l.path, data, journalFileMode); err != nil {
		return fmt.Errorf("persist recovery heartbeat: %w", err)
	}
	return nil
}

// Release relinquishes the kernel death test. It is idempotent.
func (l *RecoveryLease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	readyErr := releaseFileLock(l.readyFile)
	l.readyFile = nil
	err := releaseFileLock(l.file)
	l.file = nil
	return errors.Join(readyErr, err)
}
