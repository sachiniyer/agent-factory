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

// TestMigrateAmbiguousLegacyJSONTypeMismatchDefersToLoadConfig pins the case a
// shapeless decode CANNOT catch but the typed reader does: a flat value whose
// JSON kind is valid but whose type the frozen reader will not accept. The
// shapeless map happily holds "yes" against a bool field, so the presence check
// sees a divergence and would report the "delete whichever line is wrong"
// remedy — but the conversion would never run, because parseConfigForConversion
// rejects the string-to-bool decode. The guard confirms the typed read before
// refusing, so the file is left for LoadConfig's own parse error instead.
func TestMigrateAmbiguousLegacyJSONTypeMismatchDefersToLoadConfig(t *testing.T) {
	home := seedJSONConfig(t, `{"require_token":"yes","network":{"require_token":false}}`)

	_, err := MigrateGlobalConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not load",
		"a type mismatch is LoadConfig's to surface, not the ambiguity guard's")
	assert.Contains(t, err.Error(), "failed to parse config file",
		"LoadConfig names the parse failure, not an ambiguity")
	assert.Contains(t, err.Error(), "cannot unmarshal",
		"the underlying error is the typed reader's type rejection")
	assert.NotContains(t, err.Error(), "delete whichever line is wrong",
		"the ambiguity remedy is not offered for a file the reader would never convert")
	assert.NoFileExists(t, filepath.Join(home, TomlConfigFileName),
		"a parse failure writes nothing")
}

// TestMigrateAmbiguousJSONWithDanglingTomlLinkRefusesTheLink pins the case the
// dangling-symlink guard was added for: a config.toml that is a symlink to a
// missing target reads as ENOENT through fileExists (Stat follows the link),
// so a real, ambiguous config.json beside it would reach the JSON guard first
// and report the ambiguity — directing the operator at an ignored JSON file
// while the broken canonical link is the actual problem. LoadConfig refuses the
// dangling link before considering JSON (#3660 review); the pre-check does the
// same, so the both-ends link error wins over the JSON ambiguity remedy.
func TestMigrateAmbiguousJSONWithDanglingTomlLinkRefusesTheLink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")

	// A real, ambiguous config.json.
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ConfigFileName),
		[]byte(`{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`),
		0o644,
	))

	// A config.toml that is a symlink to a missing target — the dotfiles-repo
	// shape with a moved or deleted canonical file.
	missing := filepath.Join(t.TempDir(), "gone.toml")
	tomlLink := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(missing, tomlLink))

	_, err := MigrateGlobalConfig()
	require.Error(t, err, "a dangling canonical link is refused, not masked by the JSON guard")
	assert.Contains(t, err.Error(), "symlink", "the dangling link is named, not the JSON ambiguity")
	assert.Contains(t, err.Error(), "cannot be resolved",
		"the both-ends link error from refuseDanglingConfigLink is reported")
	assert.NotContains(t, err.Error(), "both set, to different values",
		"the JSON ambiguity remedy is not offered when the canonical link is broken")

	// Neither file was rewritten: the JSON guard never ran past the link check,
	// and the dangling link itself was left alone.
	assert.Equal(t, `{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`,
		readFile(t, filepath.Join(home, ConfigFileName)), "config.json is left exactly as it was")
	info, lerr := os.Lstat(tomlLink)
	require.NoError(t, lerr)
	assert.True(t, info.Mode()&os.ModeSymlink != 0, "the dangling symlink is left in place")
}

// TestMigrateAmbiguousLegacyJSONHardensAFHomeBeforeRefusing pins the home-hardening
// ordering the pre-check must keep. The ambiguity guard refuses BEFORE LoadConfig
// runs, and loadConfig is where the owner-only home repair lives
// (secureAFHomeForPath, config_load.go). A default AF home left 0755 by an older
// release must still be tightened to 0700 even when the file is refused, so a
// refused run does not leave credential-adjacent state under stale permissions
// (#2197) — the same repair LoadConfig would have performed had the file loaded.
func TestMigrateAmbiguousLegacyJSONHardensAFHomeBeforeRefusing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0755 home cannot be staged as world-readable")
	}
	fastShell(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")
	afHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(afHome, ConfigFileName),
		[]byte(`{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`),
		0o644,
	))
	require.NoError(t, os.Chmod(afHome, 0o755))

	_, err := MigrateGlobalConfig()
	require.Error(t, err, "ambiguous JSON is still refused")
	assert.Contains(t, err.Error(), "with different values",
		"the refusal is the ambiguity guard's, not a side effect of the repair")
	assert.Contains(t, err.Error(), "Nothing was rewritten",
		"the refusal left the source untouched")

	// The home was hardened before the refusal returned, exactly as LoadConfig
	// would have done had the file not been refused.
	info, statErr := os.Stat(afHome)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"a refused ambiguous legacy JSON still tightens a 0755 default AF home to 0700")

	// The refusal still left the source untouched, as every other refusal does.
	assert.Equal(t, `{"listen_addr":"0.0.0.0:8443","network":{"listen_addr":"127.0.0.1:8443"}}`,
		readFile(t, filepath.Join(afHome, ConfigFileName)), "config.json is left exactly as it was")
	assert.NoFileExists(t, filepath.Join(afHome, TomlConfigFileName),
		"no conversion ran, so no config.toml was written")
}

