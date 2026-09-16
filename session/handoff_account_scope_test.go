package session

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
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
			inst.accountAutoSelected = tc.autoSelected

			entry, err := inst.SwapAgentProgram(tmux.ProgramAider, HandoffReasonManual, "abc123", false)
			require.NoError(t, err)

			if account, auto := inst.AccountSelection(); account != "" || auto {
				t.Fatalf("AccountSelection = (%q, %v) after the swap, want the scope dropped — "+
					"aider has no account namespace for %q to resolve in", account, auto, "work")
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
			entry, err := inst.RecordHandoffSwap(tc.target, HandoffReasonManual, "abc123", false)
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
	_, err = inst.RecordHandoffSwap(tmux.ProgramAider, HandoffReasonManual, "abc123", false)
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
