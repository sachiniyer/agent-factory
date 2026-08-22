package upgradetxn

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Creating a transaction. Split from transaction.go, which keeps the journal
// types and the Transaction lifecycle methods, when that file reached the
// 1000-line limit (#1145).

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

	// Resolve the executable's canonical path before the locks so the
	// executable-keyed lock location is known. This is path resolution only —
	// the byte snapshot below stays inside both locks, which is the part that
	// must not straddle an in-place swap.
	executable, err := canonicalExistingFile(stablePlan.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("validate running executable: %w", err)
	}

	root := upgradeRoot(home)
	if err := ensureDurableDirectory(home, root, transactionDirMode); err != nil {
		return nil, fmt.Errorf("prepare upgrade root: %w", err)
	}

	preparationLock, err := acquireFileLock(filepath.Join(root, preparationLockName), false)
	if err != nil {
		return nil, fmt.Errorf("lock upgrade preparation: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(preparationLock))
	}()

	// The per-home lock above excludes other writers in THIS home; it cannot
	// exclude an `af upgrade` run against a different AGENT_FACTORY_HOME, which
	// would still rename over the same binary. Take the executable-keyed lock
	// too, always after the home lock so two homes racing one binary order their
	// acquisitions identically and cannot deadlock (#2212).
	executableLock, err := acquireExecutableLock(executable, false)
	if err != nil {
		return nil, fmt.Errorf("lock the executable for upgrade: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(executableLock))
	}()

	// Snapshot the running executable under the preparation lock, never before
	// it — see WithInstallLock for why that ordering is load-bearing (#2212).
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
	// The rejected-candidate ledger, read HERE, under the preparation lock (#3043).
	//
	// Every caller checks earlier too, and should — the daemon skips a download and
	// gives a better reason. But an earlier read is a decision about state that can
	// change before it is used: another home sharing this executable can be rolling
	// back the same candidate, record the rejection, and clean up before this
	// transaction reaches Prepare. Its artifacts are gone by then, so the
	// foreign-transaction check cannot see it, and the fingerprint cannot either
	// because a rollback restores the same baseline bytes it started from. Only the
	// ledger remembers, and only a read under this lock is serialised against the
	// write that put it there.
	//
	// Refused rather than overridable: this is the daemon's unattended path, and an
	// operator who genuinely wants disqualified bytes has `af upgrade
	// --allow-rejected`, which does not come through here.
	rejected, entry, err := CandidateRejected(executable, stablePlan.Candidate)
	if err != nil {
		// Fail closed, matching CandidateRejected's contract: "I could not tell" is
		// not "it is fine".
		return nil, fmt.Errorf("cannot read the rejected-candidate ledger for %s: %w", executable, err)
	}
	if rejected {
		return nil, fmt.Errorf(
			"candidate %s is byte-for-byte the build this machine rolled back at %s (%s); refusing to prepare an upgrade to it",
			entry.Version, entry.RejectedAt.Format(time.RFC3339), entry.Reason)
	}
	// The caller's expectation, checked against the bytes actually about to be
	// preserved, under the locks. This is the only place the check means
	// anything: everywhere else it is a time-of-check-to-time-of-use window an
	// in-place install can land in, after which this transaction would preserve
	// the newer binary as its rollback target and install an older candidate
	// over it (#2212).
	if expected := stablePlan.ExpectedPreviousSHA256; expected != "" {
		if actual := digest(previousBinary); actual != expected {
			return nil, fmt.Errorf(
				"the executable changed since the caller last observed it (expected %s, found %s); refusing to replace a binary this upgrade was not planned against",
				expected, actual)
		}
	}
	// A transaction in ANOTHER af home can be staging over this same executable:
	// transactions are home-scoped, the binary is not, and the executable lock
	// serialises publishing rather than the transaction's lifetime. Two active
	// transactions over one binary would race their recovery actors, and one
	// home's commit or rollback would overwrite the other's. Detected the way
	// every home's transaction is visible — by its artifacts, which are staged
	// beside the executable.
	if foreign, err := foreignTransactionOver(executable, stablePlan.ID); err != nil {
		return nil, err
	} else if foreign != "" {
		return nil, fmt.Errorf("another upgrade transaction (%s) is already staging over %s; refusing to start a second one", foreign, executable)
	}

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

	// The artifacts must say who owns them BEFORE they exist, or the window between
	// staging and describing is one where another home reads them as anonymous — and
	// an anonymous artifact blocks forever by design (#2212 gate 4).
	//
	// Written AFTER the collision check above, never before: an id whose artifacts
	// already belong to an earlier attempt or another home must be refused untouched.
	// Writing first would overwrite that owner's record and then delete it on the way
	// out, leaving THEIR binaries anonymous — manufacturing the permanent block this
	// change exists to remove.
	ownerRecord, err := captureArtifactOwner(home, stablePlan.ID)
	if err != nil {
		return nil, err
	}
	ownerPath := artifactOwnerPath(executable, stablePlan.ID)
	if _, err := os.Lstat(ownerPath); err == nil {
		return nil, fmt.Errorf("upgrade artifact owner already exists at %s", ownerPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect upgrade artifact owner %s: %w", ownerPath, err)
	}
	if err := writeArtifactOwner(ownerPath, ownerRecord); err != nil {
		return nil, err
	}
	// Removed in its OWN defer, registered before the binaries are appended to
	// createdArtifacts so it runs after that loop (defers are LIFO). A cleanup that
	// dies partway must leave an owner record with no binaries — which blocks nothing
	// — rather than binaries with no owner, which block this executable forever.
	defer func() {
		if published {
			return
		}
		_ = os.Remove(ownerPath)
	}()

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
	// Hand authority to the journal. From here the journal's ABSENCE means the
	// transaction is over, so a later scan must stop consulting this process's
	// liveness: a daemon that survives a failed cleanup is still running, and gating
	// on it would keep its own inert artifact blocking forever.
	//
	// Written after the journal is durable so the two can never disagree in the
	// dangerous direction — a sidecar claiming the handoff before the journal exists
	// would let a crash mid-publish read as finished.
	//
	// REQUIRED, not best-effort. A silent failure here leaves the sidecar saying
	// pre-handoff while the journal exists, and the pre-handoff rule consults stager
	// liveness — so a later abort that removes active.json but fails to delete
	// .previous would block this executable permanently, which is precisely the defect
	// under repair. Failing loudly here costs one retry; succeeding quietly costs the
	// guarantee.
	ownerRecord.Journalled = true
	if err := writeArtifactOwner(ownerPath, ownerRecord); err != nil {
		if abortErr := (&Transaction{journal: journal}).abort(); abortErr != nil {
			return nil, fmt.Errorf("mark artifact owner journalled: %w (and rolling the transaction back failed: %v)", err, abortErr)
		}
		return nil, fmt.Errorf("mark artifact owner journalled: %w", err)
	}
	return &Transaction{journal: journal}, nil
}
