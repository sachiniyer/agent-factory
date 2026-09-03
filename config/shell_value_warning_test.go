package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// The three sources that carry an operator-authored value into `/bin/sh -c`
// each have their OWN validation entry, and a warning wired into only the
// global one would miss exactly the case a repo-level or per-project
// program_overrides creates (#3566). These tests exercise the real loaders, not
// the shared helper, so a call site deleted from any one of them goes red.

// execSeparatorWarning asserts that out carries the load-time warning for key,
// and that it stayed a warning: nothing here may become an error.
func assertExecSeparatorWarning(t *testing.T, out, key string) {
	t.Helper()
	require.Contains(t, out, key, "the warning must name the key the operator has to edit")
	require.Contains(t, out, "`exec --`", "the warning must name the shape it found")
	require.Contains(t, out, "exec: --: not found",
		"the warning must quote the failure the operator will otherwise see, as the account refusal does")
	require.Contains(t, out, "warning, not an error",
		"a value that is correct under bash or busybox ash must not read as a rejection")
}

func TestExecSeparatorWarning_GlobalConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude --resume\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
		assert.Contains(t, warnings.String(), "global.toml", "the warning must name the file to edit")
	})

	t.Run("on_archive_command", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("on_archive_command = \"exec -- ./notify.sh\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "on_archive_command")
	})

	t.Run("sandbox.ssh", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[sandbox]\nssh = \"exec -- ssh sandbox.example\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "sandbox.ssh")
	})
}

func TestExecSeparatorWarning_InRepoConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\nclaude = \"exec -- claude\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("post_worktree_commands entry names its index", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot,
			"post_worktree_commands = [\"npm install\", \"exec -- make setup\"]\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "post_worktree_commands[1]")
		assert.NotContains(t, warnings.String(), "post_worktree_commands[0]",
			"the clean entry must not be named; the operator has to find the one to edit")
	})
}

func TestExecSeparatorWarning_ProjectPersonalConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"exec -- claude\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("on_archive_command", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "on_archive_command = \"exec -- ./notify.sh\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "on_archive_command")
	})
}

// TestExecSeparatorWarning_LeavesEveryOtherValueAlone pins the negative side.
// The warning fires on ONE shape; a plain `exec` prefix is the very rewrite the
// message asks for, and must not be warned about in turn.
func TestExecSeparatorWarning_LeavesEveryOtherValueAlone(t *testing.T) {
	for name, value := range map[string]string{
		"plain command":             "claude --resume",
		"exec without a separator":  "exec claude --resume",
		"a literal -- as an arg":    "claude -- --resume",
		"exec of a path":            "exec /usr/local/bin/claude",
		"separator inside a string": "claude --system-prompt 'exec -- x'",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			assert.Empty(t, warnings.String(), "value %q must load silently", value)
		})
	}
}

// TestWarnedShellValueKeys_EachOneActuallyWarns binds warnedShellValueKeys to
// the loaders. The list is the drift gate's vocabulary — a gate entry may only
// claim a key that appears in it — so a key that stopped being inspected (a
// deleted call site, a renamed field) has to fail here rather than keep the
// gate quietly vouching for coverage that no longer exists.
func TestWarnedShellValueKeys_EachOneActuallyWarns(t *testing.T) {
	// Each key is proven through a REAL loader, one per source, so "the key is
	// in the list" can never be satisfied by the list alone.
	proofs := map[string]func(t *testing.T) string{
		"program_overrides": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"on_archive_command": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("on_archive_command = \"exec -- ./notify.sh\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"sandbox.ssh": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[sandbox]\nssh = \"exec -- ssh host\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"post_worktree_commands": func(t *testing.T) string {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, "post_worktree_commands = [\"exec -- make setup\"]\n")
			warnings := captureLog(t, &aflog.WarningLog)
			_, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err)
			return warnings.String()
		},
	}

	for _, key := range warnedShellValueKeys {
		proof, ok := proofs[key]
		require.Truef(t, ok, "%s is declared warned but has no loader proof here; add one or drop the key", key)
		t.Run(key, func(t *testing.T) {
			assertExecSeparatorWarning(t, proof(t), key)
		})
	}
	require.Len(t, proofs, len(warnedShellValueKeys),
		"a proof with no declared key means the gate's vocabulary is missing a key it should carry")
}

// quoteTOML renders a Go string as a TOML basic string, so a test value may
// carry the single quotes a shell command naturally contains.
func quoteTOML(s string) string {
	return fmt.Sprintf("%q", s)
}
