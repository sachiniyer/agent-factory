package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// SchemaVersionField is the canonical on-disk key every versioned store uses.
	SchemaVersionField = "schema_version"
	// LegacySchemaVersion is the implicit version for stores without a
	// schema_version field. Existing array-root stores also detect as legacy.
	LegacySchemaVersion = 0
)

// SchemaVersionDetector extracts the schema version from raw file bytes.
// Missing schema_version must return LegacySchemaVersion.
type SchemaVersionDetector func(raw []byte) (int, error)

// SchemaMigrator upgrades raw bytes from one schema version to the next.
type SchemaMigrator func(raw []byte) ([]byte, error)

// SchemaValidator validates the fully-migrated raw bytes before writeback.
type SchemaValidator func(raw []byte) error

// SchemaMigrationRegistry stores one forward migrator per source version.
type SchemaMigrationRegistry struct {
	migrators map[int]SchemaMigrator
}

// NewSchemaMigrationRegistry returns an empty migrator registry.
func NewSchemaMigrationRegistry() *SchemaMigrationRegistry {
	return &SchemaMigrationRegistry{migrators: make(map[int]SchemaMigrator)}
}

// Register adds a v->v+1 migrator for fromVersion.
func (r *SchemaMigrationRegistry) Register(fromVersion int, migrator SchemaMigrator) error {
	if r == nil {
		return fmt.Errorf("schema migration registry is nil")
	}
	if fromVersion < 0 {
		return fmt.Errorf("schema migrator source version must be non-negative, got %d", fromVersion)
	}
	if migrator == nil {
		return fmt.Errorf("schema migrator for version %d is nil", fromVersion)
	}
	if r.migrators == nil {
		r.migrators = make(map[int]SchemaMigrator)
	}
	if _, exists := r.migrators[fromVersion]; exists {
		return fmt.Errorf("schema migrator for version %d is already registered", fromVersion)
	}
	r.migrators[fromVersion] = migrator
	return nil
}

func (r *SchemaMigrationRegistry) migrator(fromVersion int) (SchemaMigrator, bool) {
	if r == nil {
		return nil, false
	}
	migrator, ok := r.migrators[fromVersion]
	return migrator, ok
}

// SchemaWriteLinkPolicy decides what the migration write-back does when the
// store's path is a symlink. It is the plan's answer to #3672's per-caller
// question, carried to the one place that answers it for a store the caller
// does not otherwise write: LoadAndMigrateSchemaFile.
//
// It exists because the two stores that migrate through this helper need
// DIFFERENT answers. tasks.json is user-authored content a user may keep in
// dotfiles, so its ordinary writes follow a link (#3672); instances.json is
// af-managed, so its writes do not. One shared writer here would have to be
// wrong for one of them, and it was: the write-back replaced a linked
// tasks.json that every other write to the same file preserved (#3718).
type SchemaWriteLinkPolicy int

const (
	// SchemaWriteReplaceLink replaces a symlink at the store's path with the
	// migrated regular file. It is the ZERO VALUE on purpose: following a link
	// is a promise a store has to ask for, never one it inherits by omission
	// (#3672), and replacing is what every store did before this field existed.
	SchemaWriteReplaceLink SchemaWriteLinkPolicy = iota
	// SchemaWriteFollowLink rewrites the file the link names and leaves the link
	// in place. Only for a store whose ORDINARY writes already follow, so the
	// migration cannot disagree with them about the same file.
	SchemaWriteFollowLink
)

func (p SchemaWriteLinkPolicy) valid() bool {
	return p == SchemaWriteReplaceLink || p == SchemaWriteFollowLink
}

// SchemaMigrationPlan describes how one store is detected, migrated, and validated.
type SchemaMigrationPlan struct {
	StoreName      string
	Path           string
	CurrentVersion int
	DetectVersion  SchemaVersionDetector
	Migrators      *SchemaMigrationRegistry
	Validate       SchemaValidator
	Perm           os.FileMode

	// ProveCurrentVersion is an optional fast path for the overwhelmingly
	// common case: bytes that are already at CurrentVersion and have nothing to
	// migrate. It answers one question cheaply — are these bytes provably at
	// currentVersion? — and MigrateSchemaBytes uses it to skip DetectVersion and
	// the migrator chain when the answer is yes.
	//
	// The contract is one-directional. A true answer MUST mean
	// DetectVersion(raw) would return (currentVersion, nil); a false answer
	// promises nothing and costs only the full path, which runs anyway. Probes
	// are therefore written to refuse whenever they are unsure.
	//
	// currentVersion is passed rather than closed over so a plan cannot wire a
	// probe that proves a version this plan is not on.
	//
	// Every store in the tree detects its version the same way — a top-level
	// schema_version field, DetectJSONSchemaVersion — and so shares one probe,
	// ProveJSONSchemaVersion. TestFastPathMatchesFullPlanOverInstancesFixtures
	// enforces the contract for the instances plan over every fixture in
	// config/testdata/instances, so a migration step added later cannot make the
	// fast path lie without turning that test red.
	ProveCurrentVersion func(raw []byte, currentVersion int) bool

	// LinkPolicy is what the write-back does with a symlink at Path. Unset
	// means SchemaWriteReplaceLink; see the type's comment for why that is the
	// zero value rather than something a plan must state.
	LinkPolicy SchemaWriteLinkPolicy
}

