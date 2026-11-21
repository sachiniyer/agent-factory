package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codexTrustErrorParagraph is the paragraph upstream paints between the
// decline option row and the footer after a failed trust write —
// codex-rs/tui/src/onboarding/onboarding_screen.rs sets it as
// `format!("Failed to set trust for {}: {error}", trust_target.display())`
// and the insta snapshot
// codex_tui__onboarding__trust_directory__tests__renders_snapshot_for_trust_error
// at rust-v0.171.0 renders it verbatim between "  2. Quit" and
// "enter continue · esc quit".
const codexTrustErrorParagraph = "Failed to set trust for /workspace/project: config/batchWrite failed in TUI: Invalid configuration: features.fast_mode=true is not supported; allowed set [fast_mode=false]"

// The error variant of the directory-trust modal in each wording: the rows
// of the ordinary modal with the error paragraph between the decline row
// and the footer, the layout the upstream snapshot shows.
const codexDirectoryTrustDialogErrorOld = `> You are in /tmp/af-home

  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.

› 1. Yes, continue
  2. No, quit

  Failed to set trust for /workspace/project: config/batchWrite failed in
  TUI: Invalid configuration: features.fast_mode=true is not supported;
  allowed set [fast_mode=false]

  Press enter to continue`

const codexDirectoryTrustDialogErrorNew = `  Folder access
  /workspace/project

  Trust this folder? Codex can read, edit, and run files here, subject to your
  permission settings. Folder settings can run code automatically, even
  without a model request. Continue only if you trust these files. Your trust
  decision will be saved.

› 1. Trust and continue
  2. Quit

  Failed to set trust for /workspace/project: config/batchWrite failed in
  TUI: Invalid configuration: features.fast_mode=true is not supported;
  allowed set [fast_mode=false]

  enter continue · esc quit`

// #7735 R1: the error-paragraph variant escapes the plain-wording tail
// grammar — its tail stream ends at the decline row, so every frame from the
// error paragraph until the footer is whole cleared on master, and a settled
// short pane that kept the "›" row but lost the question cleared with it.
// The tail hold now tolerates the same intervening rows between the decline
// row and the footer the complete-modal match tolerates, so every frame
// from the selected row onward holds in the error variant too — and sends no
// key, Enter staying gated on the complete-modal predicate.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogErrorVariantHolds(t *testing.T) {
	for _, modal := range []struct{ name, text string }{
		{"old wording", codexDirectoryTrustDialogErrorOld},
		{"new wording", codexDirectoryTrustDialogErrorNew},
	} {
		for _, width := range []int{12, 17, 40, 80} {
			for idx, frame := range codexTrustPaintFrames(t, modal.text, width) {
				handled, cmds := runTrustPromptCheck(t, ProgramCodex, frame)
				require.True(t, handled,
					"%s error-variant mid-paint at %d cols (frame %d) must not be reported clear:\n%s",
					modal.name, width, idx, frame)
				require.Empty(t, sentKeystrokes(cmds),
					"%s error-variant mid-paint at %d cols (frame %d) must receive no key; got %v",
					modal.name, width, idx, cmds)
			}
		}
	}

	for _, tc := range []struct{ name, content string }{
		// The settled pane from the finding: the "›" row and the decline row
		// survived a too-short pane while the question scrolled, the error
		// paragraph is mid-modal, and the footer is mid-paint.
		{"new wording settled error tail",
			"› 1. Trust and continue\n  2. Quit\n\n  Failed to set trust for /workspace/project: config/batchWrite\n  failed in TUI: Invalid configuration: features.fast_mode=true is\n  not supported; allowed set [fast_mode=false]\n\n  enter continue · esc qu"},
		{"new wording error tail, footer gone",
			"› 1. Trust and continue\n  2. Quit\n\n  Failed to set trust for /workspace/project: config/batchWrite failed\n  in TUI: Invalid configuration: features.fast_mode=true is not\n  supported; allowed set [fast_mode=false]"},
		{"old wording settled error tail",
			"› 1. Yes, continue\n  2. No, quit\n\n  Failed to set trust for /workspace/project: config/batchWrite failed\n  in TUI: Invalid configuration\n\n  Press enter to con"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, tc.content)
			require.True(t, handled,
				"a settled directory-trust error tail with a hidden cursor marks a modal-owned pane")
			require.Empty(t, sentKeystrokes(cmds),
				"the hold path sends no key — Enter stays gated on the complete-modal predicate; got %v", cmds)
		})
	}
}

