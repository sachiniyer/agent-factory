package task

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// docTrustDialogFixture is the generic documentation-trust dialog, at the full
// length DocTrustPromptPresent requires (it deliberately refuses to match a
// shortened prefix — see its doc in session/tmux/start.go).
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
// and amp's first run was never captured. Keying this map on agent behaviour
// would assert things the repo has not measured, and in opencode's case the
// opposite of what it did measure.
const (
	// armGeneric: the arm is isDocTrustPrompt, so docTrustDialogFixture exercises
	// it and the full biconditional is checkable.
	armGeneric = "generic"
	// armAgentSpecific: the arm recognizes only the agent's OWN dialog and does
	// not fall through to isDocTrustPrompt, so the shared fixture cannot exercise
	// it and only gate membership is asserted. That arm is covered by the agent's
	// own cases in runner_test.go. Note this is narrower than "has its own
	// predicate" — claude has one and still falls through, so it is armGeneric.
	armAgentSpecific = "agent-specific"
	// armNone: no trust arm at all, so no dialog can ever read as ready, and the
	// agent must stay out of the gate.
	armNone = "none"
)

// trustReadinessArm must name every supported agent. It exists because the
// obvious version of this test — "skip claude and codex, assert the
// biconditional for the rest" — is blind to the very incident it cites. #729 was
// codex: an agent with its OWN dialog predicate that was missing from the
// dismissal gate. Skipping the agent-specific cases skips that shape entirely,
// and a hand-maintained skip list with no forcing function is the same unforced
// hand-list #2416 was about, reintroduced inside its own fix.
//
// Relabelling a row would DOWNGRADE what is asserted about that agent —
// armGeneric is the only value carrying the full biconditional — so the label is
// not taken on trust. Each value makes a checkable claim about whether the
// generic doc-trust dialog reads as ready, and the test verifies that claim
// against runner.go before using the label to pick an assertion. A row moved to
// silence a failure fails on the label itself.
var trustReadinessArm = map[string]string{
	// claude's arm carries its own strings ("Do you trust", "new MCP server")
	// AND falls through to isDocTrustPrompt, so the generic fixture does exercise
	// it — armGeneric, not agent-specific. The label check below is what caught
	// this; it was initially written as armAgentSpecific on the assumption that
	// "has its own predicate" and "uses the generic one" were exclusive.
	tmux.ProgramClaude: armGeneric,
	// codex is the sole agent-specific member: CodexTrustPromptPresent plus the
	// "›" composer glyph, with no isDocTrustPrompt fallback.
	tmux.ProgramCodex:    armAgentSpecific,
	tmux.ProgramAider:    armGeneric,
	tmux.ProgramGemini:   armGeneric,
	tmux.ProgramAmp:      armGeneric,
	tmux.ProgramOpencode: armGeneric,
	tmux.ProgramDevin:    armNone,
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
// had a trust arm here and was missing from the gate — and #2416 put opencode in
// the same state.
//
// It is not circular: the two sides are independent implementations, neither
// derived from the other, so a desync in either direction fails here. Verified by
// experiment in each — dropping codex from the gate, dropping opencode (#2416's
// own state), and forcing devin in all fail, with distinct messages.
//
// The assertion is chosen per arm rather than by skipping the awkward agents; see
// trustReadinessArm.
func TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss(t *testing.T) {
	fullyChecked := 0
	for _, agent := range tmux.SupportedPrograms {
		arm, named := trustReadinessArm[agent]
		if !named {
			t.Errorf("supported agent %q has no entry in trustReadinessArm; record which "+
				"predicate its isReadyContent arm uses so this invariant can be checked for it", agent)
			continue
		}
		inGate := tmux.ProgramNeedsTrustDismissal(agent)
		ready := isReadyContent(docTrustDialogFixture, agent)

		// Verify the LABEL against runner.go first. Each arm value makes a
		// checkable claim about whether the generic doc-trust dialog reads as
		// ready, so a wrong or convenient label fails here rather than silently
		// changing which assertion the agent gets. Without this the map is a
		// strength dial: relabelling one row to armNone would turn a genuine #729
		// desync green, which is exactly how a reviewer broke the previous version.
		switch arm {
		case armGeneric:
			fullyChecked++
			if !ready {
				t.Errorf("%q is labelled armGeneric, but the generic doc-trust dialog does not make it "+
					"ready — its isReadyContent arm no longer uses isDocTrustPrompt. Fix the row to "+
					"match runner.go rather than leaving it stale", agent)
				continue
			}
			if !inGate {
				t.Errorf("isReadyContent(docTrustDialog, %q) = true but ProgramNeedsTrustDismissal(%q) = false; "+
					"a dialog AF calls ready must be one AF dismisses (#729)", agent, agent)
			}
		case armAgentSpecific:
			if ready {
				t.Errorf("%q is labelled armAgentSpecific, but the generic doc-trust dialog makes it "+
					"ready — its arm now includes isDocTrustPrompt, so it belongs in armGeneric where "+
					"the full biconditional is checked", agent)
			}
			if !inGate {
				t.Errorf("%q is labelled armAgentSpecific — it has its own readiness predicate for its "+
					"own trust dialog — but ProgramNeedsTrustDismissal(%q) = false; that is #729's "+
					"shape, ready with nothing to clear it", agent, agent)
			}
		case armNone:
			if ready {
				t.Errorf("%q is labelled armNone, but the generic doc-trust dialog makes it ready — it "+
					"does have a doc-trust arm, so relabel it armGeneric. Do NOT leave this row as a "+
					"way to skip the check", agent)
			}
			if inGate {
				t.Errorf("%q has no trust arm, so no dialog of its can read as ready, but "+
					"ProgramNeedsTrustDismissal(%q) = true; the loop would buy no dismissal and leave "+
					"only false-positive exposure (#1952)", agent, agent)
			}
		default:
			t.Errorf("agent %q has unknown trust-readiness arm %q", agent, arm)
		}
	}

	// armGeneric is the only value that checks the biconditional. Without this,
	// relabelling every generic row leaves a green test asserting nothing —
	// changing one word could turn a red test green with the defect intact.
	if fullyChecked == 0 {
		t.Fatal("no agent is classified armGeneric, so the biconditional is never checked — " +
			"a row was probably relabelled to silence a failure")
	}

	for agent := range trustReadinessArm {
		if !tmux.IsSupportedProgram(agent) {
			t.Errorf("trustReadinessArm names %q, which is no longer a supported agent; drop its row", agent)
		}
	}
}
