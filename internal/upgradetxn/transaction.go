// Package upgradetxn owns the crash-durable filesystem transaction used to
// replace af. It is independent of the candidate: only the already-running
// previous binary may supervise, commit, or roll back an upgrade.
package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"

	"github.com/sachiniyer/agent-factory/log"
)

const (
	journalSchemaVersion = 1
	journalFileMode      = 0o600
	transactionDirMode   = 0o700
	recoveryNonceBytes   = 32
	systemdDaemonService = systemdunit.DaemonUnitName
	launchdDaemonService = "com.agent-factory.daemon"
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
	PhaseDaemonStopping      Phase = "daemon_stopping"
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

// IsRollbackRestore reports whether the previous daemon is being restored and
// (re)started under this phase — the window in which the recovery actor calls
// StartPrevious/ValidatePrevious (supervisor.go). A daemon starting during these
// phases IS the previous daemon the actor is bringing back, so a daemon-bind
// entrypoint gate must let it bind rather than defer to the very actor waiting
// on it — otherwise automatic rollback deadlocks and dead-ends in rollback_failed
// with no daemon running.
//
// The canonical executable has been restored to the previous binary by
// rollback_restored, so a bind here is the correct previous version. The forward
// candidate-start phases are deliberately excluded: an ordinary daemon must NOT
// bind a rival over a live candidate — the candidate is started through the
// transaction-id probation path, not this gate.
func (p Phase) IsRollbackRestore() bool {
	switch p {
	case PhaseRollbackRestored, PhasePreviousStarting, PhasePreviousValidating, PhaseRolledBack:
		return true
	default:
		return false
	}
}

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
	// ExpectedPreviousSHA256, when set, is the digest the caller last observed
	// for the executable it means to replace. Prepare verifies it INSIDE the
	// locks, against the same bytes it snapshots as the rollback binary, and
	// refuses on a mismatch.
	//
	// A caller cannot do this for itself. Any check it makes happens before
	// Prepare takes the locks, so an in-place installer can land in the gap —
	// and then Prepare preserves the newer bytes as "previous" while installing
	// the caller's older candidate over them. Empty means no expectation, which
	// is what a caller with no prior observation (a test, a manual trigger)
	// passes.
	ExpectedPreviousSHA256 string
	Daemon                 DaemonSnapshot
	RecoveryJob            RecoveryJob
	// MetadataPaths are paths relative to HomeDir. Existing regular files are
	// snapshotted; absence is recorded so rollback removes files created by a
	// candidate.
	MetadataPaths []string
	// ClearUnverifiableArtifacts lets Prepare set aside a staged artifact beside
	// the executable whose owning home cannot be read, instead of refusing.
	//
	// Off by default, and it has to be: an unreadable claim is still a claim, and
	// an unmounted home reports exactly what a finished transaction does. It is
	// the operator's way past the one case the evidence cannot settle, which an
	// unattended daemon would otherwise meet forever with no override at all
	// (#3864, and the unoverridable-block shape of #2859).
	ClearUnverifiableArtifacts bool
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
	// CandidateInstalled records that the candidate's BYTES actually reached the
	// executable (#3011 review). A rollback can happen without that: an actor lost
	// after PhaseDaemonStopping, or an install error before the rename, both roll
	// back a candidate that was never installed and never validated. Disqualifying
	// such a release would block an upgrade the box never actually tried — a
	// permanent block earned by an interruption rather than by bad bytes, which is
	// the unoverridable shape #2859 was bitten by.
	CandidateInstalled bool      `json:"candidate_installed,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Transaction is a loaded, validated journal. The mutex only protects callers
// in one process; TryAcquireRecovery is the cross-process single-actor rule.
type Transaction struct {
	mu      sync.Mutex
	journal Journal
	// journalUnconfirmed marks a journal adopted from a write whose bytes are
	// VISIBLE at the path but whose durability barrier never closed (#3453).
	// t.journal has to follow those bytes or the next write erases them — but a
	// phase that arrived this way must not satisfy an idempotence skip or a
	// durability gate. See reaffirmPhaseLocked.
	journalUnconfirmed      bool
	afterRollbackCheckpoint func(RollbackProgress) error
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
	journal.CandidateInstalled = journal.CandidateInstalled || phaseImpliesCandidateInstalled(journal.Phase)
	return &Transaction{journal: journal}, nil
}

// phaseImpliesCandidateInstalled derives CandidateInstalled for a journal written
// before the field existed (#3011 review).
//
// The field is omitempty and the schema version did not change, so a transaction
// left in flight by the PREVIOUS release decodes with it false. Resumed by this
// binary it would roll back without disqualifying a candidate whose bytes really
// did run — the safety mechanism silently skipped for exactly one upgrade, which is
// the boundary nobody tests by hand.
//
// Derived at LOAD, while the phase can still answer the question. These three
// phases are reached only after the bytes are on disk; PhaseRolledBack cannot be
// read this way, which is why the inference has to happen before the transaction
// moves on. From here the value is carried in the journal like any other.
func phaseImpliesCandidateInstalled(phase Phase) bool {
	switch phase {
	case PhaseCandidateInstalled, PhaseCandidateStarting, PhaseCandidateValidating:
		return true
	}
	return false
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

// Advance persists a state-machine boundary that does not itself modify the
// candidate or rollback inputs.
func (t *Transaction) advance(next Phase) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == next {
		return t.reaffirmPhaseLocked(next)
	}
	allowed := map[Phase]Phase{
		PhasePrepared:           PhaseSupervisorReady,
		PhaseSupervisorReady:    PhaseDaemonStopping,
		PhaseDaemonStopping:     PhaseDaemonStopped,
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
		if err := t.verifyInstalledCandidateLocked(); err != nil {
			return err
		}
		return t.reaffirmPhaseLocked(PhaseCandidateInstalled)
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
	// Durable, and written with the phase it belongs to: the bytes are on disk now,
	// so a later rollback can tell "this candidate was tried and failed" from "this
	// candidate never ran".
	journal := t.journal
	journal.CandidateInstalled = true
	journal.Phase = PhaseCandidateInstalled
	return t.persistJournalLocked(journal)
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
		return t.reaffirmPhaseLocked(PhaseCommitted)
	}
	if t.journal.Phase != PhaseCandidateValidating {
		return fmt.Errorf("cannot commit upgrade from phase %s", t.journal.Phase)
	}
	if err := t.verifyInstalledCandidateLocked(); err != nil {
		return err
	}
	return t.persistPhaseLocked(PhaseCommitted)
}

// abort records that activation ended before the previous daemon was confirmed
// stopped. It deliberately restores nothing: overwriting metadata while the
// proven-live previous daemon may still be writing it would turn a safe refusal
// into data loss.
func (t *Transaction) abort() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.journal.Phase == PhaseAborted {
		return t.reaffirmPhaseLocked(PhaseAborted)
	}
	if t.journal.Phase != PhasePrepared && t.journal.Phase != PhaseSupervisorReady &&
		t.journal.Phase != PhaseDaemonStopping {
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
		// Already past the restore. Same rule as the lifecycle skips: an ADOPTED
		// boundary is not a durable one, and reporting it as finished would let a
		// retried Rollback confirm a barrier that never closed (#3453 review).
		return t.reaffirmPhaseLocked(t.journal.Phase)
	case PhaseCommitted:
		return errors.New("cannot roll back a committed upgrade")
	case PhaseRollingBack:
		// Resume an interrupted restoration.
	case PhaseDaemonStopped, PhaseCandidateInstalled, PhaseCandidateStarting, PhaseCandidateValidating:
		if err := t.persistPhaseLocked(PhaseRollingBack); err != nil {
			return err
		}
		if t.afterRollbackCheckpoint != nil {
			if err := t.afterRollbackCheckpoint(t.journal.RollbackProgress); err != nil {
				return err
			}
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
	// Ask the executable whether the candidate ran, BEFORE overwriting the evidence
	// (#3011 review). The marker is normally written with PhaseCandidateInstalled,
	// but the rename can succeed and that journal write fail — and a takeover from
	// the phase BEFORE install rolls back without re-entering InstallCandidate, so
	// nothing ever sets it. The bytes on disk are the ground truth, and this is the
	// last moment they exist.
	if !t.journal.CandidateInstalled {
		installed, readErr := os.ReadFile(t.journal.ExecutablePath)
		switch {
		case readErr != nil:
			// "I could not read it" is not "it was never installed" (#3011 review).
			// Silently treating a read failure as evidence of absence would let a
			// candidate that really did run escape disqualification, which is the one
			// outcome this marker exists to prevent. Reported rather than assumed —
			// and NOT returned, because the restore below is what gets the box back to
			// a known-good binary and must not be blocked by a diagnostic.
			log.WarningLog.Printf(
				"could not read %s to confirm whether the candidate had been installed before restoring it; the rollback will proceed and the candidate may not be disqualified: %v",
				t.journal.ExecutablePath, readErr)
		case digest(installed) == t.journal.CandidateSHA256:
			journal := t.journal
			journal.CandidateInstalled = true
			if err := t.persistJournalLocked(journal); err != nil {
				return fmt.Errorf("checkpoint that the candidate had been installed: %w", err)
			}
		}
	}
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

func (t *Transaction) persistPhaseLocked(phase Phase) error {
	journal := t.journal
	journal.Phase = phase
	return t.persistJournalLocked(journal)
}

// persistJournalLocked writes the journal and advances the in-memory copy to
// match what is now ON DISK — which is not always the same thing as "the write
// succeeded".
//
// t.journal is the base every later write starts from (persistPhaseLocked copies
// it and changes one field), so leaving it behind the disk is how a landed record
// gets overwritten. That was reachable on the rollback path: restoreLocked
// checkpoints CandidateInstalled=true, the directory sync fails AFTER the rename,
// the checkpoint is on disk but t.journal still says false — and Rollback's error
// handler then writes PhaseRollbackFailed from that stale copy, erasing the
// marker. The candidate had been installed, had run, and came back eligible to be
// offered again (#3453).
//
// So the memory follows the bytes: when persistJournal reports the content is
// visible, t.journal advances even though the error propagates. The caller still
// learns durability is unconfirmed; it just cannot un-write what landed.
func (t *Transaction) persistJournalLocked(journal Journal) error {
	journal.UpdatedAt = time.Now().UTC()
	err := persistJournal(activeJournalPath(journal.HomeDir), journal)
	if err == nil {
		t.journal = journal
		t.journalUnconfirmed = false
		return nil
	}
	if errors.Is(err, errJournalVisibleNotDurable) {
		t.journal = journal
		t.journalUnconfirmed = true
	}
	return fmt.Errorf("persist upgrade phase %s: %w", journal.Phase, err)
}

// reaffirmPhaseLocked completes an "already at this phase, nothing to do" fast
// path. It returns nil when that phase is durably settled, and otherwise
// RE-PERSISTS it.
//
// Every such fast path must go through here rather than returning nil on a bare
// phase comparison. persistJournalLocked adopts a write whose bytes are visible
// but whose barrier never closed, because the memory has to follow the bytes —
// and a phase that arrived that way is exactly the kind that must not be reported
// as an established boundary. finishTerminalRollbackFailure says so in as many
// words: only the DURABLE rollback_failed phase is a circuit-breaker that may
// disable the persistent recovery actor. Reporting success from an unconfirmed
// one lets DisableRecoveryJob run over a terminal entry a crash can still lose,
// stranding an earlier rollback phase with no actor left to finish it.
func (t *Transaction) reaffirmPhaseLocked(phase Phase) error {
	if !t.journalUnconfirmed {
		return nil
	}
	return t.persistPhaseLocked(phase)
}
