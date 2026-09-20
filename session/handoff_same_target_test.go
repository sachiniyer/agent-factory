package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// HandoffTargetIsCurrent is the one same-target predicate the guard, the
// account-swap admission, and both pickers share. A provable resolution
// decides; an opaque one (effective "") compares the RECORDED enum to the
// requested one — the token-scanned current identity can name a different
// agent than the enum when a wrapper's arguments spoof one (#4430 review).
func TestHandoffTargetIsCurrent(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		current, target, effective, recorded string
		want                                 bool
	}{
		{"plain enum", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, true},
		{"plain other agent", tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramClaude, false},
		{"override onto the running agent", tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramCodex, tmux.ProgramClaude, true},
		{"same name, override elsewhere", tmux.ProgramCodex, tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramCodex, false},
		{"opaque wrapper of the running enum", tmux.ProgramClaude, tmux.ProgramClaude, "", tmux.ProgramClaude, true},
		{"opaque wrapper of another enum", tmux.ProgramClaude, tmux.ProgramAider, "", tmux.ProgramClaude, false},
		// The round-6 case: an opaque wrapper whose ARGUMENTS name an agent
		// makes current token-scan to codex while the recorded enum is aider.
		// --to aider is that exact override again — a self-handoff the old
		// current==target compare permitted (#4430 review).
		{"opaque args spoof the current identity", tmux.ProgramCodex, tmux.ProgramAider, "", tmux.ProgramAider, true},
		{"unknown current with same recorded enum", "", tmux.ProgramClaude, "", tmux.ProgramClaude, true},
		{"unknown current, different recorded enum", "", tmux.ProgramClaude, "", tmux.ProgramAider, false},
		{"unknown current never matches a resolution", "", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, false},
		{"empty recorded enum cannot prove sameness", tmux.ProgramClaude, tmux.ProgramClaude, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, HandoffTargetIsCurrent(tc.current, tc.target, tc.effective, tc.recorded))
		})
	}
}

// saveProgramOverrides writes a global config whose program_overrides are
// exactly overrides, in a fresh af home.
func saveProgramOverrides(t *testing.T, overrides map[string]string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = overrides
	require.NoError(t, config.SaveConfig(cfg))
}

// A wrapper configured as claude's override is not a provable agent
// invocation, so the target resolves to "". The session behind it is still
// claude (CurrentAgentName's enum fallback), and handing it to claude again
// would stop a working agent and restart the same wrapper with no
// conversation. The resolved-only compare let exactly that through (#4430
// review); master's enum compare refused it.
func TestValidateHandoffTarget_OpaqueOverrideKeepsTheSameTargetGuard(t *testing.T) {
	const wrapper = "/home/dev/bin/agent-wrapper"
	saveProgramOverrides(t, map[string]string{
		tmux.ProgramClaude: wrapper,
		tmux.ProgramAider:  "/home/dev/bin/other-wrapper",
	})
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Tabs[0].Conversation = AgentConversationData{}
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_handoff_opaque", wrapper, nil, nil))

	require.Empty(t, HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramClaude),
		"precondition: the wrapper must be an unprovable invocation")
	require.Equal(t, tmux.ProgramClaude, inst.CurrentAgentName(),
		"precondition: the session is claude by its configured enum")

	require.ErrorContains(t, inst.ValidateHandoffTarget(tmux.ProgramClaude), "already running claude")
	_, err := inst.SwapAgentProgram(tmux.ProgramClaude, HandoffReasonManual, "abc123", false)
	require.ErrorContains(t, err, "already running claude",
		"the state mutation must refuse what the guard refuses")
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
	require.Empty(t, inst.Handoffs(), "a refused self-handoff must not reach the ledger")

	// The fallback decides sameness only: every other target stays reachable,
	// including one whose own override is just as opaque.
	require.NoError(t, inst.ValidateHandoffTarget(tmux.ProgramCodex))
	require.NoError(t, inst.ValidateHandoffTarget(tmux.ProgramAider),
		"an opaque override of a DIFFERENT enum is a real handoff")
	require.False(t, handoffTargetOffered(inst, tmux.ProgramClaude),
		"a picker fed the same resolutions must not offer the refused row")
	require.True(t, handoffTargetOffered(inst, tmux.ProgramAider))
}

// handoffTargetOffered answers what the TUI picker's filter answers for
// target, from the same inspection-scope resolution the picker reads.
func handoffTargetOffered(inst *Instance, target string) bool {
	resolved := HandoffEffectiveAgentsForPathInspection(inst.Path, []string{target})
	return !HandoffTargetIsCurrent(inst.CurrentAgentName(), target, resolved[target], inst.AgentProgram())
}

