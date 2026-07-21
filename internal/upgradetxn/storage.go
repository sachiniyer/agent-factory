package upgradetxn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
)

func snapshotMetadata(home, txnDir string, paths []string) ([]MetadataSnapshot, error) {
	seen := make(map[string]struct{}, len(paths))
	snapshots := make([]MetadataSnapshot, 0, len(paths))
	metadataDir := filepath.Join(txnDir, "metadata")
	for index, path := range paths {
		relative, target, err := validateMetadataPath(home, path)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[relative]; exists {
			return nil, fmt.Errorf("metadata path %q is listed more than once", relative)
		}
		seen[relative] = struct{}{}

		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, MetadataSnapshot{Path: relative})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect metadata %s: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("metadata %s is not a regular file", relative)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return nil, fmt.Errorf("read metadata %s: %w", relative, err)
		}
		snapshotPath := filepath.Join(metadataDir, fmt.Sprintf("%04d.snapshot", index))
		if err := config.AtomicWriteFile(snapshotPath, data, info.Mode().Perm()); err != nil {
			return nil, fmt.Errorf("snapshot metadata %s: %w", relative, err)
		}
		snapshots = append(snapshots, MetadataSnapshot{
			Path:         relative,
			Existed:      true,
			Mode:         uint32(info.Mode().Perm()),
			SHA256:       digest(data),
			SnapshotPath: snapshotPath,
		})
	}
	return snapshots, nil
}

func validateJournal(home string, journal Journal) error {
	if journal.SchemaVersion != journalSchemaVersion {
		return fmt.Errorf("unsupported upgrade journal schema %d", journal.SchemaVersion)
	}
	if err := validateTransactionID(journal.ID); err != nil {
		return err
	}
	if journal.HomeDir != home {
		return fmt.Errorf("journal home %q does not match %q", journal.HomeDir, home)
	}
	if !filepath.IsAbs(journal.ExecutablePath) || filepath.Clean(journal.ExecutablePath) != journal.ExecutablePath {
		return errors.New("journal executable path is not canonical")
	}
	previousPath, candidatePath := binaryArtifactPaths(journal.ExecutablePath, journal.ID)
	if journal.PreviousBinaryPath != previousPath || journal.CandidatePath != candidatePath {
		return errors.New("journal binary artifact paths do not match the transaction")
	}
	if !validDigest(journal.PreviousBinarySHA256) || !validDigest(journal.CandidateSHA256) {
		return errors.New("journal contains an invalid binary digest")
	}
	if len(journal.RecoveryNonce) != recoveryNonceBytes*2 {
		return errors.New("journal contains an invalid recovery nonce")
	}
	if _, err := hex.DecodeString(journal.RecoveryNonce); err != nil {
		return errors.New("journal contains an invalid recovery nonce")
	}
	if journal.ExecutableMode == 0 || os.FileMode(journal.ExecutableMode)&^os.ModePerm != 0 {
		return errors.New("journal contains an invalid executable mode")
	}
	if !validPhase(journal.Phase) {
		return fmt.Errorf("journal contains invalid phase %q", journal.Phase)
	}
	if journal.RollbackProgress.MetadataRestored < 0 ||
		journal.RollbackProgress.MetadataRestored > len(journal.Metadata) {
		return errors.New("journal contains invalid rollback metadata progress")
	}
	if !journal.RollbackProgress.BinaryRestored && journal.RollbackProgress.MetadataRestored != 0 {
		return errors.New("journal restores metadata only after the previous binary")
	}
	if err := validateDaemonSnapshot(journal.Daemon); err != nil {
		return err
	}
	if err := validateRecoveryJob(journal.ID, journal.RecoveryJob); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(journal.Metadata))
	txnDir := transactionDir(home, journal.ID)
	for index, metadata := range journal.Metadata {
		relative, _, err := validateMetadataPath(home, metadata.Path)
		if err != nil {
			return err
		}
		if relative != metadata.Path {
			return fmt.Errorf("metadata path %q is not canonical", metadata.Path)
		}
		if _, exists := seen[relative]; exists {
			return fmt.Errorf("metadata path %q is duplicated", relative)
		}
		seen[relative] = struct{}{}
		if !metadata.Existed {
			if metadata.Mode != 0 || metadata.SHA256 != "" || metadata.SnapshotPath != "" {
				return fmt.Errorf("absent metadata %q has snapshot data", relative)
			}
			continue
		}
		expectedSnapshot := filepath.Join(txnDir, "metadata", fmt.Sprintf("%04d.snapshot", index))
		if metadata.SnapshotPath != expectedSnapshot || !validDigest(metadata.SHA256) {
			return fmt.Errorf("metadata snapshot %q does not match the transaction", relative)
		}
		if metadata.Mode == 0 || os.FileMode(metadata.Mode)&^os.ModePerm != 0 {
			return fmt.Errorf("metadata snapshot %q has invalid mode", relative)
		}
	}
	return nil
}

