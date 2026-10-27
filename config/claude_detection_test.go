package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestGetClaudeCommandMemoized pins the #883 fix: repeated probes that share
// the same SHELL/PATH/HOME must source the rc only once, while a changed HOME
// re-probes under a new cache key. A heavy interactive rc otherwise ran the
// bash probe up to four times per TUI startup.
func TestGetClaudeCommandMemoized(t *testing.T) {
	bashPath := requireBash(t)

	// A .bashrc that records every interactive sourcing into a marker file and
	// defines a claude alias, so the probe both succeeds and is countable.
	writeCountingBashrc := func(homeDir, marker string) {
		bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
			"echo x >> '" + marker + "'\n" +
			"alias claude='/custom/bin/claude'\n"
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))
	}
	sourceCount := func(marker string) int {
		data, err := os.ReadFile(marker)
		if os.IsNotExist(err) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(data), "x")
	}

	home1 := t.TempDir()
	marker1 := filepath.Join(t.TempDir(), "sourced")
	writeCountingBashrc(home1, marker1)

	t.Setenv("SHELL", bashPath)
	t.Setenv("PATH", t.TempDir()) // no claude on PATH — the alias is the only source
	t.Setenv("HOME", home1)

	for range 3 {
		result, err := GetClaudeCommand()
		require.NoError(t, err)
		assert.Equal(t, "/custom/bin/claude", result)
	}
	assert.Equal(t, 1, sourceCount(marker1), "stable env should probe (source the rc) exactly once")

	// Changing HOME must invalidate the cache and probe again.
	home2 := t.TempDir()
	marker2 := filepath.Join(t.TempDir(), "sourced")
	writeCountingBashrc(home2, marker2)
	t.Setenv("HOME", home2)

	result, err := GetClaudeCommand()
	require.NoError(t, err)
	assert.Equal(t, "/custom/bin/claude", result)
	assert.Equal(t, 1, sourceCount(marker2), "a changed HOME should re-probe under a new cache key")
}

func TestLoadConfigLogsMissingClaudeOnce(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	// A fresh AF home: the first load materializes config.toml, the rest parse
	// it — the warning's two call sites both converge on the one emission.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)
	errors := captureLog(t, &log.ErrorLog)

	for range 5 {
		_, err := LoadConfig()
		require.NoError(t, err)
	}

	assert.Equal(t, 1, strings.Count(warnings.String()+errors.String(), "\n"),
		"five config reads should produce exactly one log line for an absent optional binary")
	assert.Empty(t, errors.String(), "an absent optional binary is not an error")
	assert.Contains(t, warnings.String(), "optional")
	assert.Contains(t, warnings.String(), "program_overrides.claude")
	assert.Contains(t, warnings.String(), "another program")
}

// TestDefaultConfigDoesNotWarnOnMissingClaude pins the #5588 split: bare
// defaults never emit load advice, because whether a configured
// program_overrides.claude exists is only decidable by the loaders that merge
// the operator's file.
func TestDefaultConfigDoesNotWarnOnMissingClaude(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)

	DefaultConfig()

	assert.Empty(t, warnings.String(),
		"DefaultConfig answers defaults, not advice — the loaders emit it once the effective overrides are known")
}

// TestLoadConfigMissingClaudeWarning covers both halves of #5588: the advice
// must appear when nothing configures a claude program, and must NOT appear
// when program_overrides.claude is already set — telling the operator to apply
// a fix they already applied makes a correct configuration look broken.
func TestLoadConfigMissingClaudeWarning(t *testing.T) {
	writeToml := func(t *testing.T, content string) {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(content), 0644))
	}
	// A probe that always misses: /bin/sh cannot see aliases and claude is
	// nowhere on PATH.
	missClaude := func(t *testing.T) {
		t.Helper()
		t.Setenv("SHELL", "/bin/sh")
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", t.TempDir())
	}

	t.Run("silent when program_overrides.claude is already set", func(t *testing.T) {
		writeToml(t, "[program_overrides]\nclaude = \"/opt/stand-in\"\n")
		missClaude(t)
		warnings := captureLog(t, &log.WarningLog)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "/opt/stand-in", cfg.ProgramOverrides[tmux.ProgramClaude])
		assert.NotContains(t, warnings.String(), "program_overrides.claude",
			"the advice must not tell the operator to apply a fix they already applied")
		assert.NotContains(t, warnings.String(), "another program")
	})

	t.Run("advises program_overrides.claude when nothing configures it", func(t *testing.T) {
		writeToml(t, "default_program = \"claude\"\n")
		missClaude(t)
		warnings := captureLog(t, &log.WarningLog)

		_, err := LoadConfig()
		require.NoError(t, err)
		assert.Contains(t, warnings.String(), "optional")
		assert.Contains(t, warnings.String(), "program_overrides.claude")
		assert.Contains(t, warnings.String(), "another program")
	})
}

func TestGetClaudeCommandReprobesAfterPATHChange(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	_, err := GetClaudeCommand()
	require.Error(t, err)

	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0755))
	t.Setenv("PATH", binDir)

	result, err := GetClaudeCommand()
	require.NoError(t, err)
	assert.Equal(t, claudePath, result)
}

func TestGetClaudeCommandCoalescesConcurrentMissingProbe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probes")
	t.Setenv("AF_CLAUDE_PROBE_MARKER", marker)
	fakeShell := filepath.Join(t.TempDir(), "probe-shell")
	require.NoError(t, os.WriteFile(fakeShell, []byte(
		"#!/bin/sh\nprintf x >> \"$AF_CLAUDE_PROBE_MARKER\"\n/bin/sleep 0.2\nexit 1\n",
	), 0755))
	t.Setenv("SHELL", fakeShell)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := GetClaudeCommand()
			errs <- err
		}()
	}
	close(start)
	for range callers {
		require.Error(t, <-errs)
	}

	probes, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(probes), "x"), "one environment should run one probe")
}

// TestWarnMissingClaudeOverrideCoalescesConcurrentCallers is the #5588 version
// of the coalescing pin: the warning moved off the probe onto the effective
// overrides, and concurrent first callers still must not duplicate it (#3999).
func TestWarnMissingClaudeOverrideCoalescesConcurrentCallers(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)

	const callers = 8
	start := make(chan struct{})
	done := make(chan struct{}, callers)
	for range callers {
		go func() {
			<-start
			warnMissingClaudeOverride(nil)
			done <- struct{}{}
		}()
	}
	close(start)
	for range callers {
		<-done
	}

	assert.Equal(t, 1, strings.Count(warnings.String(), "\n"), "one failed probe should emit one warning")
	assert.Contains(t, warnings.String(), "program_overrides.claude")
}