// The fully painted error variant is still a complete modal upstream paints:
// the error paragraph is intervening rows between the decline row and the
// footer, and the gate answers it exactly as it answers the ordinary modal.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogErrorVariantDismissed(t *testing.T) {
	for _, modal := range []struct{ name, text string }{
		{"old wording", codexDirectoryTrustDialogErrorOld},
		{"new wording", codexDirectoryTrustDialogErrorNew},
	} {
		t.Run(modal.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, modal.text)
			require.True(t, handled,
				"the complete %s error variant must be reported handled", modal.name)
			require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: Enter",
				"the complete %s error variant gets the same Enter the ordinary modal does; got %v", modal.name, cmds)
		})
	}
}

// #7735 R2: two frame shapes the compact matcher answered that upstream
// never paints. The question repeated between the decline row and the footer
// answers because the question check kept ANY anchored occurrence before the
// selected row; it must use the same LAST occurrence master used. Rows split
// where no pane width explains the break — the unwrapped question proves a
// wide pane while every option row is broken — answer because the
// whole-lines match never asked whether a wrap produced the pieces. Both now
// decline the complete-modal match and hold under the trailing-rows clause.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogNeverPaintedShapesHold(t *testing.T) {
	oldQuestion := "  Do you trust the contents of this directory? Working with untrusted contents\n  comes with higher risk of prompt injection."
	shapes := []struct{ name, content string }{
		{"question repeated between decline and footer",
			"> You are in /tmp/af-home\n\n" + oldQuestion + "\n\n" +
				"› 1. Yes, continue\n  2. No, quit\n\n" + oldQuestion + "\n\n  Press enter to continue"},
		{"question repeated between decline and footer, new wording",
			"  Folder access\n  /workspace/project\n\n" +
				"  Trust this folder? Codex can read, edit, and run files here.\n\n" +
				"› 1. Trust and continue\n  2. Quit\n\n" +
				"  Trust this folder? Codex can read, edit, and run files here.\n\n" +
				"  enter continue · esc quit"},
		{"rows split where no width explains it",
			"> You are in /tmp/af-home\n\n" +
				"  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.\n\n" +
				"› 1. Yes,\ncontinue\n  2. No,\nquit\n\n  Press enter\nto continue"},
		{"rows split where no width explains it, new wording",
			"  Folder access\n  /workspace/project\n\n" +
				"  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings. Folder settings can run code automatically, even without a model request. Continue only if you trust these files. Your trust decision will be saved.\n\n" +
				"› 1. Trust and\ncontinue\n  2.\nQuit\n\n  enter continue\n· esc\nquit"},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, CodexTrustPromptPresent(tc.content),
				"a shape upstream never paints must decline the complete-modal match")
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, tc.content)
			require.True(t, handled,
				"the shape still carries the modal's trailing rows, so it holds rather than clears")
			require.Empty(t, sentKeystrokes(cmds),
				"no Enter may be sent on a modal shape upstream never paints; got %v", cmds)
		})
	}
}

// The mirror of the shapes above: the same pieces where wrapping DOES
// explain them — the question is wrapped just as tight — are a real narrow
// modal and get the same Enter as at any other width.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogSplitRowsStillAnswer(t *testing.T) {
	const narrow = "> You are in\n  /tmp/af-home\n\n" +
		"  Do you\n  trust the\n  contents\n  of this\n  directory?\n  Working\n  with\n  untrusted\n  contents\n  comes with\n  higher\n  risk of\n  prompt\n  injection.\n\n" +
		"› 1. Yes,\n     continue\n  2. No,\n     quit\n\n" +
		"  Press\n  enter to\n  continue"
	handled, cmds := runTrustPromptCheck(t, ProgramCodex, narrow)
	require.True(t, handled, "the complete narrow modal must be reported handled")
	require.Contains(t, sentKeystrokes(cmds), "tmux send-keys -t =af_trust: Enter",
		"a row split a real wrap explains is answered like any other complete modal; got %v", cmds)
}

