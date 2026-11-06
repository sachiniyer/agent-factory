package session

// Regression coverage for the #6769 recording guarantee under a
// program_overrides configuration that launches codex under another agent's
// name (e.g. Program: "aider" with the pane running codex). The create-time
// capture gate and SessionCodexHomeDir both key off the resolved command, so
// the deferred/respawn baseline must too — otherwise a daemon restart or a
// refused-resume respawn drops the rollout id and a later recovery starts a
// fresh conversation, losing the user's thread.

import (
	"testing"

	"github.com/stretchr/testify/require"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// overrideScenarioInstance is the shape the bug report describes: a local
// worktree session whose configured enum is a non-codex agent ("aider") but
// whose runtime evidence shows the pane actually launched codex — exactly what
// a program_overrides entry pointing "aider" at the codex binary produces.
// retireTestInstance (Program: codex) cannot represent this case, so the
// baseline tests in conversation_retire_test.go never exercised the conjunct.
func overrideScenarioInstance(t *testing.T, codexHome string) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := initTempGitRepo(t)
	inst, err := NewInstance(InstanceOptions{Title: "codex-under-aider", Path: repoPath, Program: "aider"})
	require.NoError(t, err)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	// Runtime evidence: the pane is running codex against an explicit store.
	// ResolvedAgent() reads this and reports codex; AgentProgram() still
	// reports the configured enum "aider".
	inst.setRuntimeProgram("CODEX_HOME=" + codexHome + " codex")
	return inst
}

// TestSessionCodexCaptureBaselineOverrideScenario pins the deferred-arm
// baseline for a codex-under-another-name session: the snapshot must capture
// codex and bind the store the running command declares, exactly as
// SessionCodexHomeDir (the sibling resolver in the same file) already does.
// Before the fix the configured-enum conjunct returned an empty snapshot for
// this instance even though ResolvedAgent() == codex.
func TestSessionCodexCaptureBaselineOverrideScenario(t *testing.T) {
	codexHome := t.TempDir()
	inst := overrideScenarioInstance(t, codexHome)

	require.Equal(t, tmux.ProgramCodex, inst.ResolvedAgent(),
		"precondition: the running command resolves to codex")
	require.NotEqual(t, tmux.ProgramCodex, inst.AgentProgram(),
		"precondition: the configured enum is NOT codex — this is the override case")
	require.NotEmpty(t, inst.SessionCodexHomeDir(),
		"SessionCodexHomeDir already resolves the override case via ResolvedAgent")

	snap := inst.SessionCodexCaptureBaseline()
	require.True(t, snap.CapturesCodex(),
		"a codex-under-another-name session must baseline its store so a daemon restart records the rollout")
	require.Equal(t, codexHome, snap.codexHome,
		"the baseline must watch the store the running command declares, not the ambient one")
}

// TestRespawnBaselineOverrideScenario pins the pre-spawn baseline the respawn
// path builds via sessionCodexCaptureBaselineFor(resolvedProgram) before a
// refused-resume fresh spawn. The same conjunct governs this entry point, so
// the override case must baseline here too — otherwise the replacement's
// rollout is excluded from its own discovery and the thread is lost.
func TestRespawnBaselineOverrideScenario(t *testing.T) {
	codexHome := t.TempDir()
	inst := overrideScenarioInstance(t, codexHome)

	resolvedProgram := "CODEX_HOME=" + codexHome + " codex"
	snap := inst.sessionCodexCaptureBaselineFor(resolvedProgram)
	require.True(t, snap.CapturesCodex(),
		"the respawn pre-spawn baseline must capture codex for a codex-under-another-name session")
	require.Equal(t, codexHome, snap.codexHome,
		"the respawn baseline must bind the store the resolved command declares")
}

// TestSessionCodexCaptureBaselineStillRefusesAGenuinelyNonCodexSession is the
// control: the fix admits the override case (command resolves to codex) but
// must still refuse a session whose runtime evidence is genuinely non-codex,
// even when the configured enum happens to be codex. This confirms the first
// conjunct (DetectAgentFromCommand) still guards the gate.
func TestSessionCodexCaptureBaselineStillRefusesAGenuinelyNonCodexSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	repoPath := initTempGitRepo(t)
	inst, err := NewInstance(InstanceOptions{Title: "claude-session", Path: repoPath, Program: tmux.ProgramCodex})
	require.NoError(t, err)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	// Runtime evidence is claude, not codex: the command does not resolve to
	// codex, so the first conjunct refuses regardless of the enum.
	inst.setRuntimeProgram("claude --session-id abc")

	require.NotEqual(t, tmux.ProgramCodex, inst.ResolvedAgent(),
		"precondition: the running command resolves to claude, not codex")
	snap := inst.SessionCodexCaptureBaseline()
	require.False(t, snap.CapturesCodex(),
		"a session whose runtime command is not codex must not baseline a codex store")
}