// TestMigrateDoesNotRefuseLegacyJSONNullFlatAgainstZeroGrouped pins the case a
// raw DeepEqual gets wrong: an explicit JSON null decodes to an untyped nil,
// which DeepEqual never matches against a typed zero like false, so a flat
// null and a grouped spelling equal to the field's zero would be reported as
// divergent and refused. The typed reader leaves the scalar at its zero value
// for null, so the conversion writes that zero into both spellings — the same
// value the grouped spelling already carries — and there is no tie to break.
// The guard normalizes the flat null to the grouped kind's zero before
// comparing, so the run converts instead of refusing. A null flat against a
// NON-zero grouped spelling still diverges and is still refused.
func TestMigrateDoesNotRefuseLegacyJSONNullFlatAgainstZeroGrouped(t *testing.T) {
	t.Run("null flat and zero grouped convert as redundant", func(t *testing.T) {
		home := seedJSONConfig(t, `{"require_token":null,"network":{"require_token":false}}`)

		result, err := MigrateGlobalConfig()
		require.NoError(t, err, "a null flat and a zero-valued grouped spelling agree, so no refusal")
		require.True(t, result.ConvertedFromJSON)
		require.Len(t, result.Migrated, 1)
		assert.True(t, result.Migrated[0].Redundant, "the flat null and the zero grouped value agree")
		assert.Equal(t, "require_token", result.Migrated[0].From)

		cfg, err := parseConfigTOML([]byte(readFile(t, filepath.Join(home, TomlConfigFileName))), filepath.Join(home, TomlConfigFileName))
		require.NoError(t, err)
		assert.False(t, cfg.RequireToken, "the null flat and zero grouped value both resolve to false")
	})

	t.Run("null flat and non-zero grouped is still refused", func(t *testing.T) {
		seedJSONConfig(t, `{"require_token":null,"network":{"require_token":true}}`)

		_, err := MigrateGlobalConfig()
		require.Error(t, err, "a null flat and a non-zero grouped spelling diverge, so the run refuses")
		assert.Contains(t, err.Error(), "Nothing was rewritten")
		assert.Contains(t, err.Error(), `"require_token"`, "the refusal still names the flat spelling")
	})
}

// TestMigrateAmbiguousLegacyJSONTrailingGarbageDefersToLoadConfig pins the other
// divergence the finding named: json.Decoder.Decode (the shapeless read) stops
// after the first object and ignores trailing garbage, while json.Unmarshal
// (the typed reader, via normalizeJSONDurationValues) rejects it. The guard
// confirms the typed read before refusing, so trailing garbage defers to
// LoadConfig's parse error rather than reporting an ambiguity.
func TestMigrateAmbiguousLegacyJSONTrailingGarbageDefersToLoadConfig(t *testing.T) {
	home := seedJSONConfig(t, `{"require_token":true,"network":{"require_token":false}}garbage`)

	_, err := MigrateGlobalConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not load",
		"trailing garbage is LoadConfig's to surface, not the ambiguity guard's")
	assert.Contains(t, err.Error(), "failed to parse config file",
		"LoadConfig names the parse failure, not an ambiguity")
	assert.Contains(t, err.Error(), "invalid character",
		"the underlying error is the typed reader rejecting the trailing bytes")
	assert.NotContains(t, err.Error(), "delete whichever line is wrong",
		"the ambiguity remedy is not offered for a file the reader would never convert")
	assert.NoFileExists(t, filepath.Join(home, TomlConfigFileName),
		"a parse failure writes nothing")
}
