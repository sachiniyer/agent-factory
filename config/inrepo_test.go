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
	for _, key := range []string{"auto_update", "auto_yes", "branch_prefix", "daemon_poll_interval", "detach_keys", "keys", "log_max_backups", "log_max_size_mb", "root_agents", "update_channel", "worktree_root"} {
		t.Run(key, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := t.TempDir()
			writeInRepoConfig(t, repoRoot, `{"`+key+`": "x"}`)

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.Contains(t, err.Error(), "global setting")
			// The message must name the real global config file under the
			// active config dir, not a hardcoded ~/.agent-factory path that
			// AGENT_FACTORY_HOME has relocated (#890). TOML-only keys (the
			// keymap) point at config.toml; every other key at config.json
			// (#1141 play-test minor 4).
			wantFile := ConfigFileName
			if tomlOnlyGlobalKeys[key] {
				wantFile = TomlConfigFileName
			}
			assert.Contains(t, err.Error(), prettyHomePath(filepath.Join(home, wantFile)))
			assert.NotContains(t, err.Error(), "~/.agent-factory/config.json")
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

// TestLoadInRepoConfigRejectsNonObject covers #1153: a bare JSON `null`
// unmarshals into a map as nil without error and was silently accepted as an
// empty config; other non-object top-level values (string, number, array) were
// already rejected by the decoder. All must fail loudly with the file named.
func TestLoadInRepoConfigRejectsNonObject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("null", func(t *testing.T) {
		repoRoot := t.TempDir()
		path := writeInRepoConfig(t, repoRoot, "null")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a JSON object")
		assert.Contains(t, err.Error(), prettyHomePath(path))
	})

	t.Run("bare string", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `"hello"`)
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
	})

	t.Run("bare number", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, "123")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
	})

	t.Run("bare array", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, "[]")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
	})
}

// TestLoadInRepoConfigEmptyObject confirms `{}` — a structurally valid object
// with no keys — is accepted (distinct from `null`): a non-nil config with no
// keys marked set.
func TestLoadInRepoConfigEmptyObject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, "{}")

	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, raw)
	for _, key := range inRepoAllowedKeys {
		assert.False(t, cfg.IsSet(key), "no key should be marked set for {}")
	}
}

// TestSaveInRepoPostWorktreeCommandsNull is the regression test for the #1153
// panic: SaveInRepoPostWorktreeCommands read a `null` config file, unmarshaled
// it into a nil map, and panicked writing the key. It must now return an
// actionable error naming the file instead of crashing.
func TestSaveInRepoPostWorktreeCommandsNull(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	path := writeInRepoConfig(t, repoRoot, "null")

	err := SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a JSON object")
	assert.Contains(t, err.Error(), prettyHomePath(path))
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

// TestSaveInRepoSymlinkedConfigHomeCollision is the regression test for #812:
// AGENT_FACTORY_HOME pointing at the repo's .agent-factory dir through a
// symlink must still trip the save collision guard — filepath.Clean alone
// compares the unequal strings and lets the save overwrite the global config.
func TestSaveInRepoSymlinkedConfigHomeCollision(t *testing.T) {
	repoRoot := t.TempDir()
	realHome := filepath.Join(repoRoot, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(realHome, 0755))
	globalContent := `{"default_program": "claude", "auto_yes": true}`
	globalFile := filepath.Join(realHome, ConfigFileName)
	require.NoError(t, os.WriteFile(globalFile, []byte(globalContent), 0644))

	linkHome := filepath.Join(t.TempDir(), "af-home-link")
	require.NoError(t, os.Symlink(realHome, linkHome))
	t.Setenv("AGENT_FACTORY_HOME", linkHome)

	err := SaveInRepoPostWorktreeCommands(repoRoot, []string{"echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")

	data, err := os.ReadFile(globalFile)
	require.NoError(t, err)
	assert.JSONEq(t, globalContent, string(data), "global config must be untouched by the refused save")

	// The read path must keep treating this layout as "no in-repo config".
	cfg, raw, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.Nil(t, raw)
}

func TestSaveInRepoRefusesSymlinkDirOutsideRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(repoRoot, InRepoConfigDirName)))

	err := SaveInRepoPostWorktreeCommands(repoRoot, []string{"echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the repository")

	for _, name := range []string{ConfigFileName, TomlConfigFileName} {
		_, statErr := os.Stat(filepath.Join(outsideDir, name))
		assert.True(t, os.IsNotExist(statErr), "nothing must be written outside the repo (%s)", name)
	}
}

func TestSaveInRepoRefusesSymlinkDirSwappedOutsideBeforeWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	configDir := filepath.Join(repoRoot, InRepoConfigDirName)
	writeInRepoTomlConfig(t, repoRoot, `default_program = "claude"`+"\n")
	outsideDir := t.TempDir()

	prevHook := beforeInRepoConfigWrite
	beforeInRepoConfigWrite = func() error {
		if err := os.RemoveAll(configDir); err != nil {
			return err
		}
		return os.Symlink(outsideDir, configDir)
	}
	t.Cleanup(func() { beforeInRepoConfigWrite = prevHook })

	err := SaveInRepoPostWorktreeCommands(repoRoot, []string{"echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the repository")

	for _, name := range []string{ConfigFileName, TomlConfigFileName} {
		_, statErr := os.Stat(filepath.Join(outsideDir, name))
		assert.True(t, os.IsNotExist(statErr), "nothing must be written outside the repo (%s)", name)
	}
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

	// Regression test for #1092: a symlinked config.json must be written
	// through to its target, not replaced by a new regular file at the link
	// path (which would strand the target with stale content).
	t.Run("writes through a symlinked config file", func(t *testing.T) {
		repoRoot := t.TempDir()
		target := filepath.Join(repoRoot, "shared-config.json")
		require.NoError(t, os.WriteFile(target, []byte(`{"default_program": "codex"}`), 0644))
		require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0755))
		linkPath := InRepoConfigPath(repoRoot)
		require.NoError(t, os.Symlink(target, linkPath))

		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		info, err := os.Lstat(linkPath)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "config path must still be a symlink after save")
		dest, err := os.Readlink(linkPath)
		require.NoError(t, err)
		assert.Equal(t, target, dest, "symlink must still point at its original target")

		data, err := os.ReadFile(target)
		require.NoError(t, err)
		assert.Contains(t, string(data), "make setup", "target file must receive the update")

		// Read-after-write round-trips through the symlink, preserving the
		// pre-existing field alongside the saved commands.
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
		assert.Equal(t, "codex", cfg.DefaultProgram)
	})

	t.Run("dir symlink that stays inside the repo still saves", func(t *testing.T) {
		repoRoot := t.TempDir()
		realDir := filepath.Join(repoRoot, "cfg")
		require.NoError(t, os.MkdirAll(realDir, 0755))
		require.NoError(t, os.Symlink(realDir, filepath.Join(repoRoot, InRepoConfigDirName)))

		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
	})
}