// An account-only request (no --to) names the running agent's IDENTITY as its
// agent. With claude configured to launch codex and codex configured to launch
// gemini, re-resolving that identity through codex's own override read
// `--account work` as a cross-agent gemini handoff (#4430 review). It must keep
// the recorded program; an explicit `--to codex` is still the redirect it asks
// for.
func TestManualAccountSwapProgram_AccountOnlyKeepsTheRecordedProgram(t *testing.T) {
	saveProgramOverrides(t, map[string]string{
		tmux.ProgramClaude: tmux.ProgramCodex,
		tmux.ProgramCodex:  tmux.ProgramGemini,
	})
	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("swap", tmux.ProgramCodex))}
	require.Equal(t, tmux.ProgramCodex, inst.CurrentAgentName(), "precondition: the pane runs codex")

	program, cross := inst.ManualAccountSwapProgram(inst.CurrentAgentName(), true)
	require.False(t, cross, "an account-only request never changes the agent")
	require.Equal(t, tmux.ProgramClaude, program)
	require.Equal(t, tmux.ProgramCodex, HandoffEffectiveAgentForPath(inst.Path, program),
		"the account namespace is the running codex, not codex's override")

	program, cross = inst.ManualAccountSwapProgram(tmux.ProgramCodex, false)
	require.True(t, cross, "an explicit --to codex resolves to gemini — a real cross-agent handoff")
	require.Equal(t, tmux.ProgramCodex, program)

	program, cross = inst.ManualAccountSwapProgram(tmux.ProgramClaude, false)
	require.False(t, cross, "--to claude resolves to the running codex — the same-agent account change")
	require.Equal(t, tmux.ProgramClaude, program)
}

// The same account-only request through the real launch proof: the frozen
// plan must launch the recorded program's command (codex) under the codex
// account, not codex's override (gemini). Re-deriving cross-agent inside the
// validator is what launched gemini before (#4430 review).
func TestValidateManualAccountSwap_AccountOnlyLaunchesTheRecordedProgram(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{tmux.ProgramCodex, tmux.ProgramGemini} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	agentHome(t)
	grantGlobalAgentSkills(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramClaude: tmux.ProgramCodex,
		tmux.ProgramCodex:  tmux.ProgramGemini,
	}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramCodex, "work")

	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("swap", tmux.ProgramCodex))}
	inst.setRuntimeProgram(tmux.ProgramCodex)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// The runtime evidence is recorded at launch; moving the override
	// afterwards must not re-route an account-only swap.
	cfg.ProgramOverrides[tmux.ProgramClaude] = tmux.ProgramGemini
	require.NoError(t, config.SaveConfig(cfg))

	agent := inst.CurrentAgentName()
	_, cross := inst.ManualAccountSwapProgram(agent, true)
	require.NoError(t, inst.ValidateManualAccountSwap("work", agent, cross))
	require.NotNil(t, inst.accountSwapLaunch)
	launched := inst.accountSwapLaunch.program
	require.True(t, strings.HasPrefix(launched, tmux.ProgramCodex),
		"the account-only swap must relaunch the running codex, got %q", launched)
	require.NotContains(t, launched, tmux.ProgramGemini)
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
		"validation must not rewrite the outgoing runtime identity")
}

// The commit takes crossAgent from admission instead of re-deriving it. If
// the configured override has moved since the pane launched, the namespace
// admission proved (gemini) no longer equals the running identity (codex);
// re-deriving sameness from those two turned an account-only swap into a
// Program rewrite to the identity enum, so the next restart would launch
// codex's override instead of the configured claude command (#4430 review).
func TestSelectAccountForHandoff_SameAgentKeepsProgramDespiteNamespaceDrift(t *testing.T) {
	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("swap", tmux.ProgramCodex))}

	entry, err := inst.SelectAccountForHandoff("", "work", tmux.ProgramCodex, tmux.ProgramGemini, false,
		HandoffReasonManual, "head", "continue")
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
		"a same-agent account swap retains the configured program")
	require.Equal(t, tmux.ProgramCodex, entry.To, "the ledger still names the running identity")
	require.Equal(t, "work", inst.Account)

	// Canary: a cross-agent commit still rewrites Program to its target.
	cross := accountSwapTestInstance(tmux.ProgramClaude)
	_, err = cross.SelectAccountForHandoff("", "work", tmux.ProgramCodex, tmux.ProgramCodex, true,
		HandoffReasonManual, "head", "continue")
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, cross.AgentProgram())
}
