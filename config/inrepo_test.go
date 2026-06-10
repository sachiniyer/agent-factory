package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeInRepoConfig materializes <repoRoot>/.agent-factory/config.json with
// the given content and returns its path.
func writeInRepoConfig(t *testing.T, repoRoot, content string) string {
	t.Helper()
	dir := filepath.Join(repoRoot, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	path := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoadInRepoConfigAbsent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg, raw, err := LoadInRepoConfig(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.Nil(t, raw)
}

func TestLoadInRepoConfigFields(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, `{
		"default_program": "aider",
		"program_overrides": {"codex": "/opt/codex --fast"},
		"post_worktree_commands": ["npm install"],
		"remote_hooks": {"launch_cmd": "l", "list_cmd": "ls", "attach_cmd": "a", "delete_cmd": "d"}
	}`)

	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, raw)
	assert.Equal(t, "aider", cfg.DefaultProgram)
	assert.Equal(t, "/opt/codex --fast", cfg.ProgramOverrides["codex"])
	assert.Equal(t, []string{"npm install"}, cfg.PostWorktreeCommands)
	require.NotNil(t, cfg.RemoteHooks)
	assert.Equal(t, "l", cfg.RemoteHooks.LaunchCmd)
	for _, key := range []string{"default_program", "program_overrides", "post_worktree_commands", "remote_hooks"} {
		assert.True(t, cfg.IsSet(key), "expected %s to be marked set", key)
	}
	assert.Equal(t, []string{"post_worktree_commands", "program_overrides", "remote_hooks"}, cfg.CommandBearingFields())
}

func TestLoadInRepoConfigEmptyValueIsSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": []}`)

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	assert.True(t, cfg.IsSet("post_worktree_commands"))
	assert.Empty(t, cfg.PostWorktreeCommands)
	assert.False(t, cfg.IsSet("remote_hooks"))
}

func TestLoadInRepoConfigRejectsGlobalOnlyKeys(t *testing.T) {
	for _, key := range []string{"auto_yes", "branch_prefix", "daemon_poll_interval", "detach_keys", "worktree_root"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoRoot := t.TempDir()
			writeInRepoConfig(t, repoRoot, `{"`+key+`": "x"}`)

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.Contains(t, err.Error(), "global setting")
			assert.Contains(t, err.Error(), "~/.agent-factory/config.json")
		})
	}
}

func TestLoadInRepoConfigRejectsUnknownKeys(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, `{"post_worktree_cmds": ["typo"]}`)

	_, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "post_worktree_cmds")
	assert.Contains(t, err.Error(), "allowed keys")
}

func TestLoadInRepoConfigValidatesProgramEnums(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("default_program with flags", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"default_program": "claude --model opus"}`)
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "program_overrides")
	})

	t.Run("program_overrides unknown agent key", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"program_overrides": {"not-an-agent": "/bin/x"}}`)
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-an-agent")
	})
}

func TestLoadInRepoConfigMalformed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("empty file", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, "")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("invalid json", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, "{not json")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
	})
}

func TestLoadInRepoConfigTraversalSafety(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("symlinked file outside repo is rejected", func(t *testing.T) {
		repoRoot := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		require.NoError(t, os.WriteFile(outside, []byte(`{"post_worktree_commands": ["evil"]}`), 0644))
		require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0755))
		require.NoError(t, os.Symlink(outside, InRepoConfigPath(repoRoot)))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the repository")
	})

	t.Run("symlinked .agent-factory dir outside repo is rejected", func(t *testing.T) {
		repoRoot := t.TempDir()
		outsideDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outsideDir, ConfigFileName), []byte(`{}`), 0644))
		require.NoError(t, os.Symlink(outsideDir, filepath.Join(repoRoot, InRepoConfigDirName)))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the repository")
	})

	t.Run("config path that is a directory is rejected", func(t *testing.T) {
		repoRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(InRepoConfigPath(repoRoot), 0755))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	})

	t.Run("symlink that stays inside the repo is allowed", func(t *testing.T) {
		repoRoot := t.TempDir()
		real := filepath.Join(repoRoot, "shared-config.json")
		require.NoError(t, os.WriteFile(real, []byte(`{"default_program": "codex"}`), 0644))
		require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0755))
		require.NoError(t, os.Symlink(real, InRepoConfigPath(repoRoot)))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "codex", cfg.DefaultProgram)
	})
}

// TestLoadInRepoConfigHomeRepoCollision covers a dotfiles-style repo rooted at
// the config home: the in-repo path coincides with the global config file,
// which must be treated as "no in-repo config" — not parsed under in-repo
// scoping rules (which would reject its global keys).
func TestLoadInRepoConfigHomeRepoCollision(t *testing.T) {
	repoRoot := t.TempDir()
	configHome := filepath.Join(repoRoot, InRepoConfigDirName)
	t.Setenv("AGENT_FACTORY_HOME", configHome)
	require.NoError(t, os.MkdirAll(configHome, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configHome, ConfigFileName), []byte(`{"default_program": "claude", "auto_yes": true}`), 0644))

	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.Nil(t, raw)

	err = SaveInRepoPostWorktreeCommands(repoRoot, []string{"echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")
}

func TestSaveInRepoPostWorktreeCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("creates file and round-trips", func(t *testing.T) {
		repoRoot := t.TempDir()
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
	})

	t.Run("preserves other fields", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"default_program": "gemini", "remote_hooks": {"launch_cmd": "l", "attach_cmd": "a", "delete_cmd": "d"}}`)
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"go generate ./..."}))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "gemini", cfg.DefaultProgram)
		require.NotNil(t, cfg.RemoteHooks)
		assert.Equal(t, "l", cfg.RemoteHooks.LaunchCmd)
		assert.Equal(t, []string{"go generate ./..."}, cfg.PostWorktreeCommands)
	})

	t.Run("empty list is written as an explicit key", func(t *testing.T) {
		repoRoot := t.TempDir()
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, nil))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.True(t, cfg.IsSet("post_worktree_commands"))
		assert.Empty(t, cfg.PostWorktreeCommands)
	})
}
