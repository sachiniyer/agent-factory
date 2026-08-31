package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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

// TestDefaultConfigWithExecPrefixAlias pins the end-to-end path that #3108 fixes:
// a shell alias that begins with the `exec` builtin (`alias
// claude='exec /opt/claude'`) is captured VERBATIM by the detector (the probe
// does not strip `exec`), so program_overrides carries the prefix through to
// the launcher. Before the fix, GenerateAccountLaunchProof kept `exec` as its
// first token, never set TrustedExecutable, and ValidateAccountCommand refused
// the absolute path. This test asserts both halves: the detector passes
// `exec` through unchanged, AND the proof/validation pair accepts the
// account-scoped launch the launcher would actually run.
func TestDefaultConfigWithExecPrefixAlias(t *testing.T) {
	bashPath := requireBash(t)
	homeDir := t.TempDir()
	binDir := filepath.Join(t.TempDir(), "opt")
	require.NoError(t, os.MkdirAll(binDir, 0755))
	target := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755))

	// `exec` replaces the shell with the agent; a legitimate alias shape that
	// reaches program_overrides verbatim through the detector.
	writeGuardedBashrc(t, homeDir, "exec "+target)

	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", bashPath)
	t.Setenv("PATH", t.TempDir())

	cfg := DefaultConfig()
	require.NotNil(t, cfg)
	override := cfg.ProgramOverrides[tmux.ProgramClaude]

	// Detection does NOT strip the `exec` prefix — it passes through unchanged,
	// so the override an account-scoped launch receives begins with `exec`.
	assert.Equal(t, "exec "+target+" "+DetectedClaudePermissionsFlag, override,
		"the detector passes the exec prefix through verbatim; the fix is on the proof side, not the detection side")

	// The override as the pane would actually run it: /bin/sh -c <override>.
	// `exec` replaces the shell with the detected executable, which prints $@.
	output, err := exec.Command("/bin/sh", "-c", override).CombinedOutput()
	require.NoError(t, err, "exec-prefixed override failed to launch: %s", output)
	assert.Equal(t, DetectedClaudePermissionsFlag+"\n", string(output))

	// Now feed the SAME override through the proof + validation pair the
	// launcher uses (session.accountLaunchProof semantics with trustBase=true
	// for an exact built-in match), simulating af appending its generated args.
	const sessionID = "0b6f2c1e-8a44-4d0e-9f31-7c5b2a9e4d10"
	const pluginDir = "/home/op/.local/share/agent-factory/plugins/af"
	base := override
	final := base + " --session-id " + sessionID + " --plugin-dir " + pluginDir

	proof, ok := sessionenv.GenerateAccountLaunchProof(base, final, []string{DetectedClaudePermissionsFlag})
	require.True(t, ok, "proof generation must succeed for an exec-prefixed built-in base")
	require.Equal(t, target, proof.TrustedExecutable,
		"the exec prefix must be stripped so the absolute detected executable is trusted")
	require.Equal(t,
		[]string{DetectedClaudePermissionsFlag, "--session-id", sessionID, "--plugin-dir", pluginDir},
		proof.GeneratedArgs)

	err = sessionenv.ValidateAccountCommand(final, sessionenv.Account{
		Agent:             "claude",
		Name:              "work",
		Dir:               "/afhome/accounts/claude/work",
		TrustedExecutable: proof.TrustedExecutable,
		GeneratedArgs:     proof.GeneratedArgs,
	})
	require.NoError(t, err,
		"the producer (proof) and consumer (validation) must tokenize exec identically, so a valid "+
			"account-scoped session is accepted rather than refused with an unexplained 127")
}
