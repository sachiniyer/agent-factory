package task

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// docTrustDialogFixture is the generic documentation-trust dialog, at the full
// length DocTrustPromptPresent requires (it deliberately refuses to match a
// shortened prefix — see its doc in session/tmux/start.go).
//
// It must contain NO agent composer glyph. Several arms are a disjunction of a
// trust predicate and a composer glyph (codex is CodexTrustPromptPresent || "›"),
// so a stray glyph would make those agents "ready" for a reason that has nothing
// to do with a trust dialog, and the label checks below would blame runner.go for
// a change in this string. TestTrustDialogFixtureHasNoComposerGlyph guards it.
const docTrustDialogFixture = `Add https://aider.chat/docs/faq.html to the chat?
Open documentation url for more info
(Y)es/(N)o/(D)on't ask again [Yes]:`

// Which predicate isReadyContent's arm for an agent uses to decide a trust
// dialog means "ready". This is a fact about AF's code, greppable in runner.go —
// NOT a claim about what the third-party agent displays.
//
// That distinction is the whole reason the gate is named
// ProgramNeedsTrustDismissal rather than "has a trust prompt": amp and opencode
// carry the isDocTrustPrompt arm without being known to raise the dialog at all
// — opencode's arm says so itself, "kept for consistency with the other agents",
// and amp's first run was never captured. Keying this on agent behaviour would
// assert things the repo has not measured, and in opencode's case the opposite
// of what it did measure.
const (
	// armGeneric: the arm falls through to isDocTrustPrompt, so
	// docTrustDialogFixture exercises it. claude qualifies despite also having its
	// own strings — "has its own predicate" and "uses the generic one" are not
	// exclusive.
	armGeneric = "generic"
	// armAgentSpecific: the arm recognizes only the agent's OWN dialog and does
	// not fall through to isDocTrustPrompt, so the shared fixture cannot exercise
	// it. codex is the sole member; its arm is covered by its own cases in
	// runner_test.go.
	armAgentSpecific = "agent-specific"
	// armNone: no trust arm at all, so no dialog can ever read as ready.
	armNone = "none"
)

// trustArmSpec pairs the verifiable fact with the policy decision, deliberately
// keeping them in separate fields.
type trustArmSpec struct {
	// arm is which predicate isReadyContent uses. It is VERIFIED against
	// runner.go below, not taken on trust.
	arm string
	// inGate is what ProgramNeedsTrustDismissal must answer. It is STATED, never
	// inferred from arm — and that is load-bearing.
	//
	// The label check can only ask runner.go one question: does the generic
	// fixture make this agent ready? That splits three labels into two buckets —
	// armGeneric (true) versus armAgentSpecific and armNone (both false). Those
	// two are indistinguishable to the check while implying OPPOSITE gate
	// answers, so inferring inGate from arm would let a relabel between them move
	// the gate expectation silently. Both halves were demonstrated against the
	// inferred version and both passed: relabel devin armNone→armAgentSpecific
	// and add it to the gate (the #1952 exposure, waved through), or relabel
	// codex armAgentSpecific→armNone and drop it from the gate (#729's literal
	// shape). Stating inGate closes both, because no relabel can move it.
	inGate bool
}

// trustReadinessArm must name every supported agent.
//
// It exists because the obvious version of this test — "skip claude and codex,
// assert the biconditional for the rest" — is blind to the very incident it
// cites. #729 was codex: an agent with its OWN dialog predicate that was missing
// from the dismissal gate. Skipping the agent-specific cases skips that shape,
// and a hand-maintained skip list with no forcing function is the same unforced
// hand-list #2416 was about, reintroduced inside its own fix.
//
// The inGate column deliberately restates what session/tmux's classification
// table pins. That duplication is safe in a way #2416's was not: both are
// compared against the same ProgramNeedsTrustDismissal, so if they ever disagree
// one goes red immediately. Silent drift was the danger, not duplication.
var trustReadinessArm = map[string]trustArmSpec{
	// claude's arm carries its own strings ("Do you trust", "new MCP server") AND
	// falls through to isDocTrustPrompt, so the generic fixture does exercise it.
	// The label check is what caught this — it was first written armAgentSpecific
	// on the assumption that the two were exclusive.
	tmux.ProgramClaude: {arm: armGeneric, inGate: true},
	// codex: CodexTrustPromptPresent plus the "›" composer glyph, with no
	// isDocTrustPrompt fallthrough. The sole agent-specific member.
	tmux.ProgramCodex:    {arm: armAgentSpecific, inGate: true},
	tmux.ProgramAider:    {arm: armGeneric, inGate: true},
	tmux.ProgramGemini:   {arm: armGeneric, inGate: true},
	tmux.ProgramAmp:      {arm: armGeneric, inGate: true},
	tmux.ProgramOpencode: {arm: armGeneric, inGate: true},
	// devin has no trust arm and is out of the gate: AF has no predicate that
	// identifies its modal, so membership would buy no dismissal and leave only
	// false-positive exposure (#1952). If #2435 gives AF a devin predicate, the
	// likely end state is {arm: armNone, inGate: true} — dismissed on the poll,
	// never treated as ready. Separate fields make that representable; inferring
	// inGate from arm did not.
	tmux.ProgramDevin: {arm: armNone, inGate: false},
}

