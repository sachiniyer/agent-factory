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
// dismissals returns on its first poll instead of dominating the test. It is
// claude's glyph specifically: isReadyContent keys on a different one per agent,
// so any case that drives another agent THROUGH a dismissal must either supply
// that agent's glyph or avoid reaching the re-wait at all.
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

// TestDismissConfigAgentTrustPrompt_ChecksEveryAgentInTheGate is the #2416
// regression.
//
// This gate used to be its own hand-copied list of agents under a comment
// claiming it mirrored LocalBackend.CheckAndHandleTrustPrompt. It had drifted:
// opencode was added to that gate in #1959 and never here, so an opencode config
// agent took the default branch and never ran the dismissal loop. The exposure
// was not a hang: isReadyContent's opencode arm calls a doc-trust dialog ready,
// so had opencode ever raised one the spawn would have delivered the briefing
// into it — the #729 defect class the comment was written to prevent. opencode
// is not known to raise one, which is why this stayed latent.
//
// The case runs over the shared gate rather than a literal list, so it covers
// whatever agents are in the gate today, and a future agent is covered the
// moment it is classified into it. That does make the SELECTION circular — this
// cannot catch a wrong classification, only a call site that stopped delegating.
// The non-circular half is the literal table in
// TestProgramNeedsTrustDismissal_ClassifiesEverySupportedAgent.
//
// What is asserted is the gate, not the loop: each pane is given no dialog, so
// DismissTrustPrompt makes exactly one check and returns without a readiness
// wait. One check means the agent reached the loop; zero is the defect
// signature. The loop's own behaviour — budget, re-wait, bound — is held by the
// two claude cases above, and per-agent ready glyphs belong to task's tests
// rather than being restated here.
func TestDismissConfigAgentTrustPrompt_ChecksEveryAgentInTheGate(t *testing.T) {
	// No timing seam needed: prompts is zero, so the first check ends the loop
	// and the readiness re-wait is never reached. (Compressing it would not help
	// anyway — SetTrustPromptTimingForTest moves the poll interval, not
	// WaitForReadyOn's deadline; see the context bound on the sibling case.)
	covered := 0
	for _, agent := range tmux.SupportedPrograms {
		if !tmux.ProgramNeedsTrustDismissal(agent) {
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
		t.Fatal("no agent is in the trust-dismissal gate; the case under test is vacuous")
	}
}

// TestDismissConfigAgentTrustPrompt_SkipsAgentsOutsideTheGate is the other half
// of #2416: closing the drift must not over-correct into driving a dismissal
// loop for an agent AF has no dismissal for. devin is the current member — the
// only predicate membership would run for it is DocTrustPromptPresent, which
// cannot match its modal wording, so the loop buys no dismissal and leaves only
// that predicate's false-positive exposure on live panes (#1952).
func TestDismissConfigAgentTrustPrompt_SkipsAgentsOutsideTheGate(t *testing.T) {
	covered := 0
	for _, agent := range tmux.SupportedPrograms {
		if tmux.ProgramNeedsTrustDismissal(agent) {
			continue
		}
		covered++
		t.Run(agent, func(t *testing.T) {
			// prompts: 1 so a pane that IS asked would answer "dialog present" and
			// be counted — the check has to be able to fail.
			target := &fakeTrustTarget{agent: agent, prompts: 1}
			// A regressed gate would enter the loop and then block in the readiness
			// re-wait, because this fake renders claude's glyph and no other
			// agent's. SetTrustPromptTimingForTest compresses the poll interval but
			// not WaitForReadyOn's 60s deadline, so bound it here instead;
			// WaitForReadyOn observes cancellation at the top of each iteration.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := dismissConfigAgentTrustPrompt(ctx, target)
			// Assert the checks BEFORE the error: on a regression both fire, and
			// this is the one that names the actual defect. Leading with err would
			// report a readiness timeout and send the next reader to the wrong file.
			if target.checks != 0 {
				t.Fatalf("%s's pane must not be driven through the dismissal loop, got %d checks", agent, target.checks)
			}
			if err != nil {
				t.Fatalf("%s is outside the trust-dismissal gate, got: %v", agent, err)
			}
		})
	}
	// Without this, reclassifying the last excluded agent would retire the
	// over-correction half of #2416 to a green test with zero assertions.
	if covered == 0 {
		t.Fatal("every supported agent is now in the gate; this case asserts nothing — " +
			"delete it deliberately or restore an excluded agent")
	}
}
