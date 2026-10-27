package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// A cross-agent handoff must never carry an account name onto an agent the name
// does not belong to (#4428).
//
// The hazard is a collision, not a leak: handing a claude session scoped to
// "work" over to codex would let refreshSessionEnvironment reapply that name in
// the CODEX namespace, launching the pane under a codex account also called
// "work" — a different identity the user never selected. The two target classes
// have different honest answers, and the session record is where they diverge:
//
//   - a target WITH account support (claude, codex, gemini) keeps the recorded
//     account, and the swap refuses unless the caller named the incoming
//     agent's account — never a guess;
//   - a target WITHOUT it (aider, amp, opencode, devin) has no namespace the
//     name could resolve in, so the record drops the scope in the same locked
//     mutation that rewrites Program — the ledger records what was dropped.
//
// Making the drop part of the record mutation, rather than a backend tweak,
// keeps the scope and the program consistent at every observable point and lets
// a failed runtime swap roll the whole record back.
func TestSwapAgentProgram_DropsScopeForNonScopableTarget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		autoSelected bool
	}{
		{name: "pinned", autoSelected: false},
		{name: "auto-selected", autoSelected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := handoffTestInstance(t, tmux.ProgramClaude)
			inst.Account = "work"
			inst.accountAgent = tmux.ProgramClaude
			inst.accountAutoSelected = tc.autoSelected

			entry, err := inst.SwapAgentProgram(tmux.ProgramAider, HandoffReasonManual, "abc123", false)
			require.NoError(t, err)

			if account, auto := inst.AccountSelection(); account != "" || auto {
				t.Fatalf("AccountSelection = (%q, %v) after the swap, want the scope dropped — "+
					"aider has no account namespace for %q to resolve in", account, auto, "work")
			}
			if got := inst.AccountAgent(); got != "" {
				t.Fatalf("AccountAgent = %q after the scope drop, want empty — "+
					"a dropped pin must not keep pointing at the old registry", got)
			}
			if got := inst.AgentProgram(); got != tmux.ProgramAider {
				t.Fatalf("Program = %q, want %q", got, tmux.ProgramAider)
			}
			if entry.FromAccount != "work" || entry.ToAccount != "" {
				t.Fatalf("ledger account fields = (%q → %q), want the dropped scope recorded as "+
					"from_account only", entry.FromAccount, entry.ToAccount)
			}

			require.NoError(t, inst.RevertHandoff(entry))
			if account, auto := inst.AccountSelection(); account != "work" || auto != tc.autoSelected {
				t.Fatalf("AccountSelection after revert = (%q, %v), want (%q, %v) — a swap that "+
					"never completed must not descope the session it left running",
					account, auto, "work", tc.autoSelected)
			}
			if got := inst.AccountAgent(); got != tmux.ProgramClaude {
				t.Fatalf("AccountAgent after revert = %q, want %q — the pin's registry "+
					"rolls back with the account", got, tmux.ProgramClaude)
			}
		})
	}
}

// A scopable target does NOT get the same treatment: the account stays on the
// record so the swap boundary can refuse a handoff that never named the
// incoming account, and so the account transaction can replace it under its own
// fence when one is named. The record layer deliberately does not decide the
// refusal — it only guarantees the name was never silently rewritten.
func TestSwapAgentProgram_KeepsScopeForScopableTarget(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Account = "work"

	entry, err := inst.SwapAgentProgram(tmux.ProgramCodex, HandoffReasonManual, "abc123", false)
	require.NoError(t, err)

	if account, _ := inst.AccountSelection(); account != "work" {
		t.Fatalf("Account = %q after the record, want %q — the refusal belongs to the swap "+
			"boundary, which needs the recorded scope to refuse", account, "work")
	}
	if entry.FromAccount != "work" {
		t.Fatalf("entry.FromAccount = %q, want %q", entry.FromAccount, "work")
	}
}

