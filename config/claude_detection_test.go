package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
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

func TestDefaultConfigLogsMissingClaudeOnce(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)
	errors := captureLog(t, &log.ErrorLog)

	for range 5 {
		DefaultConfig()
	}

	assert.Equal(t, 1, strings.Count(warnings.String()+errors.String(), "\n"),
		"five config reads should produce exactly one log line for an absent optional binary")
	assert.Empty(t, errors.String(), "an absent optional binary is not an error")
	assert.Contains(t, warnings.String(), "optional")
	assert.Contains(t, warnings.String(), "program_overrides.claude")
	assert.Contains(t, warnings.String(), "another program")
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
