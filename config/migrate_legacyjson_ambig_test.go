package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedJSONConfig writes a legacy config.json (no config.toml) into a fresh
// AGENT_FACTORY_HOME and returns the home dir, so a migration's LoadConfig
// precondition is the one that converts it.
func seedJSONConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(body), 0644))
	return home
}

// TestMigrateRefusesAmbiguousLegacyJSON is the core of the fix: a legacy
// config.json that writes one setting in BOTH spellings with DIFFERENT values is
// refused, exactly as the same content delivered as TOML is refused (contract
// clause 3 — "refuses rather than choose"). Before the fix the LoadConfig
// precondition converted the JSON to TOML first, the frozen JSON reader
// dropped the grouped value, and the conversion wrote the flat value into both
// spellings — so the migration's own ambiguity guard saw agreement and reported
// Redundant: true about a source that did not agree.
func TestMigrateRefusesAmbiguousLegacyJSON(t *testing.T) {
	home := seedJSONConfig(t, `{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`)
	jsonPath := filepath.Join(home, ConfigFileName)

	_, err := MigrateGlobalConfig()
	require.Error(t, err, "the JSON path must refuse the same ambiguity the TOML path refuses")
	assert.Contains(t, err.Error(), `"listen_addr"`, "the refusal names the flat spelling")
	assert.Contains(t, err.Error(), `"network.listen_addr"`, "and the grouped spelling")
	assert.Contains(t, err.Error(), "0.0.0.0:8443", "it reports the flat value")
	assert.Contains(t, err.Error(), "127.0.0.1:8443", "and the grouped value it would have overwritten")
	assert.Contains(t, err.Error(), "Nothing was rewritten")

	// A refused run leaves the source untouched and writes nothing, mirroring
	// the TOML path (TestMigrateRefusesWhenBothSpellingsDisagree).
	assert.Equal(t, `{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`,
		readFile(t, jsonPath), "config.json is left exactly as it was")
	assert.NoFileExists(t, filepath.Join(home, TomlConfigFileName),
		"no conversion ran, so no config.toml was written")
	assert.NoFileExists(t, filepath.Join(home, ConfigFileName+".bak"),
		"a refused run moves no original aside")

	// The SAME content delivered as TOML is still refused — the two paths now
	// agree, where before they diverged.
	t.Run("same_content_as_toml_is_also_refused", func(t *testing.T) {
		migrateHome(t, "schema_version = 1\nlisten_addr = '0.0.0.0:8443'\n\n[network]\nlisten_addr = '127.0.0.1:8443'\n")
		_, err := MigrateGlobalConfig()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Nothing was rewritten")
	})
}

// TestMigrateJSONAmbiguityRefusalCoversEveryAliasKind makes sure the refusal is
// not specific to string listen_addr: a bool alias and a list alias take the
// same path. The frozen JSON reader drops the grouped table for either kind, so
// the homogenization that hides the divergence is the same.
func TestMigrateJSONAmbiguityRefusalCoversEveryAliasKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		flat string
	}{
		{
			name: "bool require_token",
			body: `{"require_token":true,"network":{"require_token":false}}`,
			flat: "true",
		},
		{
			name: "list cors_allowed_origins",
			body: `{"cors_allowed_origins":["https://a.example.com"],"network":{"cors_allowed_origins":["https://b.example.com"]}}`,
			flat: "[https://a.example.com]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedJSONConfig(t, tc.body)
			_, err := MigrateGlobalConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Nothing was rewritten")
			assert.Contains(t, err.Error(), tc.flat, "the flat value is named in the refusal")
		})
	}
}

// TestMigrateMigratesLegacyJSONWhenBothSpellingsAgree is the other half of the
// both-spellings question on the JSON path. When the two carry the SAME value
// the pre-check does not refuse: the source genuinely agreed, so the
// conversion's redundant report is honest, and the migration drops the flat
// spelling as it does for TOML.
func TestMigrateMigratesLegacyJSONWhenBothSpellingsAgree(t *testing.T) {
	home := seedJSONConfig(t, `{"listen_addr":"127.0.0.1:8443","network":{"listen_addr":"127.0.0.1:8443"}}`)

	result, err := MigrateGlobalConfig()
	require.NoError(t, err, "same-value both-spellings is not ambiguous, so it is not refused")
	require.True(t, result.ConvertedFromJSON)
	require.Len(t, result.Migrated, 1)
	assert.True(t, result.Migrated[0].Redundant, "the source agreed, so Redundant is honest here")
	assert.Equal(t, "listen_addr", result.Migrated[0].From)

	cfg, err := parseConfigTOML([]byte(readFile(t, filepath.Join(home, TomlConfigFileName))), filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr)
}

// TestMigrateDoesNotRefuseLegacyJSONWithOnlyFlatOrOnlyGrouped guards the scope
// of the pre-check: ambiguity needs BOTH spellings. A config.json with only the
// flat spelling converts and migrates as before (no grouped value to diverge
// from); one with only a grouped spelling is not refused by the ambiguity
// guard either, since there is no flat spelling to choose between. The
// grouped-only case is the JSON reader's existing "unknown key … is ignored"
// path and stays LoadConfig's to handle, not the ambiguity guard's.
func TestMigrateDoesNotRefuseLegacyJSONWithOnlyFlatOrOnlyGrouped(t *testing.T) {
	t.Run("only flat converts and migrates", func(t *testing.T) {
		home := seedJSONConfig(t, `{"listen_addr":"0.0.0.0:8443"}`)
		result, err := MigrateGlobalConfig()
		require.NoError(t, err)
		assert.True(t, result.ConvertedFromJSON)
		cfg, err := parseConfigTOML([]byte(readFile(t, filepath.Join(home, TomlConfigFileName))), filepath.Join(home, TomlConfigFileName))
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	})

	t.Run("only grouped is not refused by the ambiguity guard", func(t *testing.T) {
		// The grouped value is dropped by the frozen JSON reader on every load,
		// so the effective value is the default — not a migration concern the
		// ambiguity guard addresses. Asserting no refusal (not no change):
		// whatever LoadConfig does, it is not the ambiguity guard refusing.
		seedJSONConfig(t, `{"network":{"listen_addr":"0.0.0.0:9999"}}`)
		_, err := MigrateGlobalConfig()
		assert.NoError(t, err, "a single grouped spelling is not an ambiguity the guard refuses")
	})
}

// TestMigrateAmbiguousLegacyJSONInvalidFileDefersToLoadConfig makes sure a
// config.json that cannot be decoded for the presence check is not refused by
// the ambiguity guard with a confusing message: it is left for LoadConfig to
// refuse with its own parse-level error, exactly as before the fix.
func TestMigrateAmbiguousLegacyJSONInvalidFileDefersToLoadConfig(t *testing.T) {
	seedJSONConfig(t, `{"listen_addr":"0.0.0.0:8443", oops}`)

	_, err := MigrateGlobalConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not load",
		"a parse error is LoadConfig's to surface, not the ambiguity guard's")
	assert.NotContains(t, err.Error(), "both set, to different values",
		"the ambiguity guard did not run on an unparseable file")
}