// SchemaMigrationResult reports what happened during a migration attempt.
type SchemaMigrationResult struct {
	OriginalVersion int
	FinalVersion    int
	Migrated        bool
	BackupPath      string
}

// UnsupportedSchemaVersionError is returned when a file was written by a newer
// binary than the current one knows how to read.
type UnsupportedSchemaVersionError struct {
	StoreName        string
	Path             string
	FileVersion      int
	SupportedVersion int
}

func (e *UnsupportedSchemaVersionError) Error() string {
	return fmt.Sprintf("%s has schema_version %d, but this binary supports up to %d; upgrade af before using this state file",
		describeSchemaStore(e.StoreName, e.Path), e.FileVersion, e.SupportedVersion)
}

// MigrateSchemaBytes upgrades raw bytes to plan.CurrentVersion and validates
// the result. It does not write files; callers that own a disk store should use
// LoadAndMigrateSchemaFile.
//
// On the fast path the returned slice ALIASES raw rather than copying it — a
// 1.36 MB copy per read on this repo's largest instances.json — so callers must
// treat it as read-only. Every caller in the tree only unmarshals it.
func MigrateSchemaBytes(raw []byte, plan SchemaMigrationPlan) ([]byte, SchemaMigrationResult, error) {
	if err := validateSchemaMigrationPlan(plan); err != nil {
		return nil, SchemaMigrationResult{}, err
	}
	if plan.ProveCurrentVersion != nil && plan.ProveCurrentVersion(raw, plan.CurrentVersion) {
		return migrateSchemaBytesAlreadyCurrent(raw, plan)
	}
	return migrateSchemaBytesFullPlan(raw, plan)
}

// migrateSchemaBytesAlreadyCurrent is the fast path: the probe has proved raw is
// at plan.CurrentVersion, so no migrator can run and the migrated bytes are the
// input bytes. Only validation is left, and it is deliberately NOT skipped —
// dropping it would change what MigrateSchemaBytes reports for a file that is at
// the current version but holds content the store cannot use, which is a
// behavior change rather than an optimization.
func migrateSchemaBytesAlreadyCurrent(raw []byte, plan SchemaMigrationPlan) ([]byte, SchemaMigrationResult, error) {
	result := SchemaMigrationResult{
		OriginalVersion: plan.CurrentVersion,
		FinalVersion:    plan.CurrentVersion,
	}
	if plan.Validate != nil {
		if err := plan.Validate(raw); err != nil {
			return nil, result, fmt.Errorf("%s: validate schema version %d: %w",
				describeSchemaStore(plan.StoreName, plan.Path), result.FinalVersion, err)
		}
	}
	return raw, result, nil
}

// migrateSchemaBytesFullPlan runs the plan end to end: detect, migrate forward
// one version at a time, validate. It stays a named function so the fast path
// can be tested against it directly.
func migrateSchemaBytesFullPlan(raw []byte, plan SchemaMigrationPlan) ([]byte, SchemaMigrationResult, error) {
	version, err := plan.DetectVersion(raw)
	if err != nil {
		return nil, SchemaMigrationResult{}, fmt.Errorf("%s: detect schema version: %w", describeSchemaStore(plan.StoreName, plan.Path), err)
	}
	if version > plan.CurrentVersion {
		return nil, SchemaMigrationResult{}, &UnsupportedSchemaVersionError{
			StoreName:        plan.StoreName,
			Path:             plan.Path,
			FileVersion:      version,
			SupportedVersion: plan.CurrentVersion,
		}
	}

	result := SchemaMigrationResult{
		OriginalVersion: version,
		FinalVersion:    version,
	}
	upgraded := append([]byte(nil), raw...)
	for version < plan.CurrentVersion {
		migrator, ok := plan.Migrators.migrator(version)
		if !ok {
			return nil, result, fmt.Errorf("%s: no schema migrator registered for version %d -> %d",
				describeSchemaStore(plan.StoreName, plan.Path), version, version+1)
		}
		nextRaw, err := migrator(upgraded)
		if err != nil {
			return nil, result, fmt.Errorf("%s: migrate schema version %d -> %d: %w",
				describeSchemaStore(plan.StoreName, plan.Path), version, version+1, err)
		}
		nextVersion, err := plan.DetectVersion(nextRaw)
		if err != nil {
			return nil, result, fmt.Errorf("%s: detect migrated schema version after %d -> %d: %w",
				describeSchemaStore(plan.StoreName, plan.Path), version, version+1, err)
		}
		if nextVersion != version+1 {
			return nil, result, fmt.Errorf("%s: migrator for version %d produced schema_version %d, want %d",
				describeSchemaStore(plan.StoreName, plan.Path), version, nextVersion, version+1)
		}
		upgraded = nextRaw
		version = nextVersion
		result.FinalVersion = version
	}

	if plan.Validate != nil {
		if err := plan.Validate(upgraded); err != nil {
			return nil, result, fmt.Errorf("%s: validate schema version %d: %w",
				describeSchemaStore(plan.StoreName, plan.Path), result.FinalVersion, err)
		}
	}
	result.Migrated = result.OriginalVersion != result.FinalVersion
	return upgraded, result, nil
}

