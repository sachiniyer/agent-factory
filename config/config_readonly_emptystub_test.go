package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedHome writes content to config.toml in a fresh AGENT_FACTORY_HOME and
// returns that home. When json is non-empty it also writes config.json so the
// shadow case can be exercised.
func seedHome(t *testing.T, content string, json ...string) string {
	t.Helper()
	home := t.TempDir()
	tomlPath := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte(content), 0o644))
	for _, j := range json {
		require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(j), 0o644))
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	return home
}

// effectivelyEmptyTomlInputs is every shape isEffectivelyEmptyToml treats as
// empty — the fingerprint the bug report names. Each must self-heal at
// startup and report EmptyStub (not an error) from the read-only diagnostics.
func effectivelyEmptyTomlInputs() map[string]string {
	return map[string]string{
		"zero bytes":      "",
		"whitespace":      "   \n\n\t ",
		"bom only":        utf8BOM,
		"comment only":    "# TODO fill this in\n",
		"comments only":   "# header\n# another reminder\n",
		"bom and comment": utf8BOM + "# just a note\n",
	}
}

// TestLoadConfigReadOnly_EmptyStubAgreesWithStartup is the core guarantee: for
// every effectively-empty config.toml with no shadowing config.json, the read
// -only diagnostic (LoadConfigReadOnly) and startup (LoadConfig) must return the
// SAME verdict. Startup self-heals (removes the stub, materializes defaults,
// returns (*Config, nil)); the no-write diagnostic returns EmptyStub=true with a
// nil error. Before the fix the diagnostic raised a hard "config is empty"
// error, so `af doctor`/`af config validate` rejected a state af boots on.
//
// The two loaders run against separate identical homes because LoadConfig
// mutates disk (os.Remove + materializeDefaultConfig): the assertion is that
// the two functions return the same verdict on the same INPUT state.
func TestLoadConfigReadOnly_EmptyStubAgreesWithStartup(t *testing.T) {
	for name, content := range effectivelyEmptyTomlInputs() {
		t.Run(name, func(t *testing.T) {
			fastShell(t)

			// Read-only on a fresh home: the diagnostic verdict.
			homeRO := seedHome(t, content)
			tomlPath := filepath.Join(homeRO, TomlConfigFileName)
			loaded, roErr := LoadConfigReadOnly()
			require.NoError(t, roErr, "read-only diagnostic must not fail a state startup self-heals")
			assert.True(t, loaded.EmptyStub, "an effectively-empty stub with no config.json must surface as EmptyStub")
			assert.False(t, loaded.Missing, "a present stub is not Missing")
			assert.Nil(t, loaded.Config, "EmptyStub does not synthesize a Config; defaults are implied and materialized on the next start")
			assert.Equal(t, tomlPath, loaded.Path)

			// No-write contract: the stub is intact and nothing was created.
			got, err := os.ReadFile(tomlPath)
			require.NoError(t, err)
			assert.Equal(t, content, string(got), "LoadConfigReadOnly must not remove or rewrite the stub")
			_, statErr := os.Stat(filepath.Join(homeRO, ConfigFileName))
			assert.True(t, os.IsNotExist(statErr), "LoadConfigReadOnly must not materialize a config.json")

			// Startup on a separate identical home: the self-heal verdict.
			homeLC := seedHome(t, content)
			cfg, lcErr := LoadConfig()
			require.NoError(t, lcErr, "startup must self-heal the empty stub")
			require.NotNil(t, cfg, "startup must return a real (default) config after self-healing")
			lcAfter, err := os.ReadFile(filepath.Join(homeLC, TomlConfigFileName))
			require.NoError(t, err)
			assert.NotEmpty(t, lcAfter, "startup must re-materialize non-empty defaults over the stub")
		})
	}
}

// TestLoadConfigReadOnly_EmptyStubDoesNotMutate pins the read-only contract on a
// representative effectively-empty stub and asserts nothing on disk changed:
// the stub stays, no config.json appears, and no lock/backup artifacts were
// left behind. LoadConfigReadOnly backs `af config validate`, whose stated
// contract is "It writes nothing and materializes nothing — a read-only check".
func TestLoadConfigReadOnly_EmptyStubDoesNotMutate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, TomlConfigFileName)
	content := []byte("# placeholder; fill in later\n")
	require.NoError(t, os.WriteFile(tomlPath, content, 0o644))

	before, err := os.ReadDir(home)
	require.NoError(t, err)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err)
	require.True(t, loaded.EmptyStub)

	got, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.Equal(t, content, got, "validate must not rewrite the stub it checks")

	after, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Len(t, after, len(before), "validate must not create any file (no config.json, no lock, no backup)")
}

// TestLoadConfigReadOnly_EmptyStubWithShadowStaysLoudError mirrors loadConfig:
// an effectively-empty config.toml sitting beside a real config.json is a
// hand-made shadow, and re-materializing would silently discard the config.json
// settings — so BOTH paths keep it a loud error. The fix must NOT downgrade the
// shadow case to EmptyStub.
func TestLoadConfigReadOnly_EmptyStubWithShadowStaysLoudError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, TomlConfigFileName)
	jsonPath := filepath.Join(home, ConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# oops, empty\n"), 0o644))
	// A real, parseable config.json so the shadow contains actual settings.
	require.NoError(t, os.WriteFile(jsonPath, []byte(`{"schema_version":1,"default_program":"claude"}`), 0o644))

	loaded, roErr := LoadConfigReadOnly()
	require.Error(t, roErr, "an empty toml shadowing a real config.json must stay a loud error")
	assert.False(t, loaded.EmptyStub, "the shadow case must not be downgraded to EmptyStub")
	assert.Contains(t, roErr.Error(), "is empty")

	// And the verdict matches startup, which also keeps it a loud error.
	fastShell(t)
	_, lcErr := LoadConfig()
	require.Error(t, lcErr, "startup must also keep the shadow case a loud error (do not self-heal over config.json)")

	// Neither loader mutated the shadow.
	got, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "default_program", "the config.json must be left intact")
	stub, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, "# oops, empty\n", string(stub), "the empty toml stub must be left intact")
}

// TestLoadConfigReadOnly_EmptySymlinkStubStaysLoudError mirrors loadConfig: a
// symlink config.toml whose target is effectively empty (and no config.json)
// is a hand-made arrangement, not a failed first-run write — so BOTH paths keep
// it a loud error rather than unlinking the operator's dotfiles. The fix must
// NOT upgrade the symlink-stub case to EmptyStub.
func TestLoadConfigReadOnly_EmptySymlinkStubStaysLoudError(t *testing.T) {
	home, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	target := filepath.Join(dotfiles, "empty.toml")
	require.NoError(t, os.WriteFile(target, []byte(""), 0o644))
	link := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(target, link))

	loaded, roErr := LoadConfigReadOnly()
	require.Error(t, roErr, "a symlink stub to an empty target must stay a loud error")
	assert.False(t, loaded.EmptyStub, "the symlink-stub case must not be downgraded to EmptyStub")
	assert.False(t, loaded.Missing)
	assert.Contains(t, roErr.Error(), "is empty")

	_, lcErr := LoadConfig()
	require.Error(t, lcErr, "startup must also keep the symlink stub a loud error")

	// Neither loader unlinked the operator's symlink.
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "the symlink must be left in place")
	_, err = os.Stat(target)
	require.NoError(t, err, "the dotfiles target must be left in place")
}
