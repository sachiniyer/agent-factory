package upgradetxn

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type activationApproval struct {
	SchemaVersion int    `json:"schema_version"`
	TransactionID string `json:"transaction_id"`
	Nonce         string `json:"nonce"`
	ActorID       string `json:"actor_id"`
}

func newRecoveryActorID() (string, error) {
	value := make([]byte, recoveryNonceBytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate upgrade recovery actor identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// AuthorizeActivation completes the old process -> previous-binary actor
// handshake. The approval is bound to the exact recovery-lock acquisition
// whose supervisor_ready proof was validated, not merely to the transaction.
func (t *Transaction) AuthorizeActivation(transactionID, nonce string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	current, err := readJournal(activeJournalPath(t.journal.HomeDir))
	if err != nil {
		return err
	}
	if err := validateJournal(t.journal.HomeDir, current); err != nil {
		return err
	}
	if current.ID != transactionID || current.RecoveryNonce != nonce {
		return errors.New("upgrade activation handshake does not match the active transaction")
	}
	if current.Phase != PhaseSupervisorReady {
		return fmt.Errorf("upgrade supervisor is not ready for activation (phase %s)", current.Phase)
	}
	status, err := validateActivationRecoveryProof(current, time.Now().UTC())
	if err != nil {
		return err
	}
	approval := activationApproval{
		SchemaVersion: journalSchemaVersion,
		TransactionID: current.ID,
		Nonce:         current.RecoveryNonce,
		ActorID:       status.ActorID,
	}
	data, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade activation approval: %w", err)
	}
	data = append(data, '\n')
	approvalPath := filepath.Join(transactionDir(current.HomeDir, current.ID), "activation.approved")
	if err := publishActivationApproval(approvalPath, data); err != nil {
		return err
	}
	t.journal = current
	return nil
}

func publishActivationApproval(path string, data []byte) error {
	writeErr := durableAtomicWriteFile(path, data, journalFileMode)
	if writeErr == nil {
		return nil
	}
	// A directory-sync failure happens after rename, so the exact approval can
	// be visible even though the durable writer reports an error. Complete that
	// barrier here when possible; ActivationAuthorized repeats it before use.
	visible, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(visible, data) {
		return fmt.Errorf("persist upgrade activation approval: %w", writeErr)
	}
	if syncErr := syncTransactionDirectory(filepath.Dir(path)); syncErr != nil {
		return errors.Join(
			fmt.Errorf("persist upgrade activation approval: %w", writeErr),
			fmt.Errorf("confirm visible upgrade activation approval: %w", syncErr),
		)
	}
	return nil
}

func validateActivationRecoveryProof(current Journal, now time.Time) (RecoveryStatus, error) {
	lockPath := recoveryLockPath(current.HomeDir, current.ID)
	probe, err := acquireRecoveryLock(lockPath, current.RecoveryLockIdentity, true)
	if err == nil {
		_ = releaseFileLock(probe)
		return RecoveryStatus{}, errors.New("upgrade activation cannot be authorized without a live recovery actor")
	}
	if !errors.Is(err, ErrRecoveryActive) {
		return RecoveryStatus{}, fmt.Errorf("verify recovery actor before activation: %w", err)
	}
	readyPath := filepath.Join(transactionDir(current.HomeDir, current.ID), "recovery.ready.lock")
	readyProbe, err := acquireFileLock(readyPath, true)
	if err == nil {
		_ = releaseFileLock(readyProbe)
		return RecoveryStatus{}, errors.New("upgrade recovery actor has not published its current readiness proof")
	}
	if !errors.Is(err, ErrRecoveryActive) {
		return RecoveryStatus{}, fmt.Errorf("verify recovery actor readiness before activation: %w", err)
	}
	status, err := readRecoveryStatusForJournal(current)
	if err != nil {
		return RecoveryStatus{}, err
	}
	if status.Phase != PhaseSupervisorReady {
		return RecoveryStatus{}, fmt.Errorf(
			"upgrade recovery actor has not reached supervisor_ready (status %s)", status.Phase)
	}
	if !now.Before(status.Deadline) {
		return RecoveryStatus{}, fmt.Errorf(
			"upgrade recovery actor's supervisor_ready deadline expired at %s", status.Deadline.Format(time.RFC3339Nano))
	}
	return status, nil
}

// ActivationAuthorized may only be consumed through the still-held recovery
// lease to which the old process granted approval. A missing approval or one
// for a predecessor is a normal not-yet result: a takeover must publish a new
// readiness proof and obtain its own approval before it can stop the daemon.
func (l *RecoveryLease) ActivationAuthorized() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.file == nil || l.readyFile == nil {
		return false, errors.New("upgrade recovery lease is released")
	}
	path := filepath.Join(filepath.Dir(l.path), "activation.approved")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read upgrade activation approval: %w", err)
	}
	var approval activationApproval
	if err := json.Unmarshal(data, &approval); err != nil {
		return false, fmt.Errorf("decode upgrade activation approval: %w", err)
	}
	if approval.SchemaVersion != journalSchemaVersion ||
		approval.TransactionID != l.txnID || approval.Nonce != l.nonce || !validDigest(approval.ActorID) {
		return false, errors.New("upgrade activation approval does not match the transaction")
	}
	if approval.ActorID != l.actorID {
		return false, nil
	}
	if err := syncTransactionDirectory(filepath.Dir(path)); err != nil {
		return false, fmt.Errorf("confirm durable upgrade activation approval: %w", err)
	}
	return true, nil
}
