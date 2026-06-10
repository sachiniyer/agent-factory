package config

import (
	"bytes"
	stdlog "log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// setupResolveTest gives the test a hermetic config home with an explicit
// global config (so LoadConfig never materializes machine-detected defaults
// into assertions) and a fresh repo root.
func setupResolveTest(t *testing.T, globalConfig string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// LoadConfig probes the user's shell for a claude alias on every call;
	// an interactive bash probe costs seconds. A plain sh takes the fast
	// `which` path and keeps the resolver tests snappy.
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), []byte(globalConfig), 0644))
	return t.TempDir()
}

// captureLog redirects the given project logger to a buffer for the test.
func captureLog(t *testing.T, logger **stdlog.Logger) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := *logger
	*logger = stdlog.New(&buf, "", 0)
	t.Cleanup(func() { *logger = old })
	return &buf
}

func TestResolveConfigPrecedence(t *testing.T) {
	t.Run("global applies when no in-repo file", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "codex"}`)
		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "codex", res.DefaultProgram)
	})

	t.Run("in-repo default_program overrides global", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "codex"}`)
		writeInRepoConfig(t, repoRoot, `{"default_program": "aider"}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "aider", res.DefaultProgram)
	})

	t.Run("in-repo file without default_program keeps global", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "codex"}`)
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": ["true"]}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "codex", res.DefaultProgram)
	})

	t.Run("program_overrides merge key-wise", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude", "program_overrides": {"codex": "/opt/codex", "aider": "/opt/aider"}}`)
		writeInRepoConfig(t, repoRoot, `{"program_overrides": {"codex": "/repo/codex --fast", "gemini": "/repo/gemini"}}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, "/repo/codex --fast", res.ProgramOverrides["codex"], "in-repo entry wins per key")
		assert.Equal(t, "/opt/aider", res.ProgramOverrides["aider"], "global entry without in-repo counterpart survives")
		assert.Equal(t, "/repo/gemini", res.ProgramOverrides["gemini"], "in-repo-only entry applies")
	})

	t.Run("global-only fields always come from global", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude", "auto_yes": true, "branch_prefix": "team/", "detach_keys": "ctrl-q"}`)
		writeInRepoConfig(t, repoRoot, `{"default_program": "gemini"}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.True(t, res.AutoYes)
		assert.Equal(t, "team/", res.BranchPrefix)
		assert.Equal(t, "ctrl-q", res.DetachKeys)
	})

	t.Run("in-repo validation errors propagate", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		writeInRepoConfig(t, repoRoot, `{"auto_yes": true}`)

		_, err := ResolveConfig(repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "auto_yes")
	})
}

func TestResolveConfigRepoFields(t *testing.T) {
	hooks := &RemoteHooks{LaunchCmd: "l", ListCmd: "ls", AttachCmd: "a", DeleteCmd: "d"}

	t.Run("in-repo values apply", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": ["npm ci"], "remote_hooks": {"launch_cmd": "l2", "attach_cmd": "a2", "delete_cmd": "d2"}}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"npm ci"}, res.PostWorktreeCommands)
		require.NotNil(t, res.RemoteHooks)
		assert.Equal(t, "l2", res.RemoteHooks.LaunchCmd)
	})

	t.Run("legacy location still works as fallback", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		repoID := RepoIDFromRoot(repoRoot)
		require.NoError(t, SaveRepoConfig(repoID, &RepoConfig{
			PostWorktreeCommands: []string{"legacy-cmd"},
			RemoteHooks:          hooks,
		}))

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"legacy-cmd"}, res.PostWorktreeCommands)
		require.NotNil(t, res.RemoteHooks)
		assert.Equal(t, "l", res.RemoteHooks.LaunchCmd)
	})

	t.Run("in-repo shadows legacy", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		repoID := RepoIDFromRoot(repoRoot)
		require.NoError(t, SaveRepoConfig(repoID, &RepoConfig{
			PostWorktreeCommands: []string{"legacy-cmd"},
			RemoteHooks:          hooks,
		}))
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": ["new-cmd"], "remote_hooks": {"launch_cmd": "l2", "attach_cmd": "a2", "delete_cmd": "d2"}}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Equal(t, []string{"new-cmd"}, res.PostWorktreeCommands)
		assert.Equal(t, "l2", res.RemoteHooks.LaunchCmd)
	})

	t.Run("explicit empty in-repo key disables legacy value", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		repoID := RepoIDFromRoot(repoRoot)
		require.NoError(t, SaveRepoConfig(repoID, &RepoConfig{PostWorktreeCommands: []string{"legacy-cmd"}}))
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": []}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Empty(t, res.PostWorktreeCommands)
	})
}

// TestResolveConfigResolvesHookPaths covers the #834 chokepoint: relative
// remote_hooks command paths leave ResolveConfig as absolute paths under the
// repo root, regardless of which config location supplied them, while
// absolute paths and bare $PATH names pass through untouched.
func TestResolveConfigResolvesHookPaths(t *testing.T) {
	t.Run("in-repo relative paths resolve against repo root", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		writeInRepoConfig(t, repoRoot, `{"remote_hooks": {
			"launch_cmd": "./.agent-factory/hooks/launch.sh",
			"list_cmd": "infra/list.sh",
			"attach_cmd": "/abs/attach.sh",
			"delete_cmd": "bash"
		}}`)

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, res.RemoteHooks)
		assert.Equal(t, filepath.Join(repoRoot, ".agent-factory/hooks/launch.sh"), res.RemoteHooks.LaunchCmd)
		assert.Equal(t, filepath.Join(repoRoot, "infra/list.sh"), res.RemoteHooks.ListCmd)
		assert.Equal(t, "/abs/attach.sh", res.RemoteHooks.AttachCmd, "absolute path passes through")
		assert.Equal(t, "bash", res.RemoteHooks.DeleteCmd, "bare name keeps $PATH lookup")
	})

	t.Run("legacy-location relative paths get the same resolution", func(t *testing.T) {
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		repoID := RepoIDFromRoot(repoRoot)
		require.NoError(t, SaveRepoConfig(repoID, &RepoConfig{
			RemoteHooks: &RemoteHooks{
				LaunchCmd: "./hooks/launch.sh",
				ListCmd:   "/abs/list.sh",
				AttachCmd: "hooks/attach.sh",
				DeleteCmd: "coder-delete",
			},
		}))

		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, res.RemoteHooks)
		assert.Equal(t, filepath.Join(repoRoot, "hooks/launch.sh"), res.RemoteHooks.LaunchCmd)
		assert.Equal(t, "/abs/list.sh", res.RemoteHooks.ListCmd)
		assert.Equal(t, filepath.Join(repoRoot, "hooks/attach.sh"), res.RemoteHooks.AttachCmd)
		assert.Equal(t, "coder-delete", res.RemoteHooks.DeleteCmd)

		// The rewrite operates on a copy; a fresh load of the legacy config
		// still sees the values the user wrote.
		legacy, err := LoadRepoConfig(repoID)
		require.NoError(t, err)
		assert.Equal(t, "./hooks/launch.sh", legacy.RemoteHooks.LaunchCmd)
	})
}

func TestResolveConfigLegacyDeprecationLog(t *testing.T) {
	buf := captureLog(t, &aflog.WarningLog)
	repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
	repoID := RepoIDFromRoot(repoRoot)
	require.NoError(t, SaveRepoConfig(repoID, &RepoConfig{PostWorktreeCommands: []string{"legacy-cmd"}}))

	_, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "deprecated")
	assert.Contains(t, buf.String(), "post_worktree_commands")
	assert.Contains(t, buf.String(), InRepoConfigPath(repoRoot))

	// Once per repo per process: a second resolve stays quiet.
	buf.Reset()
	_, err = ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "deprecated")
}

func TestResolveConfigInRepoLoadLog(t *testing.T) {
	t.Run("command-bearing config logs once and again on change", func(t *testing.T) {
		buf := captureLog(t, &aflog.InfoLog)
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": ["npm ci"]}`)

		_, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Contains(t, buf.String(), "loaded in-repo config for "+repoRoot)
		assert.Contains(t, buf.String(), "post_worktree_commands")

		// Same content: no re-log.
		buf.Reset()
		_, err = ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.NotContains(t, buf.String(), "loaded in-repo config")

		// Changed content: logs again with the new field list.
		writeInRepoConfig(t, repoRoot, `{"post_worktree_commands": ["npm ci"], "remote_hooks": {"launch_cmd": "l", "attach_cmd": "a", "delete_cmd": "d"}}`)
		_, err = ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.Contains(t, buf.String(), "loaded in-repo config")
		assert.Contains(t, buf.String(), "remote_hooks")
	})

	t.Run("preference-only config does not log", func(t *testing.T) {
		buf := captureLog(t, &aflog.InfoLog)
		repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
		writeInRepoConfig(t, repoRoot, `{"default_program": "codex"}`)

		_, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		assert.NotContains(t, buf.String(), "loaded in-repo config")
	})
}

// TestResolveConfigDoesNotMutateGlobal guards the map copy in ResolveConfig:
// merging in-repo program_overrides must never write through to the global
// config that LoadConfig callers see.
func TestResolveConfigDoesNotMutateGlobal(t *testing.T) {
	repoRoot := setupResolveTest(t, `{"default_program": "claude", "program_overrides": {"codex": "/opt/codex"}}`)
	writeInRepoConfig(t, repoRoot, `{"program_overrides": {"codex": "/repo/codex"}}`)

	_, err := ResolveConfig(repoRoot)
	require.NoError(t, err)

	global, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "/opt/codex", global.ProgramOverrides["codex"])
}
