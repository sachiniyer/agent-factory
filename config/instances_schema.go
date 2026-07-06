package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

var errInstancesSchemaContent = errors.New("invalid instances.json schema content")

type instancesEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Instances     json.RawMessage `json:"instances"`
}

// NewInstancesSchemaMigrationPlan returns the v0 array-root -> v1 envelope
// migration plan for a single per-repo instances.json file.
func NewInstancesSchemaMigrationPlan(path string) SchemaMigrationPlan {
	registry := NewSchemaMigrationRegistry()
	if err := registry.Register(LegacySchemaVersion, migrateLegacyInstancesArray); err != nil {
		panic(err)
	}
	return SchemaMigrationPlan{
		StoreName:      InstancesFileName,
		Path:           path,
		CurrentVersion: InstancesSchemaVersion,
		DetectVersion:  detectInstancesSchemaVersion,
		Migrators:      registry,
		Validate:       validateInstancesEnvelope,
		Perm:           0644,
	}
}

// MigrateRepoInstancesForDaemonLoad upgrades one repo's instances.json in
// place. It is intended for daemon-owned load paths; read-only callers should
// use LoadRepoInstances, which tolerates both array-root and envelope formats
// without writing.
func MigrateRepoInstancesForDaemonLoad(repoID string) (SchemaMigrationResult, error) {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return SchemaMigrationResult{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SchemaMigrationResult{}, nil
		}
		return SchemaMigrationResult{}, fmt.Errorf("failed to read repo instances: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return SchemaMigrationResult{}, nil
	}
	_, result, err := LoadAndMigrateSchemaFile(NewInstancesSchemaMigrationPlan(path))
	return result, err
}

// MigrateAllRepoInstancesForDaemonLoad upgrades every readable per-repo
// instances.json before daemon restore/refresh reads it. Corrupted legacy files
// keep the existing skip-and-warn behavior; newer schema versions and write
// failures are returned so the daemon never overwrites a file it cannot safely
// understand or migrate.
func MigrateAllRepoInstancesForDaemonLoad() error {
	dir, err := instancesDirPath()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read instances directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoID := entry.Name()
		if _, err := MigrateRepoInstancesForDaemonLoad(repoID); err != nil {
			var newer *UnsupportedSchemaVersionError
			switch {
			case errors.As(err, &newer):
				return fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, err)
			case errors.Is(err, errInstancesSchemaContent):
				// Preserve the daemon's existing corruption posture: a malformed
				// repo file is skipped and named, not allowed to abort every other
				// repo's restore. The later LoadAllRepoInstances path will log the
				// same repo if it still cannot decode.
				continue
			default:
				return fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, err)
			}
		}
	}
	return nil
}

func migrateInstancesSchemaBytes(raw []byte, path string) ([]byte, SchemaMigrationResult, error) {
	return MigrateSchemaBytes(raw, NewInstancesSchemaMigrationPlan(path))
}

func extractInstancesArray(raw []byte, path string) (json.RawMessage, error) {
	upgraded, _, err := migrateInstancesSchemaBytes(raw, path)
	if err != nil {
		return nil, err
	}
	var envelope instancesEnvelope
	if err := json.Unmarshal(upgraded, &envelope); err != nil {
		return nil, fmt.Errorf("%w: failed to parse instances envelope: %v", errInstancesSchemaContent, err)
	}
	return normalizeJSONRawArray(envelope.Instances, "instances")
}

func loadRepoInstancesForAll(repoID string) (json.RawMessage, error) {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return json.RawMessage("[]"), nil
		}
		return nil, fmt.Errorf("failed to read repo instances: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return json.RawMessage("[]"), nil
	}
	instances, err := extractInstancesArray(data, path)
	if err == nil {
		return instances, nil
	}
	var newer *UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		return nil, err
	}
	// All-repo callers historically decoded each repo's raw bytes themselves
	// so they could aggregate and name corrupted repos (#730). Preserve that
	// behavior even though single-repo reads now unwrap envelopes here.
	return json.RawMessage(data), nil
}

func marshalInstancesEnvelope(data json.RawMessage) ([]byte, error) {
	instances, err := normalizeJSONRawArray(data, "instances")
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(instancesEnvelope{
		SchemaVersion: InstancesSchemaVersion,
		Instances:     instances,
	}, "", "  ")
}

func migrateLegacyInstancesArray(raw []byte) ([]byte, error) {
	return marshalInstancesEnvelope(raw)
}

func validateInstancesEnvelope(raw []byte) error {
	var envelope instancesEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: failed to parse instances envelope: %v", errInstancesSchemaContent, err)
	}
	if envelope.SchemaVersion != InstancesSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", envelope.SchemaVersion, InstancesSchemaVersion)
	}
	if _, err := normalizeJSONRawArray(envelope.Instances, "instances"); err != nil {
		return err
	}
	return nil
}

func detectInstancesSchemaVersion(raw []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return LegacySchemaVersion, nil
	}
	version, err := DetectJSONSchemaVersion(raw)
	if err != nil {
		return LegacySchemaVersion, fmt.Errorf("%w: %v", errInstancesSchemaContent, err)
	}
	return version, nil
}

func normalizeJSONRawArray(raw json.RawMessage, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: %s must be a JSON array", errInstancesSchemaContent, field)
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("[]"), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("%w: %s must be a JSON array: %v", errInstancesSchemaContent, field, err)
	}
	if items == nil {
		return json.RawMessage("[]"), nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s array: %w", field, err)
	}
	return json.RawMessage(out), nil
}
