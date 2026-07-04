package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeInRepoTomlConfig materializes <repoRoot>/.agent-factory/config.toml
// with the given content and returns its path.
func writeInRepoTomlConfig(t *testing.T, repoRoot, content string) string {
	t.Helper()
	dir := filepath.Join(repoRoot, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	path := filepath.Join(dir, TomlConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoadInRepoConfigTOMLFields(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, `
default_program = "aider"
post_worktree_commands = ["npm install"]

[program_overrides]
codex = "/opt/codex --fast"

[remote_hooks]
launch_cmd = "l"
list_cmd = "ls"
attach_cmd = "a"
delete_cmd = "d"
`)

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

func TestLoadInRepoConfigTOMLEmptyValueIsSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "post_worktree_commands = []\n")

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err)
	assert.True(t, cfg.IsSet("post_worktree_commands"))
	assert.Empty(t, cfg.PostWorktreeCommands)
	assert.False(t, cfg.IsSet("remote_hooks"))
}

func TestLoadInRepoConfigRejectsBothFormats(t *testing.T) {
	// A repo carrying config.toml AND config.json is a hard error, not a
	// precedence rule: the file executes shell commands and the two copies
	// may be edited by different collaborators — af must not guess which is
	// live (#1030).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoConfig(t, repoRoot, `{"default_program": "codex"}`)
	writeInRepoTomlConfig(t, repoRoot, `default_program = "aider"`+"\n")

	_, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), TomlConfigFileName)
	assert.Contains(t, err.Error(), ConfigFileName)
	assert.Contains(t, err.Error(), "exactly one")

	// The save path must refuse identically instead of picking a side.
	err = SaveInRepoPostWorktreeCommands(repoRoot, []string{"echo hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestLoadInRepoConfigTOMLKeyPolicy(t *testing.T) {
	t.Run("rejects global-only key", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, `auto_yes = true`+"\n")

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "auto_yes")
		assert.Contains(t, err.Error(), "global setting")
		// A non-TOML-only global key points at the resolved global config file.
		assert.Contains(t, err.Error(), ConfigFileName)
	})

	t.Run("rejecting the TOML-only keys table points at config.toml", func(t *testing.T) {
		// #1141 play-test minor 4: the keymap is TOML-only, so the "move it to
		// the global config" message must name config.toml — a config.json
		// carrying "keys" is ignored-with-warning, so directing the user there
		// would land them in the dead path.
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[keys]\nquit = \"Q\"\n")

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "global setting")
		assert.Contains(t, err.Error(), TomlConfigFileName)
		assert.NotContains(t, err.Error(), prettyHomePath(filepath.Join(home, ConfigFileName)),
			"the keys rejection must not point at the ignored config.json path")
	})

	t.Run("rejects unknown key", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, `post_worktree_cmds = ["typo"]`+"\n")

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "post_worktree_cmds")
		assert.Contains(t, err.Error(), "allowed keys")
	})

	t.Run("validates program enums", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, `default_program = "claude --model opus"`+"\n")

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "program_overrides")
	})
}

func TestLoadInRepoConfigTOMLMalformed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("contentless file", func(t *testing.T) {
		// Zero bytes, whitespace-only, and BOM-only all decode as a valid
		// empty TOML document; each must stay a loud error (#1139 review).
		for name, content := range map[string]string{
			"zero-byte":       "",
			"whitespace-only": " \n\t\n",
			"BOM-only":        "\xef\xbb\xbf",
		} {
			t.Run(name, func(t *testing.T) {
				repoRoot := t.TempDir()
				writeInRepoTomlConfig(t, repoRoot, content)
				_, _, err := LoadInRepoConfig(repoRoot)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "empty")
				assert.Contains(t, err.Error(), "TOML")
			})
		}
	})

	t.Run("invalid toml names file and line", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "default_program = \"codex\"\npost_worktree_commands = [\n")
		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), TomlConfigFileName)
		assert.Contains(t, err.Error(), "line")
	})
}

