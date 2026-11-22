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
		// Answerable: the whole grammar is present, its whole footer last,
		// cursor on Yes.
		{"inline picker", q + "\n❯ Yes  No\n\n" + legacyTrustFooter, claudeLegacyTrustAnswerable},
		{"inline picker with ordinals", q + "\n❯ 1. Yes  2. No\n\n" + legacyTrustFooter, claudeLegacyTrustAnswerable},
		{"stacked picker", q + "\n❯ 1. Yes\n  2. No\n\n" + legacyTrustFooter, claudeLegacyTrustAnswerable},
		{"boxed layout with labelled options and footer last", legacyTrustBoxed, claudeLegacyTrustAnswerable},
		// 2.0.63 through 2.1.19 paint the reworded dialog's footer under it.
		{"stacked picker under the other footer", q + "\n❯ 1. Yes, proceed\n  2. No, exit\n\n" + rewordedTrustFooter, claudeLegacyTrustAnswerable},

		// Partial: a proper prefix of the grammar — hold, send nothing.
		// #7584: every build paints the footer last, so the whole option
		// block with no footer yet, or its footer only begun, is still
		// being painted.
		{"inline picker, no footer yet", q + "\n❯ Yes  No", claudeLegacyTrustPartial},
		{"inline picker with ordinals, no footer yet", q + "\n❯ 1. Yes  2. No", claudeLegacyTrustPartial},
		{"stacked picker, no footer yet", q + "\n❯ 1. Yes\n  2. No", claudeLegacyTrustPartial},
		{"stacked picker, the blank row above its footer", q + "\n❯ 1. Yes, proceed\n  2. No, exit\n\n", claudeLegacyTrustPartial},
		{"stacked picker, its footer begun", q + "\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm", claudeLegacyTrustPartial},
		{"stacked picker, its footer cut short", q + "\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to e", claudeLegacyTrustPartial},
		{"question only", q + "\n\n\n", claudeLegacyTrustPartial},
		{"question and body, no options yet", q + "\n\n/home/user/project\n", claudeLegacyTrustPartial},
		// Codex P1 (claude_trust.go:680): stacked Yes painted, No not yet.
		{"stacked Yes painted, No not yet", q + "\n❯ 1. Yes\n", claudeLegacyTrustPartial},
		{"labelled Yes painted, No not yet", q + "\n\n❯ 1. Yes, proceed\n", claudeLegacyTrustPartial},
		{"complete dialog with the cursor on No", q + "\n  1. Yes\n❯ 2. No", claudeLegacyTrustPartial},

		// A complete legacy dialog quoted inside a box that closes under the
		// pane's last painted line is a boxed composer's draft whose prompt
		// row has scrolled off, not a live modal: claudeTrustRowOf strips the
		// box sides and reduces the closing edge to blank, so the grammar
		// reads the quote as the dialog's own rows. No build paints its
		// footer inside a box, so such a frame is held (Partial), never
		// answered — Enter would submit the draft (#7648).
		{"boxed quoted legacy dialog, footer inside the box", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},
		{"boxed quoted legacy dialog with its top edge", "" +
			"╭────────────────╮\n" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},
		{"boxed quoted legacy dialog under the legacy footer", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + legacyTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},
		{"boxed quoted legacy dialog with a composer prompt above it, no top edge", "" +
			"│ > Type your message here │\n" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},
		{"boxed quoted inline picker, footer inside the box", "" +
			"│ " + q + " │\n" +
			"│ ❯ Yes  No │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},

		// #7785: the footer row itself carrying a box's side is the quote,
		// whatever closes the box below it — a bottom corner edge, a bare
		// rule no corner opened, or nothing — and however much of the right
		// border a resize clipped away. Each of these is Answerable on
		// master; the footer-row side is what holds it.
		{"boxed quoted legacy dialog, right border clipped by a resize", "" +
			"│ " + q + "\n" +
			"│\n" +
			"│ /home/u/proj\n" +
			"│\n" +
			"│ ❯ 1. Yes, proceed\n" +
			"│   2. No, exit\n" +
			"│\n" +
			"│ " + rewordedTrustFooter + "\n" +
			"╰────────────────────────────────────────\n", claudeLegacyTrustPartial},
		{"boxed quoted legacy dialog, no bottom edge painted", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n", claudeLegacyTrustPartial},
		{"heavy box quoted legacy dialog closed by a bare rule", "" +
			"┃ " + q + " ┃\n" +
			"┃ ❯ 1. Yes, proceed ┃\n" +
			"┃ 2. No, exit ┃\n" +
			"┃ " + rewordedTrustFooter + " ┃\n" +
			"━━━━━━━━━━━━━━━━━━\n", claudeLegacyTrustPartial},

		// Non-regression, not a box-guard pin: the cursor rule alone makes
		// this row Partial on master, so it never exercises either box
		// guard — it only proves a held row stays held.
		{"boxed quoted legacy dialog with the cursor on No (non-regression)", "" +
			"│ " + q + " │\n" +
			"│ 1. Yes, proceed │\n" +
			"│ ❯ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n", claudeLegacyTrustPartial},

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
		// Codex P1 (claude_trust_legacy.go): the same composer draft with NO
		// frame — the grammar only rejected a composer via its frame rule, so
		// an unframed composer whose question sits in the transcript above a
		// "❯ Yes No" draft is indistinguishable from the inline picker on a
		// single visible capture. A launch modal owns a fresh pane from the
		// top, so its question is the first non-blank content; a composer
		// always sits below agent output. Require that for the inline pair.
		{"unframed composer draft Yes No below transcript that quotes the question", "I was asked: \"" + q + "\"\n" +
			q + "\n" +
			"❯ Yes No\n", claudeLegacyTrustNone},
		{"unframed composer draft Yes 2. No below a whole-row question with prose above", "agent output above the quote\n" +
			q + "\n" +
			"❯ Yes 2. No\n", claudeLegacyTrustNone},
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
			require.Equal(t, tt.want, claudeLegacyTrustDialogOf(visibleTextOf(tt.content)))
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
		// The same composer draft with NO frame: the grammar only rejected a
		// composer via its frame rule, so an unframed composer whose question
		// sits in the transcript above a "❯ Yes No" draft is indistinguishable
		// from the inline picker on a single capture. The inline-pair branch
		// requires the question to be the first non-blank content (a launch
		// modal owns a fresh pane from the top; a composer sits below output).
		"I was asked: \"Do you trust the files in this folder?\"\n" +
			"Do you trust the files in this folder?\n" +
			"❯ Yes No\n",
		"agent output above the quote\n" +
			"Do you trust the files in this folder?\n" +
			"❯ Yes 2. No\n",
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

// A complete legacy dialog quoted inside a box that closes under the pane's
// last painted line is a boxed composer's draft whose prompt row has scrolled
// off, not a live modal. claudeTrustRowOf strips the box sides and reduces the
// closing edge to blank, so the legacy grammar reads the quote as the dialog's
// own rows and — without the closed-box guard — would answer it with Enter,
// submitting the user's draft (#7648). The gate must hold it and send nothing.
func TestCheckAndHandleTrustPrompt_BoxedQuotedLegacyDialogHolds(t *testing.T) {
	q := "Do you trust the files in this folder?"
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"footer inside the box, cursor on Yes", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n"},
		{"with the box's top edge", "" +
			"╭────────────────╮\n" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n"},
		{"with a composer prompt row above the quote, no top edge", "" +
			"│ > Type your message here │\n" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n" +
			"╰────────────────╯\n"},
		{"with the legacy footer, Esc to exit", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + legacyTrustFooter + " │\n" +
			"╰────────────────╯\n"},
		// #7785: the footer row itself carries a box's side, so the quote
		// holds whatever closes the box below it — a clipped right border,
		// no bottom edge at all, or a bare rule no corner edge opened.
		{"right border clipped by a resize", "" +
			"│ " + q + "\n" +
			"│\n" +
			"│ /home/u/proj\n" +
			"│\n" +
			"│ ❯ 1. Yes, proceed\n" +
			"│   2. No, exit\n" +
			"│\n" +
			"│ " + rewordedTrustFooter + "\n" +
			"╰────────────────────────────────────────\n"},
		{"no bottom edge painted", "" +
			"│ " + q + " │\n" +
			"│ ❯ 1. Yes, proceed │\n" +
			"│ 2. No, exit │\n" +
			"│ " + rewordedTrustFooter + " │\n"},
		{"heavy box closed by a bare rule", "" +
			"┃ " + q + " ┃\n" +
			"┃ ❯ 1. Yes, proceed ┃\n" +
			"┃ 2. No, exit ┃\n" +
			"┃ " + rewordedTrustFooter + " ┃\n" +
			"━━━━━━━━━━━━━━━━━━\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := runTrustPromptCheck(t, ProgramClaude, tt.content)
			require.True(t, handled, "a quoted draft in a closed box must hold the prompt, not read clear:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "no key may reach a working composer's draft; got %v", cmds)
		})
	}
}
