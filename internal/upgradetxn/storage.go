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
)

var (
	syncTransactionDirectory = syncDirectory
	removeTransactionFile    = os.Remove
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
		parents, err := snapshotMetadataParents(home, relative)
		if err != nil {
			return nil, err
		}

		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, MetadataSnapshot{Path: relative, Parents: parents})
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
		if err := durableAtomicWriteFile(snapshotPath, data, info.Mode().Perm()); err != nil {
			return nil, fmt.Errorf("snapshot metadata %s: %w", relative, err)
		}
		snapshots = append(snapshots, MetadataSnapshot{
			Path:         relative,
			Existed:      true,
			Mode:         uint32(info.Mode().Perm()),
			SHA256:       digest(data),
			SnapshotPath: snapshotPath,
			Parents:      parents,
		})
	}
	return snapshots, nil
}

func snapshotMetadataParents(home, relative string) ([]MetadataParentSnapshot, error) {
	paths := metadataParentPaths(relative)
	parents := make([]MetadataParentSnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(filepath.Join(home, path))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inspect metadata parent %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("metadata parent %s is not a directory", path)
		}
		parents = append(parents, MetadataParentSnapshot{Path: path, Mode: uint32(info.Mode().Perm())})
	}
	return parents, nil
}

func prepareMetadataParents(home string, parents []MetadataParentSnapshot) (int, error) {
	for index, parent := range parents {
		path := filepath.Join(home, parent.Path)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := validateDirectoryNoSymlink(filepath.Dir(path)); err != nil {
				return index, fmt.Errorf("validate parent of %s: %w", parent.Path, err)
			}
			if err := os.Mkdir(path, os.FileMode(parent.Mode)|0o700); err != nil {
				return index, fmt.Errorf("recreate metadata parent %s: %w", parent.Path, err)
			}
			if err := os.Chmod(path, os.FileMode(parent.Mode)|0o700); err != nil {
				return index + 1, fmt.Errorf("make recreated metadata parent %s writable: %w", parent.Path, err)
			}
			if err := syncTransactionDirectory(path); err != nil {
				return index + 1, fmt.Errorf("sync recreated metadata parent %s: %w", parent.Path, err)
			}
			if err := syncTransactionDirectory(filepath.Dir(path)); err != nil {
				return index + 1, fmt.Errorf("sync parent after recreating %s: %w", parent.Path, err)
			}
		} else if err != nil {
			return index, fmt.Errorf("inspect metadata parent %s: %w", parent.Path, err)
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return index, fmt.Errorf("metadata parent %s is not a real directory", parent.Path)
		}
		if err := os.Chmod(path, os.FileMode(parent.Mode)|0o700); err != nil {
			return index + 1, fmt.Errorf("make metadata parent %s writable for restoration: %w", parent.Path, err)
		}
	}
	return len(parents), nil
}

func prepareCandidateMetadataParents(
	home, relative string, snapshotted int,
) ([]MetadataParentSnapshot, error) {
	paths := metadataParentPaths(relative)
	prepared := make([]MetadataParentSnapshot, 0, len(paths)-snapshotted)
	for _, relativePath := range paths[snapshotted:] {
		path := filepath.Join(home, relativePath)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return prepared, fmt.Errorf("inspect candidate-created metadata parent %s: %w", relativePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return prepared, fmt.Errorf("candidate-created metadata parent %s is not a real directory", relativePath)
		}
		parent := MetadataParentSnapshot{Path: relativePath, Mode: uint32(info.Mode().Perm())}
		if err := os.Chmod(path, info.Mode().Perm()|0o700); err != nil {
			return prepared, fmt.Errorf("make candidate-created metadata parent %s writable: %w", relativePath, err)
		}
		prepared = append(prepared, parent)
	}
	return prepared, nil
}

func restoreMetadataParentModes(home string, parents []MetadataParentSnapshot) error {
	var result error
	for index := len(parents) - 1; index >= 0; index-- {
		parent := parents[index]
		path := filepath.Join(home, parent.Path)
		if err := os.Chmod(path, os.FileMode(parent.Mode)); err != nil {
			result = errors.Join(result, fmt.Errorf("restore metadata parent mode %s: %w", parent.Path, err))
			continue
		}
		if err := syncTransactionDirectory(path); err != nil {
			result = errors.Join(result, fmt.Errorf("sync metadata parent mode %s: %w", parent.Path, err))
		}
	}
	return result
}

