// Package upgradetxn owns the crash-durable filesystem transaction used to
// replace af. It is independent of the candidate: only the already-running
// previous binary may supervise, commit, or roll back an upgrade.
package upgradetxn

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	journalSchemaVersion = 1
	journalFileMode      = 0o600
	transactionDirMode   = 0o700
	recoveryNonceBytes   = 32
)

var (
	// ErrNoActiveTransaction means no upgrade journal is published for the AF
	// home. It is a normal result for entrypoints outside an upgrade.
	ErrNoActiveTransaction = errors.New("no active upgrade transaction")
	// ErrRecoveryActive means a live process holds the kernel recovery lock.
	// Timestamps are diagnostic only and never override this result.
	ErrRecoveryActive = errors.New("upgrade recovery is active")
	// ErrRecoveryActorMismatch means code that is not executing from the
	// transaction's immutable previous-binary artifact attempted to acquire
	// mutation authority. Candidates may observe or wake recovery, but they can
	// never supervise, commit, or roll back themselves.
	ErrRecoveryActorMismatch = errors.New("upgrade recovery actor is not the preserved previous binary")

	// errRecoveryCheckpointInterrupted is returned only by the in-package
	// crash-injection seam. A real actor death returns nothing; keeping the
	// phase at rolling_back models that durable state rather than mislabeling
	// the interruption as a failed restoration.
	errRecoveryCheckpointInterrupted = errors.New("upgrade recovery actor interrupted after checkpoint")

	transactionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Phase is a durable upgrade boundary. Every phase is persisted before a
// supervisor takes an action whose recovery meaning differs from the prior
// phase.
type Phase string

const (
	PhasePrepared            Phase = "prepared"
	PhaseSupervisorReady     Phase = "supervisor_ready"
	PhaseDaemonStopped       Phase = "daemon_stopped"
	PhaseCandidateInstalled  Phase = "candidate_installed"
	PhaseCandidateStarting   Phase = "candidate_starting"
	PhaseCandidateValidating Phase = "candidate_validating"
	PhaseCommitted           Phase = "committed"
	PhaseAborted             Phase = "aborted"
	PhaseRollingBack         Phase = "rolling_back"
	PhaseRollbackRestored    Phase = "rollback_restored"
	PhasePreviousStarting    Phase = "previous_starting"
	PhasePreviousValidating  Phase = "previous_validating"
	PhaseRolledBack          Phase = "rolled_back"
	PhaseRollbackFailed      Phase = "rollback_failed"
)

// RollbackProgress makes restoration resumable at file granularity. A
// checkpoint is persisted only after its atomic write/removal completes, so a
// crash before the checkpoint safely repeats that one idempotent operation.
type RollbackProgress struct {
	BinaryRestored   bool `json:"binary_restored"`
	MetadataRestored int  `json:"metadata_restored"`
}

// FileIdentity pins a durable lock pathname to the inode created before the
// transaction is published. A lock held on an unlinked inode cannot protect a
// replacement pathname, so every recovery acquisition verifies both values.
type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Plan contains all bytes and paths whose pre-upgrade state must be captured
// before the transaction becomes visible to another process.
type Plan struct {
	ID             string
	HomeDir        string
	ExecutablePath string
	FromVersion    string
	ToVersion      string
	Candidate      []byte
	Daemon         DaemonSnapshot
	RecoveryJob    RecoveryJob
	// MetadataPaths are paths relative to HomeDir. Existing regular files are
	// snapshotted; absence is recorded so rollback removes files created by a
	// candidate.
	MetadataPaths []string
}

// SupervisionKind is the captured owner used for both candidate activation
// and previous-daemon restoration. Recovery never guesses a different owner.
type SupervisionKind string

const (
	SupervisionNone    SupervisionKind = ""
	SupervisionAdHoc   SupervisionKind = "ad_hoc"
	SupervisionSystemd SupervisionKind = "systemd"
	SupervisionLaunchd SupervisionKind = "launchd"
)

// DaemonOwner is the service-manager identity captured before shutdown.
// ServiceName is empty only for an ad-hoc daemon.
type DaemonOwner struct {
	Kind        SupervisionKind `json:"kind"`
	ServiceName string          `json:"service_name,omitempty"`
}

// ListenerExpectation records only surfaces proven healthy before handoff.
// Candidate and rollback validation must restore each true surface.
type ListenerExpectation struct {
	HTTPUnixBound bool   `json:"http_unix_bound"`
	TCPConfigured bool   `json:"tcp_configured"`
	TCPListenAddr string `json:"tcp_listen_addr,omitempty"`
	TCPBound      bool   `json:"tcp_bound"`
}

// DaemonSnapshot is the exact pre-handoff daemon and supervision identity.
type DaemonSnapshot struct {
	WasRunning bool                `json:"was_running"`
	BootID     string              `json:"boot_id,omitempty"`
	Owner      DaemonOwner         `json:"owner"`
	Listeners  ListenerExpectation `json:"listeners"`
}

// RecoveryJobKind names the durable actor launcher selected before an upgrade
// becomes active. Service-managed daemons use a persistent transaction unit;
// ad-hoc daemons use a detached actor and rely on the all-entrypoint takeover
// gate after logout or reboot.
type RecoveryJobKind string

const (
	RecoveryJobDetached RecoveryJobKind = "detached"
	RecoveryJobSystemd  RecoveryJobKind = "systemd"
	RecoveryJobLaunchd  RecoveryJobKind = "launchd"
)

// RecoveryJob is persisted rather than re-derived after a reboot. UnitPath is
// exact and Name is the service-manager identity; both are empty for the
// detached fallback. The transaction ID determines the only valid name.
type RecoveryJob struct {
	Kind     RecoveryJobKind `json:"kind"`
	Name     string          `json:"name,omitempty"`
	UnitPath string          `json:"unit_path,omitempty"`
}

// MetadataParentSnapshot preserves the permissions of a metadata file's
// pre-existing parent directories so rollback never recreates them as 0755.
type MetadataParentSnapshot struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

// MetadataSnapshot describes one rollback input. SnapshotPath is empty when
// the metadata file did not exist at prepare time.
type MetadataSnapshot struct {
	Path         string                   `json:"path"`
	Existed      bool                     `json:"existed"`
	Mode         uint32                   `json:"mode,omitempty"`
	SHA256       string                   `json:"sha256,omitempty"`
	SnapshotPath string                   `json:"snapshot_path,omitempty"`
	Parents      []MetadataParentSnapshot `json:"parents,omitempty"`
}

// Journal is the fsynced recovery authority. Artifact paths are recorded for
// diagnostics but validated against paths derived from the transaction ID on
// every Load before they are used.
type Journal struct {
	SchemaVersion        int                `json:"schema_version"`
	ID                   string             `json:"id"`
	HomeDir              string             `json:"home_dir"`
	ExecutablePath       string             `json:"executable_path"`
	FromVersion          string             `json:"from_version"`
	ToVersion            string             `json:"to_version"`
	Phase                Phase              `json:"phase"`
	RecoveryNonce        string             `json:"recovery_nonce"`
	RecoveryLockIdentity FileIdentity       `json:"recovery_lock_identity"`
	PreviousBinaryPath   string             `json:"previous_binary_path"`
	PreviousBinarySHA256 string             `json:"previous_binary_sha256"`
	CandidatePath        string             `json:"candidate_path"`
	CandidateSHA256      string             `json:"candidate_sha256"`
	ExecutableMode       uint32             `json:"executable_mode"`
	Daemon               DaemonSnapshot     `json:"daemon"`
	RecoveryJob          RecoveryJob        `json:"recovery_job"`
	Metadata             []MetadataSnapshot `json:"metadata"`
	RollbackProgress     RollbackProgress   `json:"rollback_progress,omitempty"`
	UpdatedAt            time.Time          `json:"updated_at"`
}

// Transaction is a loaded, validated journal. The mutex only protects callers
// in one process; TryAcquireRecovery is the cross-process single-actor rule.
type Transaction struct {
	mu                      sync.Mutex
	journal                 Journal
	afterRollbackCheckpoint func(RollbackProgress) error
}

// ExecutableRole identifies bytes without granting recovery authority.
type ExecutableRole uint8

const (
	ExecutableUnknown ExecutableRole = iota
	ExecutablePrevious
	ExecutableCandidate
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

// Prepare snapshots every rollback input and publishes active.json last. A
// process crash before publication therefore leaves no transaction that a
// recovery actor could mistake for complete. Prepare does not quiesce a live
// daemon; a production caller must first prove the metadata manifest is stable.
func Prepare(stablePlan Plan) (_ *Transaction, retErr error) {
	home, err := canonicalExistingDir(stablePlan.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("validate upgrade home: %w", err)
	}
	if err := validateTransactionID(stablePlan.ID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(stablePlan.FromVersion) == "" || strings.TrimSpace(stablePlan.ToVersion) == "" {
		return nil, errors.New("upgrade versions cannot be blank")
	}
	if len(stablePlan.Candidate) == 0 {
		return nil, errors.New("candidate binary cannot be empty")
	}
	nonceBytes := make([]byte, recoveryNonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("generate upgrade recovery nonce: %w", err)
	}
	recoveryNonce := hex.EncodeToString(nonceBytes)

	executable, err := canonicalExistingFile(stablePlan.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("validate running executable: %w", err)
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return nil, fmt.Errorf("stat running executable: %w", err)
	}
	if !executableInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("running executable %s is not a regular file", executable)
	}
	previousBinary, err := os.ReadFile(executable)
	if err != nil {
		return nil, fmt.Errorf("read running executable: %w", err)
	}
	if digest(previousBinary) == digest(stablePlan.Candidate) {
		return nil, errors.New("candidate binary is byte-identical to the previous binary")
	}

	root := upgradeRoot(home)
	if err := ensureDurableDirectory(home, root, transactionDirMode); err != nil {
		return nil, fmt.Errorf("prepare upgrade root: %w", err)
	}

	preparationLock, err := acquireFileLock(filepath.Join(root, "prepare.lock"), false)
	if err != nil {
		return nil, fmt.Errorf("lock upgrade preparation: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(preparationLock))
	}()

	activePath := activeJournalPath(home)
	if _, err := os.Lstat(activePath); err == nil {
		return nil, fmt.Errorf("an upgrade transaction is already active at %s", activePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect active upgrade journal: %w", err)
	}

	txnDir := transactionDir(home, stablePlan.ID)
	transactionsRoot := filepath.Dir(txnDir)
	if err := ensureDurableDirectory(root, transactionsRoot, transactionDirMode); err != nil {
		return nil, fmt.Errorf("prepare transactions root: %w", err)
	}
	if err := createDurableDirectory(transactionsRoot, txnDir, transactionDirMode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("upgrade transaction artifacts already exist for %q", stablePlan.ID)
		}
		_ = os.Remove(txnDir)
		return nil, fmt.Errorf("create transaction directory: %w", err)
	}
	published := false
	previousPath, candidatePath := binaryArtifactPaths(executable, stablePlan.ID)
	var createdArtifacts []string
	defer func() {
		if published {
			return
		}
		for _, path := range createdArtifacts {
			_ = os.Remove(path)
		}
		_ = os.RemoveAll(txnDir)
	}()
	metadataDir := filepath.Join(txnDir, "metadata")
	if err := createDurableDirectory(txnDir, metadataDir, transactionDirMode); err != nil {
		return nil, fmt.Errorf("create metadata snapshot directory: %w", err)
	}
	lockPath := recoveryLockPath(home, stablePlan.ID)
	if _, err := os.Lstat(lockPath); err == nil {
		return nil, fmt.Errorf("upgrade recovery lock already exists at %s", lockPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect upgrade recovery lock: %w", err)
	}
	createdArtifacts = append(createdArtifacts, lockPath)
	if err := durableAtomicWriteFile(
		lockPath, []byte(recoveryNonce+"\n"), journalFileMode,
	); err != nil {
		return nil, fmt.Errorf("create durable upgrade recovery lock: %w", err)
	}
	recoveryLockInfo, err := os.Lstat(lockPath)
	if err != nil {
		return nil, fmt.Errorf("inspect durable upgrade recovery lock: %w", err)
	}
	recoveryLockIdentity, err := fileIdentity(recoveryLockInfo)
	if err != nil {
		return nil, fmt.Errorf("identify durable upgrade recovery lock: %w", err)
	}

	for _, path := range []string{previousPath, candidatePath} {
		if _, err := os.Lstat(path); err == nil {
			return nil, fmt.Errorf("upgrade binary artifact already exists at %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect upgrade binary artifact %s: %w", path, err)
		}
	}

	mode := executableInfo.Mode().Perm()
	createdArtifacts = append(createdArtifacts, previousPath)
	if err := durableAtomicWriteFile(previousPath, previousBinary, mode); err != nil {
		return nil, fmt.Errorf("snapshot previous binary: %w", err)
	}
	createdArtifacts = append(createdArtifacts, candidatePath)
	if err := durableAtomicWriteFile(candidatePath, stablePlan.Candidate, mode); err != nil {
		return nil, fmt.Errorf("stage candidate binary: %w", err)
	}

	metadata, err := snapshotMetadata(home, txnDir, stablePlan.MetadataPaths)
	if err != nil {
		return nil, err
	}

	journal := Journal{
		SchemaVersion:        journalSchemaVersion,
		ID:                   stablePlan.ID,
		HomeDir:              home,
		ExecutablePath:       executable,
		FromVersion:          stablePlan.FromVersion,
		ToVersion:            stablePlan.ToVersion,
		Phase:                PhasePrepared,
		RecoveryNonce:        recoveryNonce,
		RecoveryLockIdentity: recoveryLockIdentity,
		PreviousBinaryPath:   previousPath,
		PreviousBinarySHA256: digest(previousBinary),
		CandidatePath:        candidatePath,
		CandidateSHA256:      digest(stablePlan.Candidate),
		ExecutableMode:       uint32(mode),
		Daemon:               stablePlan.Daemon,
		RecoveryJob:          stablePlan.RecoveryJob,
		Metadata:             metadata,
		UpdatedAt:            time.Now().UTC(),
	}
	if err := validateJournal(home, journal); err != nil {
		return nil, fmt.Errorf("validate prepared journal: %w", err)
	}
	if err := persistJournal(activePath, journal); err != nil {
		if _, statErr := os.Lstat(activePath); statErr == nil {
			published = true
		}
		return nil, fmt.Errorf("publish upgrade journal: %w", err)
	}
	published = true
	return &Transaction{journal: journal}, nil
}

// Load validates the active journal and every derived path before returning
// anything that can mutate the filesystem.
func Load(homeDir string) (*Transaction, error) {
	home, err := canonicalExistingDir(homeDir)
	if err != nil {
		return nil, fmt.Errorf("validate upgrade home: %w", err)
	}
	journal, err := readJournal(activeJournalPath(home))
	if err != nil {
		return nil, err
	}
	if err := validateJournal(home, journal); err != nil {
		return nil, fmt.Errorf("validate active upgrade journal: %w", err)
	}
	return &Transaction{journal: journal}, nil
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

// Journal returns a copy suitable for health reporting and diagnostics.
func (t *Transaction) Journal() Journal {
	t.mu.Lock()
	defer t.mu.Unlock()
	journal := t.journal
	journal.Metadata = append([]MetadataSnapshot(nil), t.journal.Metadata...)
	for index := range journal.Metadata {
		journal.Metadata[index].Parents = append(
			[]MetadataParentSnapshot(nil), journal.Metadata[index].Parents...)
	}
	return journal
}

// RoleForExecutable hashes a regular executable and compares it with the two
// immutable transaction identities. This lets an entrypoint distinguish a
// canonical candidate daemon from a canonical previous daemon after rollback;
// it does not grant a RecoveryLease.
func (t *Transaction) RoleForExecutable(path string) (ExecutableRole, error) {
	canonical, err := canonicalExistingFile(path)
	if err != nil {
		return ExecutableUnknown, fmt.Errorf("validate upgrade executable role: %w", err)
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return ExecutableUnknown, fmt.Errorf("read upgrade executable role: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch digest(data) {
	case t.journal.PreviousBinarySHA256:
		return ExecutablePrevious, nil
	case t.journal.CandidateSHA256:
		return ExecutableCandidate, nil
	default:
		return ExecutableUnknown, nil
	}
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

// Advance persists a state-machine boundary that does not itself modify the
// candidate or rollback inputs.
func (t *Transaction) advance(next Phase) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == next {
		return nil
	}
	allowed := map[Phase]Phase{
		PhasePrepared:           PhaseSupervisorReady,
		PhaseSupervisorReady:    PhaseDaemonStopped,
		PhaseCandidateInstalled: PhaseCandidateStarting,
		PhaseCandidateStarting:  PhaseCandidateValidating,
		PhaseRollbackRestored:   PhasePreviousStarting,
		PhasePreviousStarting:   PhasePreviousValidating,
		PhasePreviousValidating: PhaseRolledBack,
	}
	if allowed[t.journal.Phase] != next {
		return fmt.Errorf("invalid upgrade phase transition %s -> %s", t.journal.Phase, next)
	}
	return t.persistPhaseLocked(next)
}

// InstallCandidate atomically replaces the executable only after the durable
// journal proves the previous daemon was stopped. If the process died after
// rename but before phase persistence, the candidate hash makes retry safe.
func (t *Transaction) installCandidate() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == PhaseCandidateInstalled {
		return t.verifyInstalledCandidateLocked()
	}
	if t.journal.Phase != PhaseDaemonStopped {
		return fmt.Errorf("cannot install candidate from upgrade phase %s", t.journal.Phase)
	}
	previous, err := readAndVerify(t.journal.PreviousBinaryPath, t.journal.PreviousBinarySHA256)
	if err != nil {
		return fmt.Errorf("verify previous binary snapshot: %w", err)
	}
	candidate, err := readAndVerify(t.journal.CandidatePath, t.journal.CandidateSHA256)
	if err != nil {
		return fmt.Errorf("verify candidate binary: %w", err)
	}
	current, err := os.ReadFile(t.journal.ExecutablePath)
	if err != nil {
		return fmt.Errorf("read installed executable: %w", err)
	}
	switch digest(current) {
	case t.journal.CandidateSHA256:
		// Resume after the atomic rename but before phase persistence.
	case t.journal.PreviousBinarySHA256:
		if digest(previous) != t.journal.PreviousBinarySHA256 {
			return errors.New("previous binary snapshot changed during install")
		}
		if err := durableAtomicWriteFile(
			t.journal.ExecutablePath, candidate, os.FileMode(t.journal.ExecutableMode)); err != nil {
			return fmt.Errorf("install candidate binary: %w", err)
		}
	default:
		return errors.New("installed executable matches neither the previous nor candidate binary")
	}
	return t.persistPhaseLocked(PhaseCandidateInstalled)
}

func (t *Transaction) verifyInstalledCandidateLocked() error {
	info, err := os.Lstat(t.journal.ExecutablePath)
	if err != nil {
		return fmt.Errorf("inspect installed candidate: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("installed candidate is not a regular file")
	}
	if info.Mode().Perm() != os.FileMode(t.journal.ExecutableMode) {
		return fmt.Errorf("installed candidate mode is %04o, want %04o",
			info.Mode().Perm(), os.FileMode(t.journal.ExecutableMode))
	}
	_, err = readAndVerify(t.journal.ExecutablePath, t.journal.CandidateSHA256)
	if err != nil {
		return fmt.Errorf("verify installed candidate: %w", err)
	}
	return nil
}

// Commit records the validation verdict durably before Cleanup may remove any
// rollback material.
func (t *Transaction) commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == PhaseCommitted {
		return nil
	}
	if t.journal.Phase != PhaseCandidateValidating {
		return fmt.Errorf("cannot commit upgrade from phase %s", t.journal.Phase)
	}
	if err := t.verifyInstalledCandidateLocked(); err != nil {
		return err
	}
	return t.persistPhaseLocked(PhaseCommitted)
}

// abort records that activation ended before the previous daemon stopped.
// It deliberately restores nothing: overwriting metadata while the proven
// live previous daemon may still be writing it would turn a safe refusal into
// data loss.
func (t *Transaction) abort() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == PhaseAborted {
		return nil
	}
	if t.journal.Phase != PhasePrepared && t.journal.Phase != PhaseSupervisorReady {
		return fmt.Errorf("cannot abort upgrade from phase %s", t.journal.Phase)
	}
	return t.persistPhaseLocked(PhaseAborted)
}

// Rollback restores the binary, every metadata snapshot, and every recorded
// absence. Artifacts remain until a caller proves the previous daemon healthy
// and explicitly invokes Cleanup.
func (t *Transaction) rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.journal.Phase {
	case PhaseRollbackRestored, PhasePreviousStarting, PhasePreviousValidating, PhaseRolledBack:
		return nil
	case PhaseCommitted:
		return errors.New("cannot roll back a committed upgrade")
	case PhaseRollingBack:
		// Resume an interrupted restoration.
	case PhaseDaemonStopped, PhaseCandidateInstalled, PhaseCandidateStarting, PhaseCandidateValidating:
		if err := t.persistPhaseLocked(PhaseRollingBack); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot roll back upgrade from phase %s", t.journal.Phase)
	}

	if err := t.restoreLocked(); err != nil {
		if errors.Is(err, errRecoveryCheckpointInterrupted) {
			return err
		}
		phaseErr := t.persistPhaseLocked(PhaseRollbackFailed)
		return errors.Join(err, phaseErr)
	}
	return t.persistPhaseLocked(PhaseRollbackRestored)
}

func (t *Transaction) restoreLocked() error {
	if !t.journal.RollbackProgress.BinaryRestored {
		previous, err := readAndVerify(t.journal.PreviousBinaryPath, t.journal.PreviousBinarySHA256)
		if err != nil {
			return fmt.Errorf("verify previous binary snapshot: %w", err)
		}
		if err := durableAtomicWriteFile(
			t.journal.ExecutablePath, previous, os.FileMode(t.journal.ExecutableMode)); err != nil {
			return fmt.Errorf("restore previous binary: %w", err)
		}
		journal := t.journal
		journal.RollbackProgress.BinaryRestored = true
		if err := t.persistJournalLocked(journal); err != nil {
			return fmt.Errorf("checkpoint restored previous binary: %w", err)
		}
		if t.afterRollbackCheckpoint != nil {
			if err := t.afterRollbackCheckpoint(t.journal.RollbackProgress); err != nil {
				return err
			}
		}
	}

	for index := t.journal.RollbackProgress.MetadataRestored; index < len(t.journal.Metadata); index++ {
		metadata := t.journal.Metadata[index]
		if err := restoreMetadataEntry(t.journal.HomeDir, metadata); err != nil {
			return err
		}
		journal := t.journal
		journal.RollbackProgress.MetadataRestored = index + 1
		if err := t.persistJournalLocked(journal); err != nil {
			return fmt.Errorf("checkpoint restored metadata %s: %w", metadata.Path, err)
		}
		if t.afterRollbackCheckpoint != nil {
			if err := t.afterRollbackCheckpoint(t.journal.RollbackProgress); err != nil {
				return err
			}
		}
	}
	return nil
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

func (t *Transaction) persistPhaseLocked(phase Phase) error {
	journal := t.journal
	journal.Phase = phase
	return t.persistJournalLocked(journal)
}

func (t *Transaction) persistJournalLocked(journal Journal) error {
	journal.UpdatedAt = time.Now().UTC()
	if err := persistJournal(activeJournalPath(journal.HomeDir), journal); err != nil {
		return fmt.Errorf("persist upgrade phase %s: %w", journal.Phase, err)
	}
	t.journal = journal
	return nil
}
