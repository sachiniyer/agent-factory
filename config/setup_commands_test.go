package config

import (
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

// setup_commands executes shell commands, so it is repo-scoped by the manifest:
// a global config must not be allowed to set it, exactly like
// post_worktree_commands.
func TestSetupCommandsRejectedInGlobalConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := parseGlobalConfigContent(`setup_commands = ["./x.sh"]`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "setup_commands")
}
