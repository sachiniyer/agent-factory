package task

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// docTrustDialogFixture is the generic documentation-trust dialog, at the full
// length DocTrustPromptPresent requires. It is the one dialog shared across the
// file-seam agents, which is what makes a single fixture enough below.
const docTrustDialogFixture = `Add https://aider.chat/docs/faq.html to the chat?
Open documentation url for more info
(Y)es/(N)o/(D)on't ask again [Yes]:`

// TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss locks the #729 invariant
// across the two enumerations that have to agree about it:
//
//   - isReadyContent decides whether a pane showing a trust dialog counts as
//     READY, and
//   - tmux.ProgramNeedsTrustDismissal decides whether AF will then clear it.
//
// If an agent is ready-on-dialog but not in the gate, AF declares the pane usable
// and types the briefing into an undismissed modal. That is #729 exactly — codex
// had a trust arm here and was missing from the gate — and #2416 put opencode in
// the same state. The reverse desync is milder but still wrong: AF would run a
// dismissal loop for a dialog it never treats as a reason to keep waiting.
//
// #2416 unified the two DISMISSAL sites, but this readiness arm is a third
// enumeration of the same fact and nothing tied it to them. It is not circular:
// the two sides are independent implementations, neither derived from the other,
// so a desync in either direction fails here. Verified it bites by temporarily
// adding devin to the gate — isReadyContent has no devin trust arm, and the case
// failed on that mismatch.
//
// claude and codex are skipped deliberately: they carry agent-specific dialog
// predicates (claude's trust/MCP screens, CodexTrustPromptPresent) rather than
// the generic doc-trust one, so this fixture cannot speak for them. Their arms
// are covered by their own cases in this package and in session/tmux.
func TestTrustDialogIsReadyOnlyForAgentsAFCanDismiss(t *testing.T) {
	covered := 0
	for _, agent := range tmux.SupportedPrograms {
		if agent == tmux.ProgramClaude || agent == tmux.ProgramCodex {
			continue
		}
		covered++
		want := tmux.ProgramNeedsTrustDismissal(agent)
		if got := isReadyContent(docTrustDialogFixture, agent); got != want {
			t.Errorf("isReadyContent(docTrustDialog, %q) = %t but ProgramNeedsTrustDismissal(%q) = %t; "+
				"a dialog AF calls ready must be one AF dismisses (#729)", agent, got, agent, want)
		}
	}
	if covered == 0 {
		t.Fatal("only claude and codex are supported agents; this case asserts nothing")
	}
}