// The scope capability belongs to the command a handoff resolves to, not the
// enum it was requested under (#4430 review). `program_overrides.aider =
// "codex"` passes the enum check — aider has no account namespace — yet
// launches Codex, which does: dropping the scope there would start Codex with
// ambient credentials. The inverse shape matters just as much — an enum that
// claims Codex but resolves to aider launches a process with no namespace the
// scope could occupy, so the record still drops it. Both directions are
// exercised because a fix that only consulted the enum would pass one and
// silently break the other.
//
// The unprovable shapes are the third class (#4430 review round 3), and they
// split differently after D1: a command the credential-boundary parser cannot
// prove — `bash`, or `./collect codex` whose agent-looking word is an
// argument, not the executable — is UNCLASSIFIABLE, and an unclassifiable
// resolution refuses rather than drops: a durable pin is never destroyed on
// an answer af cannot prove (`npx codex` may launch a scopable agent
// underneath). A loose token scan claims the namespace the argument names
// ("codex"), and the enum fallback claims the target's — both let "work" ride
// a launch that can never apply it; the refusal denies both.
func TestSwapAgentProgram_ScopeDecisionFollowsResolvedCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		target      string
		override    string
		wantAccount string
		wantErr     string
	}{
		{name: "non-scopable enum resolving to scopable command",
			target: tmux.ProgramAider, override: tmux.ProgramCodex, wantAccount: "work"},
		{name: "scopable enum resolving to non-scopable command",
			target: tmux.ProgramCodex, override: tmux.ProgramAider, wantAccount: ""},
		{name: "scopable enum resolving to a non-agent command",
			target: tmux.ProgramCodex, override: "bash", wantErr: "cannot classify"},
		{name: "scopable enum resolving to an agent-looking argument",
			target: tmux.ProgramCodex, override: "./collect codex", wantErr: "cannot classify"},
		{name: "non-scopable enum resolving to an agent-looking argument",
			target: tmux.ProgramAider, override: "./collect codex", wantErr: "cannot classify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			_, err := config.SetGlobalConfigValue("program_overrides."+tc.target, tc.override)
			require.NoError(t, err)
			inst := handoffTestInstance(t, tmux.ProgramClaude)
			inst.Account = "work"

			_, err = inst.SwapAgentProgram(tc.target, HandoffReasonManual, "abc123", false)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				if account, _ := inst.AccountSelection(); account != "work" {
					t.Fatalf("Account = %q after the refusal, want %q — a refused swap "+
						"leaves the pin it found", account, "work")
				}
				return
			}
			require.NoError(t, err)
			if account, _ := inst.AccountSelection(); account != tc.wantAccount {
				t.Fatalf("Account = %q, want %q — the scope decision belongs to the resolved %q "+
					"command, not the requested %q enum", account, tc.wantAccount, tc.override, tc.target)
			}
		})
	}
}

// Instance.SwapAgent is the chokepoint every runtime replacement goes through.
// A session record that still carries an account at that point is an invariant
// violation — the daemon refuses scopable targets without --account, and the
// record drops the scope for targets that cannot carry it — so the swap refuses
// rather than let the environment refresh reapply the name in the wrong
// namespace. Refusal happens before the backend is invoked, so no pane is
// touched (#4428).
//
// The two cases exercise the violation differently: codex's record legitimately
// KEEPS the scope (the refusal belongs to this boundary), while aider's record
// drops it — so a still-scoped aider record is written back by hand to stand
// in for a mutation that bypassed the transaction.
func TestInstanceSwapAgent_RefusesUnsettledAccount(t *testing.T) {
	for _, tc := range []struct {
		target      string
		wantSubtext []string
	}{
		{target: tmux.ProgramCodex, wantSubtext: []string{"--account"}},
		{target: tmux.ProgramAider, wantSubtext: []string{"work", "session record"}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			inst := handoffTestInstance(t, tmux.ProgramClaude)
			inst.Account = "work"
			require.NoError(t, inst.Transition(BeginHandoff()))
			entry, err := inst.RecordHandoffSwap(tc.target, tc.target, HandoffReasonManual, "abc123", false)
			require.NoError(t, err)
			if _, scopable := sessionenv.SupportsAccounts(tc.target); !scopable {
				// The record correctly dropped the scope; put it back to simulate
				// the drift this boundary exists to fail closed against.
				inst.Account = "work"
			}

			_, err = inst.SwapAgent(AgentSwapPlan{target: tc.target, program: tc.target})
			require.Error(t, err, "a still-scoped record must not reach the runtime swap")
			for _, sub := range tc.wantSubtext {
				require.Contains(t, err.Error(), sub)
			}
			if account, _ := inst.AccountSelection(); account != "work" {
				t.Fatalf("Account = %q after the refusal, want %q — a refused swap leaves the record it found",
					account, "work")
			}
			require.NoError(t, inst.RevertHandoff(entry))
			require.NoError(t, inst.Transition(AbortHandoff()))
		})
	}
}

