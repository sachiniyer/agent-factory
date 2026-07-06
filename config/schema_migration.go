package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pelletier/go-toml/v2"
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

// SchemaMigrationPlan describes how one store is detected, migrated, and validated.
type SchemaMigrationPlan struct {
	StoreName      string
	Path           string
	CurrentVersion int
	DetectVersion  SchemaVersionDetector
	Migrators      *SchemaMigrationRegistry
	Validate       SchemaValidator
	Perm           os.FileMode
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
func MigrateSchemaBytes(raw []byte, plan SchemaMigrationPlan) ([]byte, SchemaMigrationResult, error) {
	if err := validateSchemaMigrationPlan(plan); err != nil {
		return nil, SchemaMigrationResult{}, err
	}

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

// LoadAndMigrateSchemaFile reads, migrates, validates, backs up, and atomically
// writes back one schema-versioned file under its file lock.
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
	err := WithFileLock(plan.Path, func() error {
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
		if err := schemaAtomicWriteFile(plan.Path, migrated, perm); err != nil {
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

// DetectTOMLSchemaVersion detects schema_version from a TOML document. Missing
// schema_version is legacy v0.
func DetectTOMLSchemaVersion(raw []byte) (int, error) {
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return LegacySchemaVersion, err
	}
	value, ok := root[SchemaVersionField]
	if !ok {
		return LegacySchemaVersion, nil
	}
	return schemaVersionFromValue(value)
}

func validateSchemaMigrationPlan(plan SchemaMigrationPlan) error {
	if plan.CurrentVersion < 0 {
		return fmt.Errorf("%s: current schema version must be non-negative, got %d",
			describeSchemaStore(plan.StoreName, plan.Path), plan.CurrentVersion)
	}
	if plan.DetectVersion == nil {
		return fmt.Errorf("%s: schema version detector is required", describeSchemaStore(plan.StoreName, plan.Path))
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

var schemaAtomicWriteFile = AtomicWriteFile

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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
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
