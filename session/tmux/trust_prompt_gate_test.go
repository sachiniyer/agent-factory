package tmux

import "testing"

// TestProgramHasTrustPrompt_ClassifiesEverySupportedAgent is the drift lock for
// #2416.
//
// The bug was not that opencode was hard to classify — it was that the answer
// lived in two hand-copied switch statements, so adding an agent to one and
// forgetting the other was a silent, shippable mistake. It shipped twice: codex
// (#729) and opencode (#1959 → #2416).
//
// The gate is one function now, but a new agent still defaults quietly to false.
// This table is the forcing function: it must name every entry of
// SupportedPrograms, so adding an agent fails here until someone states which
// side it is on. Do not fix a failure by deleting the row — decide, then record
// the decision with the reason.
func TestProgramHasTrustPrompt_ClassifiesEverySupportedAgent(t *testing.T) {
	classified := map[string]bool{
		// Has a real first-run dialog AF answers.
		ProgramClaude: true,
		ProgramCodex:  true,
		ProgramAider:  true,
		ProgramGemini: true,
		// No dialog of its own; falls through to the generic doc-trust branch and
		// no-ops. Classified true so it can never go missing the way it did in
		// #2416 — the harm there was the omission, not the dismissal.
		ProgramAmp:      true,
		ProgramOpencode: true,
		// AF launches devin with --respect-workspace-trust false, so the modal
		// never renders (#2410). There is nothing to dismiss, and running the
		// dismissal loop anyway would tap Enter into a live composer.
		ProgramDevin: false,
	}

	for _, program := range SupportedPrograms {
		want, named := classified[program]
		if !named {
			t.Errorf("supported agent %q is not classified for trust prompts; "+
				"decide whether it raises a first-run dialog and add it to this table", program)
			continue
		}
		if got := ProgramHasTrustPrompt(program); got != want {
			t.Errorf("ProgramHasTrustPrompt(%q) = %t, want %t", program, got, want)
		}
	}

	for program := range classified {
		if !IsSupportedProgram(program) {
			t.Errorf("classified program %q is no longer a supported agent; drop its row", program)
		}
	}
}

// TestProgramHasTrustPrompt_RejectsNonAgents holds the invariant the gate exists
// for: anything that is not a known agent gets no dismissal loop, because
// tapping Enter into an arbitrary program is the harm (#1116/#1131).
func TestProgramHasTrustPrompt_RejectsNonAgents(t *testing.T) {
	for _, program := range []string{"", "bash", "vim", "claude-wrapper", "/opt/bin/notclaude"} {
		if ProgramHasTrustPrompt(program) {
			t.Errorf("ProgramHasTrustPrompt(%q) = true, want false for a non-agent", program)
		}
	}
}