// #7735 R3: a pane whose last content is the modal's question block —
// painted before the selected row is on screen at all, or all a settled
// short pane kept of the block — carries no line-leading "›" of its own,
// but an earlier "›" row left by prior output satisfies the same readiness
// arm, so master cleared it into the live modal. The question-block tail now
// holds under the same hidden-cursor oracle the trailing-rows hold uses.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogQuestionTailHolds(t *testing.T) {
	frames := []struct{ name, content string }{
		{"new question block under earlier › row",
			"› earlier prompt\n\n• Ran go test ./...\n  └ ok\n\n  Folder access\n  /workspace/project\n\n" +
				"  Trust this folder? Codex can read, edit, and run files here, subject to your\n" +
				"  permission settings. Folder settings can run code automatically, even\n" +
				"  without a model request. Continue only if you trust these files. Your trust\n" +
				"  decision will be saved."},
		{"new question block mid-paint",
			"› earlier prompt\n\n  Folder access\n  /workspace/project\n\n  Trust this folder? Codex can read,"},
		{"new modal head as last content",
			"› earlier prompt\n\n  Folder access\n  /workspace/project"},
		{"settled short pane kept only the block's tail",
			"› earlier prompt\n\n  trust these files. Your trust\n  decision will be saved."},
		{"old question block under earlier › row",
			"› earlier prompt\n\n> You are in /tmp/af-home\n\n" +
				"  Do you trust the contents of this directory? Working with untrusted contents\n" +
				"  comes with higher risk of prompt injection."},
		{"old question block mid-paint",
			"› earlier prompt\n\n  Do you trust the contents of this directory? Working with untrusted"},
	}
	for _, frame := range frames {
		t.Run(frame.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, frame.content)
			require.True(t, handled,
				"a directory-trust question block as last content with a hidden cursor marks a modal-owned pane")
			require.Empty(t, sentKeystrokes(cmds),
				"the hold path sends no key — Enter stays gated on the complete-modal predicate; got %v", cmds)
		})
	}
}

// The question-block hold keeps the composer's visible cursor as positive
// evidence the modal is gone: the same block under a visible cursor is
// quoted transcript above an active composer and the pane stays clear.
func TestCheckAndHandleTrustPrompt_CodexDirectoryDialogQuestionTailVisibleCursorClears(t *testing.T) {
	const quoted = "› earlier prompt\n\n• The modal read:\n\n" +
		"  Trust this folder? Codex can read, edit, and run files here, subject to your\n" +
		"  permission settings. Folder settings can run code automatically, even\n" +
		"  without a model request. Continue only if you trust these files. Your trust\n" +
		"  decision will be saved."
	session, commands := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: quoted, cursorVisible: true})
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"a visible cursor means the composer is active, not a modal mid-paint")
	require.Empty(t, sentKeystrokes(*commands),
		"quoted question text above a live composer must never receive Enter; got %v", *commands)
}

// The question-block tail must not swallow ordinary panes: the start screen,
// an idle composer, and a quoted transcript still clear.
func TestCheckAndHandleTrustPrompt_CodexQuestionTailOrdinaryPanesStayClear(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"start screen", "  >_ OpenAI Codex (v0.171.0)\n\n  model:     gpt-5.6-sol max   /model to change\n  directory: ~/agent-factory\n\n› Ask Codex to do anything"},
		{"idle composer", "›\n  gpt-5.6-sol max · ~/agent-factory"},
		{"quoted transcript", "• The modal read:\n\n› 1. Yes, continue\n  2. No, quit\n\n› \n\n  ? for shortcuts                    100% context left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramCodex, tc.content)
			require.False(t, handled,
				"an ordinary Codex pane must not be held by the question-block tail")
			require.Empty(t, sentKeystrokes(cmds), "got %v", cmds)
		})
	}
}
