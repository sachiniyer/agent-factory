package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyTrustBoxed is the legacy folder-trust dialog in the boxed layout
// older Claude Code builds rendered: the question opens a framed box, the
// options are stacked and carry their own labels, and the "Enter to confirm"
// footer sits below the box. The footer — not the picker — is the last row, and
// the option labels are not bare "Yes"/"No", so a predicate that reads only the
// final row misses it.
const legacyTrustBoxed = "╭──────────────────────────────────────────────╮\n" +
	"│                                              │\n" +
	"│ Do you trust the files in this folder?       │\n" +
	"│                                              │\n" +
	"│ /home/user/project                           │\n" +
	"│                                              │\n" +
	"│ Claude Code may read files in this folder.   │\n" +
	"│                                              │\n" +
	"│ ❯ 1. Yes, proceed                            │\n" +
	"│   2. No, exit                                │\n" +
	"│                                              │\n" +
	"╰──────────────────────────────────────────────╯\n" +
	"   Enter to confirm · Esc to exit\n"

// The legacy dialog owns the pane only when its question, painted as a whole
// row, opens the pane's trailing region and every row below it is one the
// dialog paints, in the dialog's order (claudeLegacyTrustDialogOf). Each case
// here is a frame that verdict must classify; the handler-level assertions
// follow from it: answerable → exactly one Enter, partial → held with no key,
// none → not reported and no key.
func TestClaudeLegacyTrustDialogOf(t *testing.T) {
	q := "Do you trust the files in this folder?"
	for _, tt := range []struct {
		name    string
		content string
		want    claudeLegacyTrustVerdict
	}{
		// Answerable: the whole grammar is present, cursor on Yes.
		{"inline picker", q + "\n❯ Yes  No", claudeLegacyTrustAnswerable},
		{"inline picker with ordinals", q + "\n❯ 1. Yes  2. No", claudeLegacyTrustAnswerable},
		{"stacked picker", q + "\n❯ 1. Yes\n  2. No", claudeLegacyTrustAnswerable},
		{"boxed layout with labelled options and footer last", legacyTrustBoxed, claudeLegacyTrustAnswerable},

		// Partial: a proper prefix of the grammar — hold, send nothing.
		{"question only", q + "\n\n\n", claudeLegacyTrustPartial},
		{"question and body, no options yet", q + "\n\n/home/user/project\n", claudeLegacyTrustPartial},
		// Codex P1 (claude_trust.go:680): stacked Yes painted, No not yet.
		{"stacked Yes painted, No not yet", q + "\n❯ 1. Yes\n", claudeLegacyTrustPartial},
		{"labelled Yes painted, No not yet", q + "\n\n❯ 1. Yes, proceed\n", claudeLegacyTrustPartial},
		{"complete dialog with the cursor on No", q + "\n  1. Yes\n❯ 2. No", claudeLegacyTrustPartial},

		// None: the region holds a row the dialog never paints.
		// Codex P2 (claude_trust.go:585): the composer's frame separates the
		// transcript's question from a draft reading "Yes No".
		{"composer draft Yes No below a whole-row question", q + "\n" +
			"╭────────────────────────────────────╮\n" +
			"│ ❯ Yes No                           │\n" +
			"╰────────────────────────────────────╯\n", claudeLegacyTrustNone},
		{"composer draft Yes 2. No below a whole-row question, rule-framed", q + "\n" +
			"some agent output\n" +
			"────────────────────────────────────\n" +
			"❯ Yes 2. No\n" +
			"────────────────────────────────────\n", claudeLegacyTrustNone},
		{"composer draft Yes No below a quoted question", "I was asked: \"" + q + "\"\n" +
			"╭────────────────────────────────────╮\n" +
			"│ ❯ Yes No                           │\n" +
			"╰────────────────────────────────────╯\n", claudeLegacyTrustNone},
		{"composer draft below the question", q + "\n❯ Yes, I will fix that now", claudeLegacyTrustNone},
		{"content painted below the picker", q + "\n❯ Yes  No\nThat is the end.\n", claudeLegacyTrustNone},
		{"quoted question in prose", "The report quotes \"" + q + "\" verbatim.\n? for shortcuts\n", claudeLegacyTrustNone},
		{"no question at all", "❯ Yes  No", claudeLegacyTrustNone},
		{"footer before any option", q + "\nEnter to confirm · Esc to exit", claudeLegacyTrustNone},
		{"two selected option rows", q + "\n❯ 1. Yes\n❯ 2. No", claudeLegacyTrustNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, claudeLegacyTrustDialogOf(tt.content))
		})
	}
}

// Codex P1 on claude_trust.go:680, at the handler: a stacked picker captured
// after "❯ 1. Yes" is painted but before "2. No" must be HELD. Reporting false
// is the create path's permission to type the initial prompt into the pane,
// which here is the half-drawn picker.
func TestCheckAndHandleTrustPrompt_HalfDrawnStackedLegacyPickerHolds(t *testing.T) {
	for _, content := range []string{
		"Do you trust the files in this folder?\n❯ 1. Yes\n",
		"Do you trust the files in this folder?\n\n❯ 1. Yes, proceed\n",
	} {
		handled, cmds := pollStaticPane(t, content, 4)
		require.True(t, handled, "a half-drawn legacy picker must hold the prompt: %q", content)
		require.Empty(t, sentKeystrokes(cmds), "a half-drawn picker must not receive a key; got %v", cmds)
	}
}

// Codex P2 on claude_trust.go:585, at the handler: the legacy question left in
// the transcript above a composer whose draft reads "Yes No" must not be taken
// for the picker — the Enter would submit the user's draft.
func TestCheckAndHandleTrustPrompt_YesNoComposerDraftIsNotTheLegacyPicker(t *testing.T) {
	for _, content := range []string{
		"Do you trust the files in this folder?\n" +
			"╭────────────────────────────────────╮\n" +
			"│ ❯ Yes No                           │\n" +
			"╰────────────────────────────────────╯\n",
		"I was asked: \"Do you trust the files in this folder?\"\n" +
			"────────────────────────────────────\n" +
			"❯ Yes 2. No\n" +
			"────────────────────────────────────\n",
	} {
		handled, cmds := pollStaticPane(t, content, 4)
		require.False(t, handled, "a composer draft is not the legacy picker: %q", content)
		require.Empty(t, sentKeystrokes(cmds), "no key may reach a working composer; got %v", cmds)
	}
}

// The boxed layout with labelled options and the footer last is answered with
// the historical Enter. Master answers it by the bare phrase; a last-row
// predicate neither answers nor holds it, which hands the create path a pane it
// believes is clear while the dialog is still up.
func TestCheckAndHandleTrustPrompt_BoxedLegacyDialogWithFooterIsAnswered(t *testing.T) {
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, legacyTrustBoxed)
	require.True(t, handled, "the live boxed legacy dialog is in the way")
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)))
}