// The end-to-end half on the real local backend: a scoped session handed to an
// agent with no account support must come back up on the ambient environment.
// The assertion is on the launch wrapper — a scoped launch carries af's
// account-shim marker, an ambient one does not — because that is the only
// observable difference between "the scope was dropped" and "the scope was
// reapplied under a name the target cannot use" (#4428).
func TestLocalBackendSwapAgent_LaunchesAmbientAfterScopeDrop(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	ptyFactory := &recordingPtyFactory{t: t}
	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "kill-session"):
				killed = true
				return nil
			case strings.Contains(joined, "has-session"):
				if killed && len(ptyFactory.cmds) == 0 {
					return errors.New("session absent after close")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return nil, errors.New("pane pid unavailable")
			}
			return nil, nil
		},
	}

	repoRoot := initTempGitRepo(t)
	worktreePath := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktreePath, "handoff-descope", "handoff-descope-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("handoff-descope", tmux.ProgramClaude, ptyFactory, cmdExec)
	backend := &LocalBackend{}
	inst := &Instance{
		ID:          "handoff-descope-id",
		Title:       "handoff-descope",
		Path:        repoRoot,
		Program:     tmux.ProgramClaude,
		Account:     "work",
		backend:     backend,
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
		started:     true,
		liveness:    LiveRunning,
	}

	// Drive the same record transaction the daemon does: fence, rewrite the
	// record (which drops the scope for aider), then the runtime swap.
	require.NoError(t, inst.Transition(BeginHandoff()))
	_, err = inst.RecordHandoffSwap(tmux.ProgramAider, tmux.ProgramAider, HandoffReasonManual, "abc123", false)
	require.NoError(t, err)

	checkpoint, err := inst.SwapAgent(AgentSwapPlan{target: tmux.ProgramAider, program: tmux.ProgramAider})
	require.NoError(t, err)
	require.Empty(t, checkpoint.Account,
		"the durable checkpoint must not carry a scope the incoming agent cannot represent")

	var launch string
	for _, c := range ptyFactory.cmds {
		if strings.Contains(strings.Join(c.Args, " "), "new-session") {
			launch = strings.Join(c.Args, " ")
			break
		}
	}
	require.NotEmpty(t, launch, "the incoming agent never launched")
	require.NotContains(t, launch, "__af-session-env-exec-account",
		"a descoped handoff must launch on the ambient boundary, not the account shim")
}

// The production plan pipeline half of the override contract (#4430 review):
// LocalBackend.PrepareAgentSwap must freeze the RESOLVED command, and
// EffectiveAgent must name the agent that command actually launches. The
// daemon's cross-agent refusal reads only this pair — a fake that freezes
// program=target proves nothing about it, which is how the daemon test once
// asserted a refusal its fixture could never produce.
func TestPrepareAgentSwapEffectiveAgentFollowsResolvedCommand(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := config.SetGlobalConfigValue("program_overrides."+tmux.ProgramAider, tmux.ProgramCodex)
	require.NoError(t, err)
	repoRoot := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, t.TempDir(), "plan-effective", "plan-effective-branch", "", false, false)
	require.NoError(t, err)
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Path = repoRoot
	inst.SetGitWorktreeForTest(gw)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramCodex), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	plan, err := (&LocalBackend{}).PrepareAgentSwap(inst, tmux.ProgramAider)
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, plan.EffectiveAgent(),
		"the frozen plan must name the agent the resolved command launches, not the requested enum")
}