func restoreMetadataEntry(home string, metadata MetadataSnapshot) (retErr error) {
	prepared, err := prepareMetadataParents(home, metadata.Parents)
	defer func() {
		retErr = errors.Join(retErr, restoreMetadataParentModes(home, metadata.Parents[:prepared]))
	}()
	if err != nil {
		return err
	}
	if !metadata.Existed {
		candidateParents, err := prepareCandidateMetadataParents(home, metadata.Path, len(metadata.Parents))
		defer func() {
			retErr = errors.Join(retErr, restoreMetadataParentModes(home, candidateParents))
		}()
		if err != nil {
			return err
		}
	}
	target := filepath.Join(home, metadata.Path)
	if err := ensureNoSymlinkParents(home, target); err != nil {
		return fmt.Errorf("validate rollback path %s: %w", metadata.Path, err)
	}
	if !metadata.Existed {
		if removeErr := os.Remove(target); removeErr != nil {
			if errors.Is(removeErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("restore absence of %s: %w", metadata.Path, removeErr)
		}
		if err := syncTransactionDirectory(filepath.Dir(target)); err != nil {
			return fmt.Errorf("sync restored absence of %s: %w", metadata.Path, err)
		}
		return nil
	}
	data, err := readAndVerify(metadata.SnapshotPath, metadata.SHA256)
	if err != nil {
		return fmt.Errorf("verify metadata snapshot %s: %w", metadata.Path, err)
	}
	if err := durableAtomicWriteFile(target, data, os.FileMode(metadata.Mode)); err != nil {
		return fmt.Errorf("restore metadata %s: %w", metadata.Path, err)
	}
	return nil
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
	if err := validateRecoveryConfiguration(journal.ID, journal.Daemon, journal.RecoveryJob); err != nil {
		return err
	}
	if err := validateTransactionStorage(home, journal); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(journal.Metadata))
	txnDir := transactionDir(home, journal.ID)
	for index, metadata := range journal.Metadata {
		relative, _, err := resolveMetadataPath(home, metadata.Path)
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
		expectedParents := metadataParentPaths(relative)
		if len(metadata.Parents) > len(expectedParents) ||
			(metadata.Existed && len(metadata.Parents) != len(expectedParents)) {
			return fmt.Errorf("metadata snapshot %q has an invalid parent manifest", relative)
		}
		for parentIndex, parent := range metadata.Parents {
			if parent.Path != expectedParents[parentIndex] ||
				parent.Mode == 0 || os.FileMode(parent.Mode)&^os.ModePerm != 0 {
				return fmt.Errorf("metadata snapshot %q has an invalid parent manifest", relative)
			}
		}
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

func validateRecoveryConfiguration(transactionID string, snapshot DaemonSnapshot, job RecoveryJob) error {
	if err := validateRecoveryJob(transactionID, job); err != nil {
		return err
	}
	return validateDaemonRecoveryPair(snapshot, job)
}

func validateDaemonRecoveryPair(snapshot DaemonSnapshot, job RecoveryJob) error {
	expected := RecoveryJobDetached
	if snapshot.WasRunning {
		switch snapshot.Owner.Kind {
		case SupervisionAdHoc:
			expected = RecoveryJobDetached
		case SupervisionSystemd:
			expected = RecoveryJobSystemd
		case SupervisionLaunchd:
			expected = RecoveryJobLaunchd
		}
	}
	if job.Kind != expected {
		return fmt.Errorf("daemon owner %q requires recovery job kind %q, got %q",
			snapshot.Owner.Kind, expected, job.Kind)
	}
	return nil
}

func validateTransactionStorage(home string, journal Journal) error {
	for _, path := range []string{upgradeRoot(home), filepath.Join(upgradeRoot(home), "transactions")} {
		if err := validateDirectoryNoSymlink(path); err != nil {
			return fmt.Errorf("validate upgrade storage %s: %w", path, err)
		}
	}
	txnDir := transactionDir(home, journal.ID)
	err := validateDirectoryNoSymlink(txnDir)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) && terminalPhase(journal.Phase) {
		return nil
	}
	return fmt.Errorf("validate upgrade transaction directory %s: %w", txnDir, err)
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

func metadataParentPaths(relative string) []string {
	dir := filepath.Dir(relative)
	if dir == "." {
		return nil
	}
	components := strings.Split(dir, string(filepath.Separator))
	paths := make([]string, 0, len(components))
	current := ""
	for _, component := range components {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths
}

func terminalPhase(phase Phase) bool {
	return phase == PhaseCommitted || phase == PhaseRolledBack || phase == PhaseAborted
}

func validateDirectoryNoSymlink(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(path) {
		return errors.New("directory path contains a symlink")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path is not a real directory")
	}
	return nil
}

func ensureDurableDirectory(parent, path string, mode os.FileMode) error {
	if filepath.Dir(path) != parent {
		return fmt.Errorf("directory %s is not an immediate child of %s", path, parent)
	}
	err := validateDirectoryNoSymlink(path)
	if err == nil {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("secure directory %s: %w", path, err)
		}
		return syncTransactionDirectory(path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return createDurableDirectory(parent, path, mode)
}

func createDurableDirectory(parent, path string, mode os.FileMode) error {
	if filepath.Dir(path) != parent {
		return fmt.Errorf("directory %s is not an immediate child of %s", path, parent)
	}
	if err := validateDirectoryNoSymlink(parent); err != nil {
		return fmt.Errorf("validate parent directory %s: %w", parent, err)
	}
	if err := os.Mkdir(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("secure new directory %s: %w", path, err)
	}
	if err := syncTransactionDirectory(path); err != nil {
		return fmt.Errorf("sync new directory %s: %w", path, err)
	}
	if err := syncTransactionDirectory(parent); err != nil {
		return fmt.Errorf("sync parent directory %s after creating %s: %w", parent, path, err)
	}
	return nil
}

func resolveMetadataPath(home, path string) (string, string, error) {
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
	return relative, target, nil
}

func validateMetadataPath(home, path string) (string, string, error) {
	relative, target, err := resolveMetadataPath(home, path)
	if err != nil {
		return "", "", err
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
	return durableAtomicWriteFile(path, data, journalFileMode)
}

func durableAtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := validateDirectoryNoSymlink(dir); err != nil {
		return fmt.Errorf("validate durable write directory %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create durable temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write durable temporary file: %w", err)
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set durable temporary file mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync durable temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close durable temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install durable file %s: %w", path, err)
	}
	renamed = true
	if err := syncTransactionDirectory(dir); err != nil {
		return fmt.Errorf("sync durable file directory %s: %w", dir, err)
	}
	return nil
}

func removeDurableFile(path string) error {
	if err := validateDirectoryNoSymlink(filepath.Dir(path)); err != nil {
		return fmt.Errorf("validate file removal directory: %w", err)
	}
	if err := removeTransactionFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncTransactionDirectory(filepath.Dir(path))
}

func removeRequiredDurableFile(path string) error {
	if err := validateDirectoryNoSymlink(filepath.Dir(path)); err != nil {
		return fmt.Errorf("validate required file removal directory: %w", err)
	}
	if err := removeTransactionFile(path); err != nil {
		return err
	}
	return syncTransactionDirectory(filepath.Dir(path))
}

func removeDurableTree(path string) error {
	parent := filepath.Dir(path)
	if err := validateDirectoryNoSymlink(parent); err != nil {
		return fmt.Errorf("validate tree removal parent: %w", err)
	}
	if err := validateDirectoryNoSymlink(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("validate tree removal target: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncTransactionDirectory(parent)
}

// cleanup keeps both the flock path and previous-binary actor until active.json
// is durably gone. After that authority marker disappears, leftover artifacts
// are inert and cleanup is best-effort across process loss.
func (t *Transaction) cleanup() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !terminalPhase(t.journal.Phase) {
		return fmt.Errorf("cannot clean up upgrade in phase %s", t.journal.Phase)
	}

	current, err := readJournal(activeJournalPath(t.journal.HomeDir))
	if errors.Is(err, ErrNoActiveTransaction) {
		return t.cleanupInactiveArtifacts()
	}
	if err != nil {
		return err
	}
	if err := validateJournal(t.journal.HomeDir, current); err != nil {
		return fmt.Errorf("validate journal before cleanup: %w", err)
	}
	if current.ID != t.journal.ID || current.Phase != t.journal.Phase {
		return errors.New("active upgrade transaction changed before cleanup")
	}
	if err := removeDurableFile(t.journal.CandidatePath); err != nil {
		return fmt.Errorf("remove candidate upgrade artifact: %w", err)
	}
	activePath := activeJournalPath(t.journal.HomeDir)
	if err := removeRequiredDurableFile(activePath); err != nil {
		return fmt.Errorf("remove active upgrade journal: %w", err)
	}
	return t.cleanupInactiveArtifacts()
}

func (t *Transaction) cleanupInactiveArtifacts() error {
	if err := removeDurableFile(t.journal.CandidatePath); err != nil {
		return fmt.Errorf("remove inactive candidate artifact: %w", err)
	}
	if err := removeDurableTree(transactionDir(t.journal.HomeDir, t.journal.ID)); err != nil {
		return fmt.Errorf("remove inactive transaction directory: %w", err)
	}
	if err := removeDurableFile(t.journal.PreviousBinaryPath); err != nil {
		return fmt.Errorf("remove previous-binary cleanup actor: %w", err)
	}
	return nil
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