func validateDaemonSnapshot(snapshot DaemonSnapshot) error {
	if !snapshot.WasRunning {
		if snapshot != (DaemonSnapshot{}) {
			return errors.New("journal records daemon details when no daemon was running")
		}
		return nil
	}
	if strings.TrimSpace(snapshot.BootID) == "" {
		return errors.New("journal omits the previous daemon boot identity")
	}
	switch snapshot.Owner.Kind {
	case SupervisionAdHoc:
		if snapshot.Owner.ServiceName != "" {
			return errors.New("ad-hoc daemon owner cannot name a service")
		}
	case SupervisionSystemd, SupervisionLaunchd:
		if strings.TrimSpace(snapshot.Owner.ServiceName) == "" {
			return errors.New("service-managed daemon owner omits its service name")
		}
	default:
		return fmt.Errorf("journal contains invalid daemon supervision kind %q", snapshot.Owner.Kind)
	}
	listeners := snapshot.Listeners
	if !listeners.TCPConfigured {
		if listeners.TCPListenAddr != "" || listeners.TCPBound {
			return errors.New("journal records a TCP listener without TCP configuration")
		}
	} else if strings.TrimSpace(listeners.TCPListenAddr) == "" {
		return errors.New("journal omits the configured TCP listen address")
	}
	return nil
}

// NewRecoveryJob derives the only accepted identity for a transaction. Keeping
// this constructor beside journal validation prevents the daemon-side unit
// renderer and the previous-binary cleanup path from inventing two subtly
// different names.
func NewRecoveryJob(kind RecoveryJobKind, transactionID, unitDir string) (RecoveryJob, error) {
	if err := validateTransactionID(transactionID); err != nil {
		return RecoveryJob{}, err
	}
	job := RecoveryJob{Kind: kind}
	switch kind {
	case RecoveryJobDetached:
		if unitDir != "" {
			return RecoveryJob{}, errors.New("detached recovery job cannot have a unit directory")
		}
	case RecoveryJobSystemd:
		job.Name = "agent-factory-upgrade-recovery-" + transactionID + ".service"
		job.UnitPath = filepath.Join(unitDir, job.Name)
	case RecoveryJobLaunchd:
		job.Name = "com.agent-factory.upgrade-recovery." + transactionID
		job.UnitPath = filepath.Join(unitDir, job.Name+".plist")
	default:
		return RecoveryJob{}, fmt.Errorf("unsupported upgrade recovery job kind %q", kind)
	}
	if err := validateRecoveryJob(transactionID, job); err != nil {
		return RecoveryJob{}, err
	}
	return job, nil
}

func validateRecoveryJob(transactionID string, job RecoveryJob) error {
	switch job.Kind {
	case RecoveryJobDetached:
		if job.Name != "" || job.UnitPath != "" {
			return errors.New("detached recovery job cannot name a service or unit path")
		}
		return nil
	case RecoveryJobSystemd, RecoveryJobLaunchd:
		if job.Name == "" || !filepath.IsAbs(job.UnitPath) || filepath.Clean(job.UnitPath) != job.UnitPath {
			return errors.New("persistent recovery job requires an absolute canonical unit path")
		}
		expectedName := "agent-factory-upgrade-recovery-" + transactionID + ".service"
		expectedBase := expectedName
		if job.Kind == RecoveryJobLaunchd {
			expectedName = "com.agent-factory.upgrade-recovery." + transactionID
			expectedBase = expectedName + ".plist"
		}
		if job.Name != expectedName || filepath.Base(job.UnitPath) != expectedBase {
			return errors.New("recovery job identity does not match the transaction")
		}
		return nil
	default:
		return fmt.Errorf("journal contains invalid recovery job kind %q", job.Kind)
	}
}

