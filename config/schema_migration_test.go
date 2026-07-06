package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectJSONSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr string
	}{
		{
			name: "object missing field is legacy",
			raw:  `{"name":"alpha"}`,
			want: LegacySchemaVersion,
		},
		{
			name: "array root is legacy",
			raw:  `[{"name":"alpha"}]`,
			want: LegacySchemaVersion,
		},
		{
			name: "object version",
			raw:  `{"schema_version":2}`,
			want: 2,
		},
		{
			name:    "non-integer version",
			raw:     `{"schema_version":"2"}`,
			wantErr: "schema_version must be an integer",
		},
		{
			name:    "negative version",
			raw:     `{"schema_version":-1}`,
			wantErr: "schema_version must be non-negative",
		},
		{
			name:    "oversized version",
			raw:     `{"schema_version":9223372036854775808}`,
			wantErr: "schema_version must be an integer that fits in int",
		},
		{
			name:    "invalid json",
			raw:     `{`,
			wantErr: "unexpected EOF",
		},
		{
			name:    "primitive root",
			raw:     `"not-a-store"`,
			wantErr: "JSON root must be an object or array",
		},
		{
			name:    "trailing data",
			raw:     `{} {}`,
			wantErr: "trailing data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectJSONSchemaVersion([]byte(tt.raw))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDetectTOMLSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr string
	}{
		{
			name: "missing field is legacy",
			raw:  "default_program = 'claude'\n",
			want: LegacySchemaVersion,
		},
		{
			name: "version field",
			raw:  "schema_version = 3\n",
			want: 3,
		},
		{
			name:    "non-integer version",
			raw:     "schema_version = '3'\n",
			wantErr: "schema_version must be an integer",
		},
		{
			name:    "invalid toml",
			raw:     "schema_version = [\n",
			wantErr: "toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectTOMLSchemaVersion([]byte(tt.raw))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMigrateSchemaBytesMultiHopPreservesUnknownFields(t *testing.T) {
	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, func(raw []byte) ([]byte, error) {
		obj, err := decodeSchemaObject(raw)
		if err != nil {
			return nil, err
		}
		obj["renamed_name"] = obj["name"]
		delete(obj, "name")
		obj[SchemaVersionField] = 1
		return json.Marshal(obj)
	}))
	require.NoError(t, registry.Register(1, func(raw []byte) ([]byte, error) {
		obj, err := decodeSchemaObject(raw)
		if err != nil {
			return nil, err
		}
		obj["finalized"] = true
		obj[SchemaVersionField] = 2
		return json.Marshal(obj)
	}))

	raw := []byte(`{"name":"alpha","unknown":"preserved"}`)
	got, result, err := MigrateSchemaBytes(raw, SchemaMigrationPlan{
		StoreName:      "test-store",
		CurrentVersion: 2,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate: func(raw []byte) error {
			obj, err := decodeSchemaObject(raw)
			if err != nil {
				return err
			}
			if obj["renamed_name"] != "alpha" || obj["finalized"] != true {
				return fmt.Errorf("unexpected migrated object: %#v", obj)
			}
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, SchemaMigrationResult{
		OriginalVersion: LegacySchemaVersion,
		FinalVersion:    2,
		Migrated:        true,
	}, result)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(got, &obj))
	assert.Equal(t, float64(2), obj[SchemaVersionField])
	assert.Equal(t, "alpha", obj["renamed_name"])
	assert.Equal(t, "preserved", obj["unknown"])
	assert.Equal(t, true, obj["finalized"])
	assert.NotContains(t, obj, "name")
}

func TestMigrateSchemaBytesRefusesNewerVersion(t *testing.T) {
	_, _, err := MigrateSchemaBytes([]byte(`{"schema_version":3}`), SchemaMigrationPlan{
		StoreName:      "tasks.json",
		CurrentVersion: 2,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      NewSchemaMigrationRegistry(),
	})
	require.Error(t, err)
	var newer *UnsupportedSchemaVersionError
	require.True(t, errors.As(err, &newer))
	assert.Equal(t, 3, newer.FileVersion)
	assert.Equal(t, 2, newer.SupportedVersion)
	assert.Contains(t, err.Error(), "upgrade af")
}

func TestMigrateSchemaBytesRequiresSequentialMigrator(t *testing.T) {
	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, func(raw []byte) ([]byte, error) {
		obj, err := decodeSchemaObject(raw)
		if err != nil {
			return nil, err
		}
		obj[SchemaVersionField] = 2
		return json.Marshal(obj)
	}))

	_, _, err := MigrateSchemaBytes([]byte(`{"name":"alpha"}`), SchemaMigrationPlan{
		StoreName:      "instances.json",
		CurrentVersion: 2,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced schema_version 2, want 1")
}

func TestLoadAndMigrateSchemaFileBacksUpAndWritesUpgradedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	original := []byte(`{"name":"alpha","unknown":"preserved"}`)
	require.NoError(t, os.WriteFile(path, original, 0644))

	existingBackup := path + ".bak.schema-v0"
	require.NoError(t, os.WriteFile(existingBackup, []byte("do not clobber"), 0644))

	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, migrateObjectToVersion(1)))
	upgraded, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "tasks.json",
		Path:           path,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate:       validateObjectVersion(1),
	})
	require.NoError(t, err)
	assert.True(t, result.Migrated)
	assert.Equal(t, path+".bak.schema-v0.1", result.BackupPath)

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, string(upgraded), string(onDisk))
	assert.Contains(t, string(onDisk), `"schema_version":1`)

	backup, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, original, backup)
	firstBackup, err := os.ReadFile(existingBackup)
	require.NoError(t, err)
	assert.Equal(t, "do not clobber", string(firstBackup))
}

