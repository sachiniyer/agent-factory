package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// fakeTrustTarget is a config-agent-shaped task.TrustPromptTarget with no tmux
// behind it. dismissConfigAgentTrustPrompt takes the target interface rather
// than a *tmux.TmuxSession precisely so the budget below is assertable without
// a real pane, a real agent, or a real daemon.
type fakeTrustTarget struct {
	agent string
	// prompts is how many trust dialogs this pane will show before it settles.
	prompts int
	checks  int
}

func (f *fakeTrustTarget) ResolvedAgent() string { return f.agent }

// PreviewContent renders claude's ready glyph, so the readiness re-wait between
// dismissals returns on its first poll instead of dominating the test.
func (f *fakeTrustTarget) PreviewContent(context.Context) (string, error) { return "❯", nil }

func (f *fakeTrustTarget) HooksDone() <-chan struct{} { return nil }

func (f *fakeTrustTarget) CheckAndHandleTrustPrompt() bool {
	f.checks++
	if f.prompts <= 0 {
		return false
	}
	f.prompts--
	return true
}

// TestDismissConfigAgentTrustPrompt_GetsTheSameBudgetAsASession is the #2097
// regression test: a config agent must clear exactly as many trust dialogs as a
// regular session, because it now runs the SAME loop on the SAME budget.
//
// Before the fix the daemon carried its own 5 × 500ms constants under a comment
// claiming they mirrored task/start.go's 20 × 1s, so this case — a pane that
// settles after more dialogs than 5 but far fewer than the canonical budget —
// failed the spawn with "trust prompt did not dismiss", the caller reaped the
// session, and the user was left unable to configure af.
func TestDismissConfigAgentTrustPrompt_GetsTheSameBudgetAsASession(t *testing.T) {
	defer task.SetTrustPromptTimingForTest(time.Nanosecond)()

	// Eight dialogs: more than the retired 5-attempt config-agent cap and fewer
	// than the canonical budget — squarely inside the window where a session
	// succeeded and a config agent did not.
	target := &fakeTrustTarget{agent: tmux.ProgramClaude, prompts: 8}
	if err := dismissConfigAgentTrustPrompt(context.Background(), target); err != nil {
		t.Fatalf("a config agent must clear as many trust dialogs as a session does, got: %v", err)
	}
	// Eight dismissals plus the clean check that ends the loop.
	if target.checks != 9 {
		t.Fatalf("expected 9 trust checks, got %d", target.checks)
	}
}

// TestDismissConfigAgentTrustPrompt_BoundedByTheCanonicalBudget keeps the loop
// bounded: raising the budget must not turn a permanently-stuck dialog into an
// unbounded spin. The bound is asserted against task.MaxTrustPromptAttempts
// rather than a literal so this test cannot become the next copy that drifts.
func TestDismissConfigAgentTrustPrompt_BoundedByTheCanonicalBudget(t *testing.T) {
	defer task.SetTrustPromptTimingForTest(time.Nanosecond)()

	target := &fakeTrustTarget{agent: tmux.ProgramClaude, prompts: task.MaxTrustPromptAttempts + 5}
	err := dismissConfigAgentTrustPrompt(context.Background(), target)
	if err == nil {
		t.Fatal("a trust prompt that never clears must still fail the spawn")
	}
	if !strings.Contains(err.Error(), "trust prompt did not dismiss") {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.checks != task.MaxTrustPromptAttempts {
		t.Fatalf("expected %d trust checks, got %d", task.MaxTrustPromptAttempts, target.checks)
	}
}

// TestDismissConfigAgentTrustPrompt_SkipsNonAgents holds the per-agent gate that
// predates this fix: only known agents have a trust dialog, and asking anything
// else about one taps Enter into an arbitrary program.
func TestDismissConfigAgentTrustPrompt_SkipsNonAgents(t *testing.T) {
	target := &fakeTrustTarget{agent: "", prompts: 1}
	if err := dismissConfigAgentTrustPrompt(context.Background(), target); err != nil {
		t.Fatalf("a non-agent program has no trust dialog to dismiss, got: %v", err)
	}
	if target.checks != 0 {
		t.Fatalf("Enter must never be tapped into a non-agent program, got %d checks", target.checks)
	}
}

// TestDismissConfigAgentTrustPrompt_ClearsEveryTrustPromptAgent is the #2416
// regression.
//
// This gate used to be its own hand-copied list of agents under a comment
// claiming it mirrored LocalBackend.CheckAndHandleTrustPrompt. It had drifted:
// opencode was added to that gate in #1959 and never here, so an opencode config
// agent took the default branch, never ran the dismissal loop, and would hang on
// a dialog a normal session cleared — the #729 defect class the comment was
// written to prevent.
//
// The case runs over the shared gate rather than a literal list, so it covers
// whatever agents are classified as prompting today, and a future agent that
// starts prompting is covered the moment it is classified.
//
// What is asserted is the gate, not the loop: each pane is given no dialog, so
// DismissTrustPrompt makes exactly one check and returns without a readiness
// wait. One check means the agent reached the loop; zero is the defect
// signature. The loop's own behaviour — budget, re-wait, bound — is held by the
// two claude cases above, and per-agent ready glyphs belong to task's tests
// rather than being restated here.
func TestDismissConfigAgentTrustPrompt_ClearsEveryTrustPromptAgent(t *testing.T) {
	covered := 0
	for _, agent := range tmux.SupportedPrograms {
		if !tmux.ProgramHasTrustPrompt(agent) {
			continue
		}
		covered++
		t.Run(agent, func(t *testing.T) {
			target := &fakeTrustTarget{agent: agent}
			if err := dismissConfigAgentTrustPrompt(context.Background(), target); err != nil {
				t.Fatalf("config agent must run %s's trust-dismissal loop, got: %v", agent, err)
			}
			if target.checks != 1 {
				t.Fatalf("expected %s's pane to be checked once for a trust dialog, got %d checks", agent, target.checks)
			}
		})
	}
	if covered == 0 {
		t.Fatal("no agent is classified as having a trust prompt; the gate under test is vacuous")
	}
}

// TestDismissConfigAgentTrustPrompt_SkipsAgentsWithNoDialog is the other half of
// #2416: closing the drift must not over-correct into dismissing for an agent
// whose modal AF already suppresses at launch. devin is launched with
// --respect-workspace-trust false (#2410), so there is no dialog — and Enter
// tapped at its composer would submit whatever is sitting there.
func TestDismissConfigAgentTrustPrompt_SkipsAgentsWithNoDialog(t *testing.T) {
	for _, agent := range tmux.SupportedPrograms {
		if tmux.ProgramHasTrustPrompt(agent) {
			continue
		}
		t.Run(agent, func(t *testing.T) {
			// prompts: 1 so a pane that IS asked would answer "dialog present" and
			// be counted — the check has to be able to fail.
			target := &fakeTrustTarget{agent: agent, prompts: 1}
			if err := dismissConfigAgentTrustPrompt(context.Background(), target); err != nil {
				t.Fatalf("%s has no trust dialog to dismiss, got: %v", agent, err)
			}
			if target.checks != 0 {
				t.Fatalf("Enter must never be tapped into %s's composer, got %d checks", agent, target.checks)
			}
		})
	}
}
