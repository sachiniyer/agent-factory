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

// trustDialogKind records, per agent, WHICH first-run dialog it raises. Every
// supported agent must appear; an unnamed one fails the test below.
//
// This map exists because the obvious version of this test — "skip claude and
// codex, assert the biconditional for the rest" — is blind to the very incident
// it cites. #729 was codex: an agent with its OWN dialog predicate that was
// missing from the dismissal gate. Skipping the agent-specific cases skips that
// shape entirely, and a hand-maintained skip list with no forcing function is
// the same unforced hand-list #2416 was about, reintroduced inside its fix.
const (
	// dialogGeneric: raises the shared doc-trust dialog, which
	// DocTrustPromptPresent identifies.
	dialogGeneric = "generic"
	// dialogAgentSpecific: raises its own dialog behind its own predicate
	// (claude's trust/MCP screens, CodexTrustPromptPresent). The fixture below
	// cannot speak for these, so they are asserted only for gate membership;
	// their readiness arms are covered by their own cases in this package.
	dialogAgentSpecific = "agent-specific"
	// dialogNone: AF has no predicate that can identify a dialog for this agent,
	// so it must stay OUT of the gate.
	dialogNone = "none"
)

var trustDialogKind = map[string]string{
	tmux.ProgramClaude:   dialogAgentSpecific,
	tmux.ProgramCodex:    dialogAgentSpecific,
	tmux.ProgramAider:    dialogGeneric,
	tmux.ProgramGemini:   dialogGeneric,
	tmux.ProgramAmp:      dialogGeneric,
	tmux.ProgramOpencode: dialogGeneric,
	tmux.ProgramDevin:    dialogNone,
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
// experiment in both — forcing devin into the gate fails it, and removing
// opencode fails it with #2416's own signature.
//
// What each kind can be asserted about differs, so the assertion is chosen per
// kind rather than skipping the awkward agents:
//
//   - generic: the full biconditional against the shared fixture.
//   - agent-specific: gate membership only. This fixture is not their dialog, so
//     it cannot answer the readiness half; their own cases do.
//   - none: must be out of the gate, since there is nothing AF could clear.
func TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss(t *testing.T) {
	for _, agent := range tmux.SupportedPrograms {
		kind, named := trustDialogKind[agent]
		if !named {
			t.Errorf("supported agent %q has no entry in trustDialogKind; record which "+
				"first-run dialog it raises so this invariant can be checked for it", agent)
			continue
		}
		inGate := tmux.ProgramNeedsTrustDismissal(agent)
		ready := isReadyContent(docTrustDialogFixture, agent)

		switch kind {
		case dialogGeneric:
			if ready != inGate {
				t.Errorf("isReadyContent(docTrustDialog, %q) = %t but ProgramNeedsTrustDismissal(%q) = %t; "+
					"a dialog AF calls ready must be one AF dismisses (#729)", agent, ready, agent, inGate)
			}
		case dialogAgentSpecific:
			if !inGate {
				t.Errorf("%q raises its own trust dialog and has a readiness predicate for it, "+
					"but ProgramNeedsTrustDismissal(%q) = false; that is #729's shape — ready with "+
					"nothing to clear it", agent, agent)
			}
		case dialogNone:
			if inGate {
				t.Errorf("%q has no dialog AF can identify, but ProgramNeedsTrustDismissal(%q) = true; "+
					"the loop would buy no dismissal and leave only false-positive exposure (#1952)", agent, agent)
			}
		default:
			t.Errorf("agent %q has unknown trust-dialog kind %q", agent, kind)
		}
	}

	for agent := range trustDialogKind {
		if !tmux.IsSupportedProgram(agent) {
			t.Errorf("trustDialogKind names %q, which is no longer a supported agent; drop its row", agent)
		}
	}
}