func TestLoadAndMigrateSchemaFileWriteFailurePreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	original := []byte(`{"name":"alpha"}`)
	require.NoError(t, os.WriteFile(path, original, 0644))

	prevWrite := schemaAtomicWriteFile
	writeErr := errors.New("forced write failure")
	schemaAtomicWriteFile = func(string, []byte, os.FileMode) error {
		return writeErr
	}
	t.Cleanup(func() { schemaAtomicWriteFile = prevWrite })

	registry := NewSchemaMigrationRegistry()
	require.NoError(t, registry.Register(0, migrateObjectToVersion(1)))
	_, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "instances.json",
		Path:           path,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      registry,
		Validate:       validateObjectVersion(1),
	})
	require.ErrorIs(t, err, writeErr)
	assert.NotEmpty(t, result.BackupPath)

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, onDisk)
	backup, err := os.ReadFile(result.BackupPath)
	require.NoError(t, err)
	assert.Equal(t, original, backup)
}

func TestLoadAndMigrateSchemaFileNoopDoesNotWriteBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	current := []byte(`{"schema_version":1,"help_screens_seen":15}`)
	require.NoError(t, os.WriteFile(path, current, 0644))

	got, result, err := LoadAndMigrateSchemaFile(SchemaMigrationPlan{
		StoreName:      "state.json",
		Path:           path,
		CurrentVersion: 1,
		DetectVersion:  DetectJSONSchemaVersion,
		Migrators:      NewSchemaMigrationRegistry(),
		Validate:       validateObjectVersion(1),
	})
	require.NoError(t, err)
	assert.False(t, result.Migrated)
	assert.Empty(t, result.BackupPath)
	assert.JSONEq(t, string(current), string(got))

	matches, err := filepath.Glob(path + ".bak.schema-v*")
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func decodeSchemaObject(raw []byte) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		obj = make(map[string]any)
	}
	return obj, nil
}

func migrateObjectToVersion(version int) SchemaMigrator {
	return func(raw []byte) ([]byte, error) {
		obj, err := decodeSchemaObject(raw)
		if err != nil {
			return nil, err
		}
		obj[SchemaVersionField] = version
		return json.Marshal(obj)
	}
}

func validateObjectVersion(version int) SchemaValidator {
	return func(raw []byte) error {
		got, err := DetectJSONSchemaVersion(raw)
		if err != nil {
			return err
		}
		if got != version {
			return fmt.Errorf("schema_version = %d, want %d", got, version)
		}
		return nil
	}
}