// SchemaMigrationLockTimeout bounds how long LoadAndMigrateSchemaFile waits for a
// schema-versioned file's flock. A var so tests can shorten it; production never
// reassigns.
//
// This is the most dangerous unbounded flock in the tree, because of WHERE it is
// called from rather than what it does: the daemon reaches it on every instance
// refresh (refreshDaemonInstances -> MigrateAllRepoInstancesForDaemonLoad) while
// holding the manager's global lock, and a session kill reaches that refresh
// before it has done anything else. Parking here therefore does not stall one
// migration — it freezes the whole daemon, including the RPC that would report
// the problem and the shutdown that would clear it (#1917: a wedged daemon also
// failed to exit on SIGTERM and had to be SIGKILLed).
//
// The lock is held across a read + in-place migrate + atomic write of one small
// file, so a wait beyond this budget means a peer is wedged, not slow.
var SchemaMigrationLockTimeout = 10 * time.Second

// LoadAndMigrateSchemaFile reads, migrates, validates, backs up, and atomically
// writes back one schema-versioned file under its file lock. The lock is taken
// with a DEADLINE (see SchemaMigrationLockTimeout): contention surfaces as a
// retryable ErrLockTimeout instead of freezing the caller — which, on the daemon's
// refresh path, means freezing the daemon.
func LoadAndMigrateSchemaFile(plan SchemaMigrationPlan) ([]byte, SchemaMigrationResult, error) {
	if plan.Path == "" {
		return nil, SchemaMigrationResult{}, fmt.Errorf("schema migration path is required")
	}
	perm := plan.Perm
	if perm == 0 {
		perm = 0644
	}

	var migrated []byte
	var result SchemaMigrationResult
	err := WithFileLockTimeout(plan.Path, SchemaMigrationLockTimeout, func() error {
		raw, err := os.ReadFile(plan.Path)
		if err != nil {
			return fmt.Errorf("%s: read: %w", describeSchemaStore(plan.StoreName, plan.Path), err)
		}
		migrated, result, err = MigrateSchemaBytes(raw, plan)
		if err != nil {
			return err
		}
		if !result.Migrated {
			return nil
		}
		backupPath, err := writeSchemaMigrationBackup(plan.Path, raw, result.OriginalVersion, perm)
		if err != nil {
			return fmt.Errorf("%s: back up pre-migration file: %w", describeSchemaStore(plan.StoreName, plan.Path), err)
		}
		result.BackupPath = backupPath
		if err := schemaAtomicWriteFile(plan.LinkPolicy, plan.Path, migrated, perm); err != nil {
			return fmt.Errorf("%s: write migrated schema version %d: %w",
				describeSchemaStore(plan.StoreName, plan.Path), result.FinalVersion, err)
		}
		return nil
	})
	return migrated, result, err
}

// DetectJSONSchemaVersion detects schema_version from a JSON object. A JSON
// array has no place for the field and is treated as legacy v0.
func DetectJSONSchemaVersion(raw []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return LegacySchemaVersion, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return LegacySchemaVersion, fmt.Errorf("trailing data after JSON value")
	}
	switch typed := root.(type) {
	case map[string]any:
		value, ok := typed[SchemaVersionField]
		if !ok {
			return LegacySchemaVersion, nil
		}
		return schemaVersionFromValue(value)
	case []any:
		return LegacySchemaVersion, nil
	default:
		return LegacySchemaVersion, fmt.Errorf("JSON root must be an object or array, got %T", root)
	}
}

