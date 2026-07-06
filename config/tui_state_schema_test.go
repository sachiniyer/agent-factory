package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTUIStatePathUsesConfigDir(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	got, err := TUIStatePath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(tempHome, TUIStateFileName), got)
}

func TestTUIStateSchemaPlanAcceptsCurrentSchemaVersion(t *testing.T) {
	raw := []byte(`{"schema_version":1,"repos":{}}`)
	got, result, err := MigrateSchemaBytes(raw, NewTUIStateSchemaMigrationPlan(TUIStateFileName, validateTUIStateTestEnvelope))
	require.NoError(t, err)

	assert.False(t, result.Migrated)
	assert.Equal(t, TUIStateSchemaVersion, result.OriginalVersion)
	assert.Equal(t, TUIStateSchemaVersion, result.FinalVersion)
	assert.JSONEq(t, string(raw), string(got))
}

func TestTUIStateSchemaPlanRejectsBareVersionField(t *testing.T) {
	_, result, err := MigrateSchemaBytes(
		[]byte(`{"version":1,"repos":{}}`),
		NewTUIStateSchemaMigrationPlan(TUIStateFileName, nil),
	)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "no schema migrator registered for version 0 -> 1")
	assert.Equal(t, LegacySchemaVersion, result.OriginalVersion)
	assert.Equal(t, LegacySchemaVersion, result.FinalVersion)
}

func TestTUIStateSchemaPlanRefusesNewerVersion(t *testing.T) {
	_, _, err := MigrateSchemaBytes(
		[]byte(`{"schema_version":2,"repos":{}}`),
		NewTUIStateSchemaMigrationPlan(TUIStateFileName, nil),
	)
	require.Error(t, err)

	var newer *UnsupportedSchemaVersionError
	require.True(t, errors.As(err, &newer))
	assert.Equal(t, TUIStateFileName, newer.StoreName)
	assert.Equal(t, 2, newer.FileVersion)
	assert.Equal(t, TUIStateSchemaVersion, newer.SupportedVersion)
}

func validateTUIStateTestEnvelope(raw []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	if _, ok := obj["repos"].(map[string]any); !ok {
		return fmt.Errorf("repos must be an object")
	}
	return nil
}
