package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigQuotesAliasPathWithSpacesBeforeFlags(t *testing.T) {
	// The shell probe returns the alias expansion as one string. Statting that
	// whole string fails once flags are present, but leaving it unquoted makes
	// sh split the executable path at its first space. Execute the resulting
	// override so this test pins the launch behavior, not merely formatting.
	bashPath := requireBash(t)
	homeDir := t.TempDir()
	binDir := filepath.Join(t.TempDir(), "Claude Code")
	require.NoError(t, os.MkdirAll(binDir, 0755))
	target := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755))
	writeGuardedBashrc(t, homeDir, target+" --model opus")

	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", bashPath)
	t.Setenv("PATH", t.TempDir())

	cfg := DefaultConfig()
	require.NotNil(t, cfg)
	override := cfg.ProgramOverrides[tmux.ProgramClaude]
	assert.Equal(t,
		"'"+target+"' --model opus --dangerously-skip-permissions",
		override)

	output, err := exec.Command("/bin/sh", "-c", override).CombinedOutput()
	require.NoError(t, err, "resolved override failed to launch the detected executable: %s", output)
	assert.Equal(t, "--model\nopus\n--dangerously-skip-permissions\n", string(output))
}

func TestShellQuoteDetectedCommandUsesFilesystemBoundary(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "Claude's Tools")
	require.NoError(t, os.MkdirAll(binDir, 0755))
	target := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0755))

	quoted := ShellQuotePath(target)
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "bare path", command: target, want: quoted},
		{name: "flags preserved", command: target + " --model opus", want: quoted + " --model opus"},
		{name: "tab delimiter preserved", command: target + "\t--model opus", want: quoted + "\t--model opus"},
		{name: "already quoted", command: quoted + " --model opus", want: quoted + " --model opus"},
		{name: "unproved shell command", command: "claude --model opus", want: "claude --model opus"},
		{name: "bare directory is not executable", command: binDir, want: binDir},
		{name: "directory is not executable prefix", command: binDir + " --model opus", want: binDir + " --model opus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shellQuoteDetectedCommand(tt.command))
		})
	}
}
