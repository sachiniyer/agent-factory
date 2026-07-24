package tmux

import (
	"strings"
	"testing"
)

// TestProgramNeedsTrustDismissal_ClassifiesEverySupportedAgent is the drift lock
// for #2416.
//
// The bug was not that opencode was hard to classify — it was that the answer
// lived in two hand-copied switch statements, so adding an agent to one and
// forgetting the other was a silent, shippable mistake. It shipped twice: codex
// (#729) and opencode (#1959 → #2416).
//
// The gate is one function now, but a new agent still defaults quietly to false.
// This table is the forcing function: it must name every entry of
// SupportedPrograms, so adding an agent fails here until someone states which
// side it is on. Do not fix a failure by deleting the row; decide, then record
// the decision with the reason.
//
// READ THIS BEFORE CHOOSING — both answers can cause harm, and the cheap one is
// not obvious. ProgramNeedsTrustDismissal's doc carries the full argument:
//
//   - Wrongly false: an agent whose dialog isReadyContent recognizes reads as
//     READY, nothing clears it, and the briefing is typed into the modal. That
//     is #729, then #2416.
//   - Wrongly true: the agent's live panes get DocTrustPromptPresent on the
//     daemon's one-second poll, and a false positive there types 'D'+Enter into
//     an agent that asked nothing, re-firing every tick (#1952).
//
// So neither answer is a safe default. Characterize the agent's first run, then
// record what you saw.
func TestProgramNeedsTrustDismissal_ClassifiesEverySupportedAgent(t *testing.T) {
	classified := map[string]bool{
		// Dialogs AF positively identifies and clears: claude's trust/MCP screens,
		// codex's directory-trust and safety-buffering modals, and the generic
		// doc-trust dialog DocTrustPromptPresent scopes to aider/gemini.
		ProgramClaude: true,
		ProgramCodex:  true,
		ProgramAider:  true,
		ProgramGemini: true,
		// Grandfathered — the one row that does not meet the "characterize first"
		// rule above. amp's first run was never captured here; it has carried the
		// dismissal check since before this gate was extracted, with no
		// false-positive report against it. Left as it was rather than flipped on
		// an untested guess, and NOT precedent for a new agent. Flipping it is
		// also no longer a free comment change: amp's isReadyContent arm carries
		// isDocTrustPrompt, so task's parity test fails on the desync.
		ProgramAmp: true,
		// Verified to go straight to its composer in a fresh repo
		// (0.0.0-main-202604230742), so no dialog of its own is known. In the set
		// because the omission is exactly what #2416 was, and because its
		// isReadyContent arm carries isDocTrustPrompt regardless — which is the
		// desync task's parity test locks.
		ProgramOpencode: true,
		// Out because AF has no predicate identifying devin's workspace-trust
		// modal: DocTrustPromptPresent cannot true-positive on that wording, so
		// membership would buy no dismissal and leave only the false-positive
		// exposure. Treating an unclearable dialog as handled is the #729 trap
		// (#2410). NOT the claim that the modal never appears — see #2435.
		ProgramDevin: false,
	}

	for _, program := range SupportedPrograms {
		want, named := classified[program]
		if !named {
			t.Errorf("supported agent %q is not classified for trust dismissal; "+
				"characterize its first run, then add it to this table with the reason", program)
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

// TestEnsureDevinWorkspaceTrustSuppressed is the shared-helper contract both
// launch paths depend on (#2435): injectSystemPrompt for ordinary sessions and
// the config-agent spawn both route devin's launch command through here so its
// first-run workspace-trust modal never renders — and so the two can never drift
// on how it is suppressed (the #729/#2097/#2416 class).
//
// The cases mirror what TestInjectSystemPrompt_Devin already pins for the session
// path, asserted here against the function itself so the config-agent path is
// covered by the same contract rather than a copy of it.
func TestEnsureDevinWorkspaceTrustSuppressed(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"bare devin gets the flag", "devin", "devin --respect-workspace-trust false"},
		{
			"abs-path devin gets the flag",
			"/home/u/.local/bin/devin",
			"/home/u/.local/bin/devin --respect-workspace-trust false",
		},
		{
			"a permission-mode override composes with the flag",
			"devin --permission-mode accept-edits",
			"devin --permission-mode accept-edits --respect-workspace-trust false",
		},
		// devin rejects a repeated --respect-workspace-trust ("cannot be used
		// multiple times"), so an already-present flag must be left untouched — in
		// either value form, so a user's explicit choice wins.
		{
			"already false is left alone",
			"devin --respect-workspace-trust false",
			"devin --respect-workspace-trust false",
		},
		{
			"an explicit true wins",
			"devin --respect-workspace-trust true",
			"devin --respect-workspace-trust true",
		},
		// A program_overrides entry can point "devin" at another binary (#1116/#1131);
		// appending a devin flag would make that binary exit on an unknown argument.
		{"a non-devin command is unchanged", "bash", "bash"},
		{"a claude command is unchanged", "claude --dangerously-skip-permissions", "claude --dangerously-skip-permissions"},
		{"empty is unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureDevinWorkspaceTrustSuppressed(tt.command); got != tt.want {
				t.Errorf("EnsureDevinWorkspaceTrustSuppressed(%q) = %q, want %q", tt.command, got, tt.want)
			}
			// Whatever the path, the flag must never end up on the command twice —
			// that is the launch crash the conditional exists to prevent.
			if got := EnsureDevinWorkspaceTrustSuppressed(tt.command); strings.Count(got, devinWorkspaceTrustFlag) > 1 {
				t.Errorf("EnsureDevinWorkspaceTrustSuppressed(%q) = %q carries %q more than once", tt.command, got, devinWorkspaceTrustFlag)
			}
		})
	}
}
