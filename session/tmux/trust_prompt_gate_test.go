package tmux

import "testing"

// TestProgramNeedsTrustDismissal_ClassifiesEverySupportedAgent is the drift lock
// for #2416.
//
// The bug was not that opencode was hard to classify — it was that the answer
// lived in two hand-copied switch statements, so adding an agent to one and
// forgetting the other was a silent, shippable mistake. It shipped twice: codex
// (#729) and opencode (#1959 → #2416).
//
// The gate is one function now, but a new agent still defaults quietly to false,
// and false is the expensive direction: the pane sits on an undismissed dialog
// until readiness times out. This table is the forcing function — it must name
// every entry of SupportedPrograms, so adding an agent fails here until someone
// states which side it is on. Do not fix a failure by deleting the row; decide,
// then record the decision with the reason.
func TestProgramNeedsTrustDismissal_ClassifiesEverySupportedAgent(t *testing.T) {
	classified := map[string]bool{
		// Dialogs AF positively identifies and clears: claude's trust/MCP screens,
		// codex's directory-trust and safety-buffering modals, and the generic
		// doc-trust dialog DocTrustPromptPresent scopes to aider/gemini.
		ProgramClaude: true,
		ProgramCodex:  true,
		ProgramAider:  true,
		ProgramGemini: true,
		// In the set without a known dialog of their own. opencode is verified to
		// go straight to its composer in a fresh repo (0.0.0-main-202604230742);
		// amp's first-run behaviour is not characterized here. Both are in anyway:
		// the check injects nothing unless it identifies a dialog it can clear, so
		// looking is close to free, while being wrongly OUT is how #729 and #2416
		// both shipped.
		ProgramAmp:      true,
		ProgramOpencode: true,
		// Out because AF has no predicate that identifies devin's workspace-trust
		// modal — the check could never match it, and treating an unclearable
		// dialog as handled is the #729 trap (#2410 declined it for the same
		// reason isReadyContent has no devin trust arm). This is NOT the claim
		// that the modal never appears: see #2435.
		ProgramDevin: false,
	}

	for _, program := range SupportedPrograms {
		want, named := classified[program]
		if !named {
			t.Errorf("supported agent %q is not classified for trust dismissal; "+
				"decide whether AF should check its pane for a dialog and add it to this table", program)
			continue
		}
		if got := ProgramNeedsTrustDismissal(program); got != want {
			t.Errorf("ProgramNeedsTrustDismissal(%q) = %t, want %t", program, got, want)
		}
	}

	for program := range classified {
		if !IsSupportedProgram(program) {
			t.Errorf("classified program %q is no longer a supported agent; drop its row", program)
		}
	}
}

// TestProgramNeedsTrustDismissal_RejectsNonAgents holds the invariant the gate
// exists for: anything that is not a known agent gets no dismissal loop, because
// driving one against an arbitrary program is the harm (#1116/#1131).
func TestProgramNeedsTrustDismissal_RejectsNonAgents(t *testing.T) {
	for _, program := range []string{"", "bash", "vim", "claude-wrapper", "/opt/bin/notclaude"} {
		if ProgramNeedsTrustDismissal(program) {
			t.Errorf("ProgramNeedsTrustDismissal(%q) = true, want false for a non-agent", program)
		}
	}
}