func validPhase(phase Phase) bool {
	switch phase {
	case PhasePrepared, PhaseSupervisorReady, PhaseDaemonStopped,
		PhaseCandidateInstalled, PhaseCandidateStarting, PhaseCandidateValidating,
		PhaseCommitted, PhaseAborted, PhaseRollingBack, PhaseRollbackRestored,
		PhasePreviousStarting, PhasePreviousValidating, PhaseRolledBack, PhaseRollbackFailed:
		return true
	default:
		return false
	}
}

func validateTransactionID(id string) error {
	if !transactionIDPattern.MatchString(id) || id == "." || id == ".." {
		return fmt.Errorf("invalid upgrade transaction ID %q", id)
	}
	return nil
}

func validateMetadataPath(home, path string) (string, string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", "", fmt.Errorf("metadata path %q must be relative to the upgrade home", path)
	}
	relative := filepath.Clean(path)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("metadata path %q escapes the upgrade home", path)
	}
	target := filepath.Join(home, relative)
	inside, err := filepath.Rel(home, target)
	if err != nil || inside != relative {
		return "", "", fmt.Errorf("metadata path %q escapes the upgrade home", path)
	}
	if err := ensureNoSymlinkParents(home, target); err != nil {
		return "", "", fmt.Errorf("metadata path %q is unsafe: %w", path, err)
	}
	return relative, target, nil
}

func ensureNoSymlinkParents(home, target string) error {
	relative, err := filepath.Rel(home, filepath.Dir(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("parent escapes the upgrade home")
	}
	current := home
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %s is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %s is not a directory", current)
		}
	}
	return nil
}

func canonicalExistingDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is blank")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

func canonicalExistingFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is blank")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func upgradeRoot(home string) string {
	return filepath.Join(home, "upgrade")
}

func activeJournalPath(home string) string {
	return filepath.Join(upgradeRoot(home), "active.json")
}

func transactionDir(home, id string) string {
	return filepath.Join(upgradeRoot(home), "transactions", id)
}

func binaryArtifactPaths(executable, id string) (string, string) {
	dir := filepath.Dir(executable)
	base := filepath.Base(executable)
	prefix := "." + base + ".af-upgrade-" + id
	return filepath.Join(dir, prefix+".previous"), filepath.Join(dir, prefix+".candidate")
}

func persistJournal(path string, journal Journal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade journal: %w", err)
	}
	data = append(data, '\n')
	return config.AtomicWriteFile(path, data, journalFileMode)
}

func readJournal(path string) (Journal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Journal{}, ErrNoActiveTransaction
	}
	if err != nil {
		return Journal{}, fmt.Errorf("read active upgrade journal: %w", err)
	}
	var journal Journal
	if err := json.Unmarshal(data, &journal); err != nil {
		return Journal{}, fmt.Errorf("decode active upgrade journal: %w", err)
	}
	return journal, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readAndVerify(path, expectedDigest string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if digest(data) != expectedDigest {
		return nil, fmt.Errorf("digest mismatch for %s", path)
	}
	return data, nil
}

func acquireFileLock(path string, nonblocking bool) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, journalFileMode)
	if err != nil {
		return nil, err
	}
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrRecoveryActive
		}
		return nil, err
	}
	return file, nil
}

func releaseFileLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func processStartIdentity() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	closingParen := strings.LastIndexByte(string(data), ')')
	if closingParen < 0 || closingParen+1 >= len(data) {
		return ""
	}
	fields := strings.Fields(string(data[closingParen+1:]))
	// The suffix starts with field 3 (state), so field 22 (starttime) is
	// suffix index 19.
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func kernelBootID() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
