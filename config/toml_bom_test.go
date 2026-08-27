package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// utf8BOM is the three-byte UTF-8 byte-order mark. TOML 1.0 says it "should be
// ignored when present"; these tests pin that contract across every config
// parse and write entry point.
const utf8BOM = "\xef\xbb\xbf"

// TestParseConfigTOML_BOM_SpecCompliance is the Regression case from the bug
// report: a TOML 1.0-conformant BOM-prefixed config.toml must parse.
func TestParseConfigTOML_BOM_SpecCompliance(t *testing.T) {
	t.Run("BOM-prefixed TOML with content should parse (TOML 1.0 spec)", func(t *testing.T) {
		input := utf8BOM + "schema_version = 1\ndefault_program = \"claude\"\n"
		require.False(t, isEffectivelyEmptyToml([]byte(input)), "BOM-prefixed content is not empty")

		cfg, err := parseConfigTOML([]byte(input), "test.toml")
		require.NoError(t, err, "BOM-prefixed TOML should parse per TOML 1.0 spec")
		require.Equal(t, "claude", cfg.DefaultProgram)
		assert.Equal(t, GlobalConfigSchemaVersion, cfg.SchemaVersion)
	})

	t.Run("Without BOM, same content parses successfully", func(t *testing.T) {
		input := "schema_version = 1\ndefault_program = \"claude\"\n"
		cfg, err := parseConfigTOML([]byte(input), "test.toml")
		require.NoError(t, err)
		require.Equal(t, "claude", cfg.DefaultProgram)
	})
}

// A BOM must not break duration-string normalization, which unmarshals raw
// bytes before the typed decoder runs (the path introduced in 8c1ae2ad).
func TestParseConfigTOML_BOMWithDurationString(t *testing.T) {
	input := utf8BOM + "schema_version = 1\ndefault_program = \"claude\"\ndaemon_poll_interval = \"1500ms\"\n"
	cfg, err := parseConfigTOML([]byte(input), "test.toml")
	require.NoError(t, err)
	require.Equal(t, 1500, cfg.DaemonPollInterval)
}

// parseLoadedConfigTOML re-feeds the ORIGINAL bytes to metadataForSource for
// presence tracking. A BOM there used to fail after parseConfigTOML succeeded.
func TestParseLoadedConfigTOML_BOMAttachesProvenance(t *testing.T) {
	input := utf8BOM + "schema_version = 1\ndefault_program = \"claude\"\n"
	cfg, err := parseLoadedConfigTOML([]byte(input), "config.toml", "/home/x/config.toml")
	require.NoError(t, err)
	require.Equal(t, "claude", cfg.DefaultProgram)
	require.NotNil(t, cfg.source.shape, "provenance metadata must be attached")
	_, present := cfg.source.topLevel("default_program")
	assert.True(t, present, "BOM must not prevent source presence tracking")
}

// End-to-end: the user-facing scenario — a BOM-prefixed config.toml on disk.
func TestLoadConfig_BOMGlobalConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	content := utf8BOM + "schema_version = 1\ndefault_program = \"codex\"\ndaemon_poll_interval = \"30m\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(content), 0o644))

	cfg, err := LoadConfig()
	require.NoError(t, err, "a BOM-prefixed global config.toml must load")
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Equal(t, 30*60*1000, cfg.DaemonPollInterval)
}

// A BOM-only global config is still "effectively empty": re-materialize rather
// than fail, mirroring the non-BOM empty-stub path (#864).
func TestLoadConfig_BOMOnlyConfigMaterializes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(utf8BOM), 0o644))

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, defaultDaemonPollInterval, cfg.DaemonPollInterval)
}

func TestLoadInRepoConfig_BOM(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, utf8BOM+
		"default_program = \"codex\"\npost_worktree_commands = [\"npm install\"]\n")

	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err, "a BOM-prefixed in-repo config.toml must load")
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Equal(t, []string{"npm install"}, cfg.PostWorktreeCommands)

	// LoadInRepoConfig returns the raw file bytes for content-hash tracking;
	// the BOM stays there so existing hashes remain stable across this fix.
	assert.True(t, bytes.HasPrefix(raw, []byte(utf8BOM)), "raw bytes retain BOM for hashing")
	hash := InRepoConfigHash(raw)
	assert.NotEmpty(t, hash)
	assert.Equal(t, hash, InRepoConfigHash(raw), "hash is deterministic over the same bytes")
}

func TestParseProjectConfig_BOM(t *testing.T) {
	_, _, project := registeredTestProject(t)
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(utf8BOM+"default_program = \"codex\"\n"), 0o644))

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err, "a BOM-prefixed personal project config must load")
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram)
}

// --- Write paths: a BOM-prefixed config must also be editable. ---

func TestSetGlobalConfigValue_BOM_ScalarEdit(t *testing.T) {
	path := writeTempConfig(t, utf8BOM+"schema_version = 1\ndefault_program = \"claude\"\n")

	_, err := SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.NotContains(t, content, utf8BOM, "BOM is normalized away on write")
	assert.Contains(t, content, "default_program = 'codex'")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)
}

// Structured (table) edits unmarshal the raw text in
// preserveUnknownStructuredMembers; a BOM there aborted `config set`.
func TestSetGlobalConfigValue_BOM_StructuredEdit(t *testing.T) {
	path := writeTempConfig(t, utf8BOM+"schema_version = 1\ndefault_program = \"claude\"\n")

	_, err := SetGlobalConfigValue("theme", `{"accent":"#112233"}`)
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), utf8BOM, "BOM is normalized away on write")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.Theme)
}

func TestUnsetGlobalConfigValue_BOM(t *testing.T) {
	path := writeTempConfig(t, utf8BOM+
		"schema_version = 1\nssh_host_key_verification = \"strict\"\n")

	result, err := UnsetGlobalConfigValue("ssh_host_key_verification")
	require.NoError(t, err)
	require.True(t, result.Removed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), utf8BOM, "BOM is normalized away on write")
	assert.NotContains(t, string(data), "host_key_verification")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig().SSHHostKeyVerification, cfg.SSHHostKeyVerification) // falls back to default
}

func TestUnsetProjectConfigValue_BOM(t *testing.T) {
	_, _, project := registeredTestProject(t)
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// Two keys: unsetting one must leave a parseable file (exercises
	// projectConfigHasNoTopLevelKeys, which unmarshals the edited text).
	require.NoError(t, os.WriteFile(path, []byte(utf8BOM+
		"default_program = \"codex\"\nbranch_prefix = \"feat/\"\n"), 0o644))

	result, err := UnsetProjectConfigValue(project.ID, "default_program")
	require.NoError(t, err)
	require.True(t, result.Removed)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.NotContains(t, content, utf8BOM, "BOM is normalized away on write")
	assert.Contains(t, content, "branch_prefix =")

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "codex", cfg.DefaultProgram) // override gone
}

// SaveInRepoPostWorktreeCommands unmarshals the existing in-repo file before
// re-marshaling; a BOM there used to fail the save outright.
func TestSaveInRepoPostWorktreeCommands_BOM(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, utf8BOM+"default_program = \"codex\"\n")

	require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"npm install"}))

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram, "pre-existing keys survive the round-trip")
	assert.Equal(t, []string{"npm install"}, cfg.PostWorktreeCommands)
}
