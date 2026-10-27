package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// autoSwapClaudeInstance is a limit-blocked claude session whose recorded
// runtime program is runtime, with the given program_overrides.claude saved.
func autoSwapClaudeInstance(t *testing.T, override, runtime string) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: override}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramClaude, "work")
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramClaude)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.mu.Lock()
	inst.setRuntimeEvidenceLocked(runtime, 0, 0)
	inst.mu.Unlock()
	return inst
}

func fakeDetectedClaude(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return claudePath
}

// #5623: the automatic swap's base is the recorded runtime program. When that is
// exactly af's detected claude, the flag af appended is af-authored and must not
// be refused as an undeclared argument.
func TestValidateAccountSwapAdmitsDetectedClaudeOwnFlagFromRuntimeProgram(t *testing.T) {
	claudePath := fakeDetectedClaude(t)
	program := claudePath + " " + config.DetectedClaudePermissionsFlag
	inst := autoSwapClaudeInstance(t, program, program)
	require.NoError(t, inst.ValidateAccountSwap("work"))
}

// The same flag riding a program that is NOT af's detected built-in (an override
// with another argument, or a different executable) stays refused.
func TestValidateAccountSwapRefusesOverrideWithOtherUndeclaredArgument(t *testing.T) {
	claudePath := fakeDetectedClaude(t)
	for name, program := range map[string]string{
		"extra argument":       claudePath + " " + config.DetectedClaudePermissionsFlag + " --settings /repo/x.json",
		"flag only, other exe": "/repo/tools/claude " + config.DetectedClaudePermissionsFlag,
	} {
		t.Run(name, func(t *testing.T) {
			inst := autoSwapClaudeInstance(t, program, program)
			err := inst.ValidateAccountSwap("work")
			require.Error(t, err)
		})
	}
}