// TestTrustDialogFixtureHasNoComposerGlyph guards the precondition the label
// checks depend on. Several readiness arms are (trust predicate || composer
// glyph); a glyph in the fixture would make those agents ready for an unrelated
// reason and produce a confident, wrong diagnosis pointing at runner.go.
func TestTrustDialogFixtureHasNoComposerGlyph(t *testing.T) {
	// claude ❯, codex ›, devin ❭, and frame glyphs the amp/opencode checks use.
	for _, glyph := range []string{"❯", "›", "❭", "┃", "╹", "╰"} {
		if strings.Contains(docTrustDialogFixture, glyph) {
			t.Fatalf("docTrustDialogFixture contains the composer glyph %q; agents whose arm is "+
				"(trust predicate || glyph) would read as ready for a reason unrelated to any trust "+
				"dialog, and the parity checks would misattribute that to runner.go", glyph)
		}
	}
}

// TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss locks the #729 invariant
// across the two enumerations that have to agree about it:
//
//   - isReadyContent decides whether a pane showing a trust dialog counts as
//     READY, and
//   - tmux.ProgramNeedsTrustDismissal decides whether AF will then clear it.
//
// If an agent is ready-on-dialog but not in the gate, AF calls the pane usable
// and types the briefing into an undismissed modal. That is #729 exactly — codex
// had a trust arm and was missing from the gate — and #2416 put opencode in the
// same state.
//
// It is not circular: isReadyContent and ProgramNeedsTrustDismissal are
// independent implementations, neither derived from the other.
func TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss(t *testing.T) {
	biconditionalsChecked := 0
	for _, agent := range tmux.SupportedPrograms {
		spec, named := trustReadinessArm[agent]
		if !named {
			t.Errorf("supported agent %q has no entry in trustReadinessArm; record which predicate "+
				"its isReadyContent arm uses, and whether it belongs in the dismissal gate", agent)
			continue
		}
		inGate := tmux.ProgramNeedsTrustDismissal(agent)
		ready := isReadyContent(docTrustDialogFixture, agent)

		// The gate answer is checked against the STATED expectation, so no arm
		// relabel can move it. See trustArmSpec.inGate.
		if inGate != spec.inGate {
			t.Errorf("ProgramNeedsTrustDismissal(%q) = %t, want %t; if this is an intended change, "+
				"update the row and say why — do not relabel its arm to make this pass",
				agent, inGate, spec.inGate)
		}

		// Then verify the arm label itself against runner.go, so a stale or
		// convenient label fails rather than silently changing what is asserted.
		switch spec.arm {
		case armGeneric:
			if !ready {
				t.Errorf("%q is labelled armGeneric, but the generic doc-trust dialog does not make it "+
					"ready — its isReadyContent arm no longer falls through to isDocTrustPrompt. Fix "+
					"the row to match runner.go rather than leaving it stale", agent)
				continue
			}
			biconditionalsChecked++
			// ready is true, so #729 says this agent must be dismissable.
			if !inGate {
				t.Errorf("isReadyContent(docTrustDialog, %q) = true but ProgramNeedsTrustDismissal(%q) = "+
					"false; a dialog AF calls ready must be one AF dismisses (#729)", agent, agent)
			}
		case armAgentSpecific, armNone:
			if ready {
				t.Errorf("%q is labelled %s, but the generic doc-trust dialog makes it ready — its arm "+
					"does fall through to isDocTrustPrompt, so it belongs in armGeneric where the "+
					"biconditional is checked", agent, spec.arm)
			}
		default:
			t.Errorf("agent %q has unknown trust-readiness arm %q", agent, spec.arm)
		}
	}

	// Covers the wholesale case the per-row checks cannot: isDocTrustPrompt
	// removed from every arm in runner.go with all labels updated to match. Every
	// label check would pass and the #729 biconditional would never run.
	if biconditionalsChecked == 0 {
		t.Fatal("no agent exercised the ready-implies-dismissable check — the generic doc-trust " +
			"readiness concept appears to be gone from runner.go entirely")
	}

	for agent := range trustReadinessArm {
		if !tmux.IsSupportedProgram(agent) {
			t.Errorf("trustReadinessArm names %q, which is no longer a supported agent; drop its row", agent)
		}
	}
}