func validateSchemaMigrationPlan(plan SchemaMigrationPlan) error {
	if plan.CurrentVersion < 0 {
		return fmt.Errorf("%s: current schema version must be non-negative, got %d",
			describeSchemaStore(plan.StoreName, plan.Path), plan.CurrentVersion)
	}
	if plan.DetectVersion == nil {
		return fmt.Errorf("%s: schema version detector is required", describeSchemaStore(plan.StoreName, plan.Path))
	}
	// Rejected HERE, before LoadAndMigrateSchemaFile has read, backed up, or
	// written anything. A policy nobody recognises means the caller does not
	// know what it is asking for, and the safe moment to say so is the one
	// where the file is still untouched.
	if !plan.LinkPolicy.valid() {
		return fmt.Errorf("%s: unknown schema write link policy %d",
			describeSchemaStore(plan.StoreName, plan.Path), plan.LinkPolicy)
	}
	return nil
}

func schemaVersionFromValue(value any) (int, error) {
	switch typed := value.(type) {
	case json.Number:
		return schemaVersionFromString(typed.String())
	case int:
		return checkedSchemaVersion(typed)
	case int64:
		return schemaVersionFromString(strconv.FormatInt(typed, 10))
	case int32:
		return schemaVersionFromString(strconv.FormatInt(int64(typed), 10))
	case int16:
		return schemaVersionFromString(strconv.FormatInt(int64(typed), 10))
	case int8:
		return schemaVersionFromString(strconv.FormatInt(int64(typed), 10))
	default:
		return LegacySchemaVersion, fmt.Errorf("%s must be an integer, got %T", SchemaVersionField, value)
	}
}

func schemaVersionFromString(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return LegacySchemaVersion, fmt.Errorf("%s must be an integer that fits in int: %w", SchemaVersionField, err)
	}
	return checkedSchemaVersion(n)
}

func checkedSchemaVersion(value int) (int, error) {
	if value < 0 {
		return LegacySchemaVersion, fmt.Errorf("%s must be non-negative, got %d", SchemaVersionField, value)
	}
	return value, nil
}

// schemaAtomicWriteFile is the migration write-back, and the seam tests inject
// at. It takes the POLICY rather than being chosen by it so there is one hook
// covering both answers: a second var per writer would let a test override the
// replacing path and leave the following one running for real, which is the
// shape of a hook that stops covering what it claims to.
var schemaAtomicWriteFile = writeMigratedSchemaFile

// writeMigratedSchemaFile puts the migrated bytes back under the plan's link
// policy.
//
// Both branches take the lock LoadAndMigrateSchemaFile already holds, which is
// on the plan's OWN path — unresolved, even when the write follows. That is
// deliberate and it is not the pinned-resolution shape #3688 established for
// the global config, for two reasons. The lock here is bounded
// (WithFileLockTimeout) because an unbounded wait on this path freezes the
// daemon (#1917), and withFollowedFileLock blocks forever. And the follow-side
// store's ORDINARY writes lock their own path too (task/task.go, #3672): a
// write-back that resolved the lock while writeTasks did not would put the two
// writers to one tasks.json behind two different .lock files, which is strictly
// worse than both being unresolved. The cross-AF-home aliasing that leaves open
// is the class #3697 owns, for both writers at once.
func writeMigratedSchemaFile(policy SchemaWriteLinkPolicy, path string, data []byte, perm os.FileMode) error {
	switch policy {
	case SchemaWriteReplaceLink:
		return AtomicWriteFile(path, data, perm)
	case SchemaWriteFollowLink:
		return AtomicWriteFileFollowingLink(path, data, perm)
	default:
		// validateSchemaMigrationPlan rejects this before the file is touched,
		// so reaching it means a caller built the plan some other way. Fail
		// closed rather than silently picking a policy for them.
		return fmt.Errorf("unknown schema write link policy %d", policy)
	}
}

func writeSchemaMigrationBackup(path string, raw []byte, fromVersion int, perm os.FileMode) (string, error) {
	base := fmt.Sprintf("%s.bak.schema-v%d", path, fromVersion)
	backupPath, err := availableBackupPath(base)
	if err != nil {
		return "", err
	}
	if err := writeFileExclusive(backupPath, raw, perm); err != nil {
		return "", err
	}
	return backupPath, nil
}

func writeFileExclusive(path string, data []byte, perm os.FileMode) error {
	if err := MkdirAllUnderAFHome(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

func describeSchemaStore(name, path string) string {
	switch {
	case name != "" && path != "":
		return fmt.Sprintf("%s %s", name, prettyHomePath(path))
	case name != "":
		return name
	case path != "":
		return prettyHomePath(path)
	default:
		return "state file"
	}
}
