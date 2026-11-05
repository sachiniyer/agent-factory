package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// committedRetryClaudeInstance builds the post-commit, pre-replacement restart
// state for a same-agent claude account swap whose frozen incoming command
// carries af's own --dangerously-skip-permissions flag — the production shape
// config.DefaultConfig() freezes into program_overrides.claude. The session
// has already committed the identity checkpoint (Account, accountAgent, and
// pending.Program are durable), the replacement pane never launched, and the
// pane still runs the OUTGOING agent — the exact state a daemon restart between
// commit and replacement launch lands on.
func committedRetryClaudeInstance(t *testing.T, override string) (*Instance, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: override}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramClaude, "work")

	inst := accountSwapTestInstanceWithProgram(tmux.ProgramClaude)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// The admission-time launch plan is what commit copies into the durable
	// record: base is the resolved incoming command before conversation
	// injection. The plan's proof was computed with correct trustBase at
	// admission; a restart loses this in-memory plan, and the retry must
	// re-derive provenance from the frozen command alone.
	inst.accountSwapLaunch = &accountSwapLaunchPlan{
		account: "work", base: override, program: override,
	}
	_, err = inst.SelectAccountForHandoff("", "work", tmux.ProgramClaude, tmux.ProgramClaude, false,
		HandoffReasonManual, "head", "")
	require.NoError(t, err)
	require.Equal(t, override, inst.ToInstanceData().PendingAccountSwap.Program,
		"the committed incoming command must be durable beside the namespace")

	// The restart's view: the identity checkpoint landed, the replacement
	// never launched, so the pane still runs the outgoing claude. The
	// in-memory accountSwapLaunch is lost — only the durable pending record
	// survives.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
	inst.accountSwapLaunch = nil
	return inst, target
}

// TestValidateAccountSwapCommittedRetryAdmitsFlaggedClaudeOverride is the
// regression for the committed-retry wedge: a same-agent claude account swap
// whose frozen incoming command carries af's own --dangerously-skip-permissions
// flag (the production shape from config.DefaultConfig()) must succeed on
// retry after a daemon restart, not be refused forever with "undeclared
// arguments". The committed-retry branch re-derives trustBase from the frozen
// command's built-in provenance the same way the established-evidence branch
// does, so the proof declares af's own flag and the account boundary accepts
// the launch.
func TestValidateAccountSwapCommittedRetryAdmitsFlaggedClaudeOverride(t *testing.T) {
	claudePath := fakeDetectedClaude(t)
	flagged := claudePath + " " + config.DetectedClaudePermissionsFlag
	inst, _ := committedRetryClaudeInstance(t, flagged)

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"the committed retry must re-derive trustBase for the frozen flagged command")
	require.NotNil(t, inst.accountSwapLaunch,
		"the retry must record a launch plan")
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.True(t, strings.HasPrefix(plan.program, claudePath),
		"the retry must relaunch the flagged claude command, got %q", plan.program)
	require.Contains(t, plan.proof.GeneratedArgs, config.DetectedClaudePermissionsFlag,
		"the proof must declare af's own flag as af-authored")
}

// TestValidateAccountSwapCommittedRetryBareBaseStillSucceeds confirms the fix
// does not regress the bare-base case that always worked: a committed retry
// whose frozen command is a bare agent word has no extra words to declare, so
// the proof describes only af's appended conversation words and the swap
// succeeds regardless of trustBase.
func TestValidateAccountSwapCommittedRetryBareBaseStillSucceeds(t *testing.T) {
	inst, _ := committedRetryClaudeInstance(t, tmux.ProgramClaude)

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"a bare-base committed retry must still succeed")
	require.NotNil(t, inst.accountSwapLaunch)
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.True(t, strings.HasPrefix(plan.program, tmux.ProgramClaude),
		"the retry must relaunch claude, got %q", plan.program)
}

// TestValidateAccountSwapCommittedRetryFlaggedBaseWithPostInjectionPane
// confirms the fix works even when the pane's frozen program carries af's
// injected words (the post-injection shape a live pane records). The
// pane-declaration seeding gate cannot fire here because pending.Program is
// the pre-injection base while the pane records its program post-injection, so
// the exact-match gate never equals — the fix's trustBase re-derivation is
// the only mechanism that can declare the frozen flag.
func TestValidateAccountSwapCommittedRetryFlaggedBaseWithPostInjectionPane(t *testing.T) {
	claudePath := fakeDetectedClaude(t)
	flagged := claudePath + " " + config.DetectedClaudePermissionsFlag
	inst, _ := committedRetryClaudeInstance(t, flagged)

	// Simulate a live pane whose recorded program is the post-injection
	// command (af appended --session-id and --plugin-dir on top of the
	// flagged base). The seeding gate compares pending.Program (pre-injection)
	// against this post-injection program, so they differ and the gate
	// cannot fire.
	postInjection := flagged + " --session-id abc-123 --plugin-dir /tmp/plugins"
	ts := tmux.NewTmuxSession(inst.Title, postInjection)
	setLaunchProgram(ts, postInjection, sessionenv.AccountLaunchProof{})
	inst.mu.Lock()
	inst.Tabs = []*Tab{newAgentTab(ts)}
	inst.mu.Unlock()
	require.NotEqual(t, flagged, inst.ResolvedPaneProgram(),
		"the pane's post-injection program must differ from the pre-injection frozen base")

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"the fix must declare the frozen flag via trustBase even when the seeding gate cannot fire")
	require.NotNil(t, inst.accountSwapLaunch)
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.Contains(t, plan.proof.GeneratedArgs, config.DetectedClaudePermissionsFlag,
		"the proof must declare af's own flag from trustBase, not from the pane seeding gate")
}

// TestValidateAccountSwapCommittedRetryRefusesNonBuiltInFlaggedBase confirms
// the fix does not widen what the account boundary admits: a committed retry
// whose frozen command carries a flag that is NOT af's own built-in override
// (a different executable, or an extra argument beyond the built-in) stays
// refused. Only af's own detected flag frozen inside the built-in override
// may pass; an operator override with a different path or extra words keeps
// trustBase false and the proof fails closed.
func TestValidateAccountSwapCommittedRetryRefusesNonBuiltInFlaggedBase(t *testing.T) {
	claudePath := fakeDetectedClaude(t)
	for name, program := range map[string]string{
		"extra argument":          claudePath + " " + config.DetectedClaudePermissionsFlag + " --settings /repo/x.json",
		"flag only, other exe":    "/repo/tools/claude " + config.DetectedClaudePermissionsFlag,
		"unrelated operator flag": "claude --profile p",
	} {
		t.Run(name, func(t *testing.T) {
			inst, _ := committedRetryClaudeInstance(t, program)
			err := inst.ValidateAccountSwap("work")
			require.Error(t, err,
				"a non-built-in flagged base must stay refused at the committed retry")
		})
	}
}
