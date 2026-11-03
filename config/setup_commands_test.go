package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setup_commands is the repo-level generic setup hook (#6667): commands run on
// the daemon's machine after a session's workspace is provisioned and before
// its agent starts. These tests pin the key's decode, resolution, and source
// policy — repo-file only, never global or personal.

func TestLoadInRepoConfigSetupCommandsTOML(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, `setup_commands = ["./scripts/session-setup.sh", "echo done"]
`)

	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, raw)
	assert.Equal(t, []string{"./scripts/session-setup.sh", "echo done"}, cfg.SetupCommands)
	assert.True(t, cfg.IsSet("setup_commands"))
	assert.Contains(t, cfg.CommandBearingFields(), "setup_commands")
}

func TestLoadInRepoConfigSetupCommandsJSON(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, `{"setup_commands": ["./scripts/session-setup.sh"]}`)

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"./scripts/session-setup.sh"}, cfg.SetupCommands)
	assert.True(t, cfg.IsSet("setup_commands"))
}

// An explicit empty list is still a set value: like post_worktree_commands it
// means "no setup commands", and IsSet must report the key as present.
func TestLoadInRepoConfigSetupCommandsExplicitEmpty(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "setup_commands = []\n")

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.True(t, cfg.IsSet("setup_commands"))
	assert.Empty(t, cfg.SetupCommands)
}

// setup_commands must resolve through the same ResolveConfig path every other
// repo-level key uses, landing on ResolvedConfig.SetupCommands.
func TestResolveConfigSetupCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, `setup_commands = ["./scripts/session-setup.sh"]
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, []string{"./scripts/session-setup.sh"}, resolved.SetupCommands)
}

// Absent key → empty slice, no error.
func TestResolveConfigSetupCommandsAbsent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Empty(t, resolved.SetupCommands)
}

// setup_commands executes shell commands, so the manifest scopes it to the
// checked-in repo file exactly like post_worktree_commands: it must not appear
// in the global view, and its only admitted source is the repo file — a global
// config.toml carrying it is ignored (warned unknown-key), never executed.
func TestSetupCommandsManifestIsRepoOnly(t *testing.T) {
	var entry ManifestEntry
	found := false
	for _, e := range AllManifest() {
		if e.Key == "setup_commands" {
			entry, found = e, true
		}
	}
	require.True(t, found, "setup_commands must be a manifest key")
	assert.False(t, entry.Sources.Has(SourceGlobal))
	assert.False(t, entry.Sources.Has(SourceProjectPersonal))
	assert.True(t, entry.Sources.Has(SourceRepoShared))
	assert.Equal(t, MergeListReplace, entry.Merge)

	for _, e := range Manifest() {
		assert.NotEqual(t, "setup_commands", e.Key,
			"setup_commands must not be settable from the global config file")
	}

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	dir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, TomlConfigFileName),
		[]byte(`setup_commands = ["./x.sh"]`+"\n"), 0600))
	cfg, err := LoadConfig()
	require.NoError(t, err, "a global config carrying a repo-only key still loads — it is ignored, not fatal")
	assert.NotNil(t, cfg)
}
