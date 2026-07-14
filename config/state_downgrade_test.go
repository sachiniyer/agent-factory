package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFutureState plants a state.json written by a hypothetical newer af
// (schema_version above what this binary supports) and returns its path.
func writeFutureState(t *testing.T, tempHome string) string {
	t.Helper()
	futureData, err := json.Marshal(map[string]any{
		"schema_version":    StateSchemaVersion + 1,
		"help_screens_seen": 42,
	})
	require.NoError(t, err)
	statePath := filepath.Join(tempHome, StateFileName)
	require.NoError(t, os.WriteFile(statePath, futureData, 0644))
	return statePath
}

// A newer-schema state.json must be read without crashing: LoadState degrades to
// defaults rather than panicking or erroring (#1725).
func TestLoadState_NewerSchemaDoesNotCrash(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)
	writeFutureState(t, tempHome)

	state := LoadState()

	require.NotNil(t, state)
	assert.Equal(t, StateSchemaVersion, state.SchemaVersion)
	assert.Equal(t, uint32(0), state.HelpScreensSeen)
}

// The core downgrade guard: an older binary must refuse to overwrite a
// newer-schema state.json, and the newer file must survive untouched on disk
// (#1725).
func TestSaveState_RefusesToDowngradeNewerSchema(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)
	statePath := writeFutureState(t, tempHome)

	state := LoadState()
	require.Equal(t, uint32(0), state.HelpScreensSeen)

	// SetHelpScreensSeen -> SaveState must be refused, not silently downgraded.
	err := state.SetHelpScreensSeen(1)
	require.Error(t, err)
	var newer *UnsupportedSchemaVersionError
	require.True(t, errors.As(err, &newer), "want UnsupportedSchemaVersionError, got %T: %v", err, err)
	assert.Equal(t, StateSchemaVersion+1, newer.FileVersion)
	assert.Equal(t, StateSchemaVersion, newer.SupportedVersion)

	// The on-disk file must be exactly what the newer binary wrote — no clobber.
	savedData, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(savedData, &saved))
	assert.Equal(t, float64(StateSchemaVersion+1), saved["schema_version"])
	assert.Equal(t, float64(42), saved["help_screens_seen"])
}

// Direct SaveState (not via SetHelpScreensSeen) over a newer file is likewise
// refused, and the passed-in state's SchemaVersion is not stamped down.
func TestSaveState_DirectWriteOverNewerSchemaRefused(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)
	writeFutureState(t, tempHome)

	err := SaveState(&State{HelpScreensSeen: 9})
	require.Error(t, err)
	var newer *UnsupportedSchemaVersionError
	assert.True(t, errors.As(err, &newer))
}

// Same-schema saves are unaffected by the guard.
func TestSaveState_SameSchemaSaveWorks(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	require.NoError(t, SaveState(&State{HelpScreensSeen: 3}))
	// A second save over the same-schema file also works.
	require.NoError(t, SaveState(&State{HelpScreensSeen: 5}))

	loaded := LoadState()
	assert.Equal(t, StateSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, uint32(5), loaded.HelpScreensSeen)
}

// A legacy (pre-schema_version) state.json is an older schema and must save
// normally, upgrading in place rather than being refused.
func TestSaveState_LegacySchemaSaveWorks(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	statePath := filepath.Join(tempHome, StateFileName)
	require.NoError(t, os.WriteFile(statePath, []byte(`{"help_screens_seen":7}`), 0644))

	loaded := LoadState()
	assert.Equal(t, uint32(7), loaded.HelpScreensSeen)

	require.NoError(t, loaded.SetHelpScreensSeen(8))

	savedData, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(savedData, &saved))
	assert.Equal(t, float64(StateSchemaVersion), saved["schema_version"])
	assert.Equal(t, float64(8), saved["help_screens_seen"])
}

// Fail-closed edge (Greptile #1778): a state.json whose schema_version is
// PRESENT but cannot be parsed/interpreted as a version this binary understands
// (non-integer, string, out-of-range, negative) is ambiguous — it might come
// from a newer af — so the write must be refused and the file preserved, not
// clobbered.
func TestSaveState_PresentButUnparseableSchemaRefused(t *testing.T) {
	cases := map[string]string{
		"string_value": `{"schema_version":"2","help_screens_seen":42}`,
		"non_integer":  `{"schema_version":1.5,"help_screens_seen":42}`,
		"overflow_int": `{"schema_version":99999999999999999999999,"help_screens_seen":42}`,
		"negative":     `{"schema_version":-3,"help_screens_seen":42}`,
		"bool_value":   `{"schema_version":true,"help_screens_seen":42}`,
		"null_value":   `{"schema_version":null,"help_screens_seen":42}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			tempHome := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", tempHome)
			statePath := filepath.Join(tempHome, StateFileName)
			require.NoError(t, os.WriteFile(statePath, []byte(body), 0644))

			err := SaveState(&State{HelpScreensSeen: 1})
			require.Error(t, err, "write over unparseable schema_version must be refused")

			// The file must be byte-for-byte preserved.
			after, err := os.ReadFile(statePath)
			require.NoError(t, err)
			assert.Equal(t, body, string(after))
		})
	}
}

// A corrupt/unreadable state.json must not wedge saves forever: the guard lets
// the write recover the file to a valid default.
func TestSaveState_CorruptFileDoesNotBlockSave(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	statePath := filepath.Join(tempHome, StateFileName)
	require.NoError(t, os.WriteFile(statePath, []byte("{not json"), 0644))

	require.NoError(t, SaveState(&State{HelpScreensSeen: 4}))

	loaded := LoadState()
	assert.Equal(t, uint32(4), loaded.HelpScreensSeen)
}