// The daemon's resolved_agents response and the swap pipeline must read the
// same answer: HandoffEffectiveAgentForPath is handoffEffectiveAgent's
// instance-free form over the same path, so a picker classifying by the
// response and the plan that froze the command can never disagree about which
// agent a target launches — the bug moved if they could (#4430 review).
func TestHandoffEffectiveAgentForPath_MatchesInstanceResolution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := config.SetGlobalConfigValue("program_overrides."+tmux.ProgramCodex, tmux.ProgramAider)
	require.NoError(t, err)
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	for _, target := range tmux.SupportedPrograms {
		require.Equal(t, handoffEffectiveAgent(inst, target),
			HandoffEffectiveAgentForPath(inst.Path, target), target)
	}
	require.Equal(t, tmux.ProgramAider,
		HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramCodex))
}

// The plan-failure precedence fix (daemon/handoff.go) resolves the same
// command the plan already resolved — DetectAgentFromCommand reads the command
// STRING, never the filesystem, so a binary that failed preflight still yields
// its agent and the scope refusal can win the overlap (#4430 review).
func TestHandoffEffectiveAgentForPath_NeedsNoBinary(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := config.SetGlobalConfigValue(
		"program_overrides."+tmux.ProgramAider, "/nonexistent/codex")
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex,
		HandoffEffectiveAgentForPath(t.TempDir(), tmux.ProgramAider),
		"detection answers from the resolved command even when its binary cannot launch")
}

// refreshSessionEnvironment must pin the account's namespace to the durable
// selection record — i.accountAgent — never to the command the pane will run
// now (#4430 review round 4). A program_overrides edit between the pin and a
// respawn can resolve the recorded Program to another agent's command;
// deriving the namespace from that command would declare the same account
// label in a different registry, and prepareLaunchEnvironment's matching
// declaration would pass the drift check the old enum answer used to fail.
// The sibling-tab refresh takes the same answer — a credential-bearing shell
// inherits the account's selection namespace, not its own program's (a shell
// is not the agent the account belongs to).
func TestRefreshSessionEnvironment_PinsNamespaceToSelectionRecord(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramAider)
	inst.Account = "work"
	inst.accountAgent = tmux.ProgramCodex

	agent := tmux.NewTmuxSession("refresh-agent", "codex --model o4")
	require.NoError(t, refreshSessionEnvironment(inst, agent))
	require.Equal(t, tmux.ProgramCodex, agent.AccountAgentForTest(),
		"the account lives in the recorded selection namespace — codex — not the recorded aider enum")

	// The durable record wins over the pane's own command: a codex-pinned
	// session respawning under an override that now resolves to gemini must
	// still declare codex/work — the launch-side namespace check is what
	// refuses, rather than silently re-scoping to gemini/work.
	drifted := tmux.NewTmuxSession("refresh-drifted", "gemini")
	require.NoError(t, refreshSessionEnvironment(inst, drifted))
	require.Equal(t, tmux.ProgramCodex, drifted.AccountAgentForTest(),
		"a program_overrides edit cannot move the pin to another registry")

	process := tmux.NewTmuxSession("refresh-process", "cat")
	tab := &Tab{ID: newTabID(), Name: "build", Kind: TabKindProcess, Command: "cat", tmux: process}
	require.NoError(t, refreshTabSessionEnvironment(inst, tab))
	require.Equal(t, tmux.ProgramCodex, process.AccountAgentForTest(),
		"a credential-bearing sibling inherits the account's selection namespace")
}

// A record older than the accountAgent field has no durable namespace: the
// only registry its account could have been selected in is the requested
// program's enum — the redirected-account handoff that separates label from
// enum is newer than the record.
func TestRefreshSessionEnvironment_LegacyRecordFallsBackToEnum(t *testing.T) {
	inst := handoffTestInstance(t, tmux.ProgramCodex)
	inst.Account = "work"
	// accountAgent deliberately unset — a pre-#4430-round-4 record.

	agent := tmux.NewTmuxSession("refresh-legacy", "codex")
	require.NoError(t, refreshSessionEnvironment(inst, agent))
	require.Equal(t, tmux.ProgramCodex, agent.AccountAgentForTest(),
		"the enum is the only namespace a pre-field record could have selected in")

	require.Equal(t, tmux.ProgramCodex, inst.AccountAgent(),
		"the accessor reports the same fallback a reader sees")
}