// TestLoadInRepoConfigTOMLRejectsNonObject is the TOML counterpart to the
// #1153 JSON null hole. TOML has no `null` literal and a document must be a
// table (key = value) at the top level, so a bare null / string / number is a
// syntax error the decoder already rejects — this pins that guarantee so the
// TOML path can never silently accept a non-object the way JSON did.
func TestLoadInRepoConfigTOMLRejectsNonObject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	for name, content := range map[string]string{
		"null":        "null\n",
		"bare string": `"hello"` + "\n",
		"bare number": "123\n",
	} {
		t.Run(name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, content)
			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), TomlConfigFileName)
		})
	}
}

// TestSaveInRepoPostWorktreeCommandsTOMLNonObject confirms the TOML save path
// has no nil-map panic analog to the JSON one (#1153): a non-object existing
// config.toml is a parse error the save surfaces cleanly rather than crashing.
func TestSaveInRepoPostWorktreeCommandsTOMLNonObject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "123\n")

	err := SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), TomlConfigFileName)
}

func TestLoadInRepoConfigTOMLTraversalSafety(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("symlinked toml outside repo is rejected", func(t *testing.T) {
		repoRoot := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.toml")
		require.NoError(t, os.WriteFile(outside, []byte(`default_program = "codex"`+"\n"), 0644))
		require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0755))
		require.NoError(t, os.Symlink(outside, InRepoTomlConfigPath(repoRoot)))

		_, _, err := LoadInRepoConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the repository")
	})

	t.Run("global config.toml symlinked as in-repo is treated as absent", func(t *testing.T) {
		// The dotfiles-repo guard must cover the TOML global file too: a
		// repo rooted at the config home would otherwise re-read the global
		// config under in-repo scoping rules.
		repoRoot := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", filepath.Join(repoRoot, InRepoConfigDirName))
		writeInRepoTomlConfig(t, repoRoot, `default_program = "codex"`+"\n")

		cfg, raw, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Nil(t, cfg)
		assert.Nil(t, raw)
	})
}

func TestSaveInRepoPostWorktreeCommandsTOML(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("a repo with no config gets a new config.toml", func(t *testing.T) {
		repoRoot := t.TempDir()
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		_, statErr := os.Stat(InRepoTomlConfigPath(repoRoot))
		require.NoError(t, statErr, "new in-repo configs are created as config.toml")
		_, statErr = os.Stat(InRepoConfigPath(repoRoot))
		assert.True(t, os.IsNotExist(statErr), "no config.json must appear alongside")

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
	})

	t.Run("an existing config.json stays JSON", func(t *testing.T) {
		// The file is checked into the user's repo; converting it out from
		// under collaborators on an older af is not af's call to make.
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"default_program": "gemini"}`)
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		_, statErr := os.Stat(InRepoTomlConfigPath(repoRoot))
		assert.True(t, os.IsNotExist(statErr), "saving into a json repo must not create config.toml")

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "gemini", cfg.DefaultProgram)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
	})

	t.Run("an existing config.toml is updated in place preserving fields", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, `
default_program = "gemini"

[remote_hooks]
launch_cmd = "l"
attach_cmd = "a"
delete_cmd = "d"
`)
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
		writeInRepoTomlConfig(t, repoRoot, `default_program = "codex"`+"\n")
		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, nil))

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.True(t, cfg.IsSet("post_worktree_commands"))
		assert.Empty(t, cfg.PostWorktreeCommands)
	})

	t.Run("writes through a symlinked config.toml", func(t *testing.T) {
		repoRoot := t.TempDir()
		target := filepath.Join(repoRoot, "shared-config.toml")
		require.NoError(t, os.WriteFile(target, []byte(`default_program = "codex"`+"\n"), 0644))
		require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0755))
		linkPath := InRepoTomlConfigPath(repoRoot)
		require.NoError(t, os.Symlink(target, linkPath))

		require.NoError(t, SaveInRepoPostWorktreeCommands(repoRoot, []string{"make setup"}))

		info, err := os.Lstat(linkPath)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "config path must still be a symlink after save")

		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"make setup"}, cfg.PostWorktreeCommands)
		assert.Equal(t, "codex", cfg.DefaultProgram)
	})
}
