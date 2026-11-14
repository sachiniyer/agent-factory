package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeFolderTrustDialogOf is the one decision every folder-trust path reads
// (#7271). Its table names a shape per verdict; the end-to-end sweeps in
// claude_folder_trust_region_test.go cover the synthetic dialog at every width
// and every painted prefix.
func TestClaudeFolderTrustDialogOf(t *testing.T) {
	const (
		question = "Quick safety check:\nIs this a project you created or one you trust?\n\n"
		options  = "❯ No, exit\n  Yes, I trust this folder\n"
		footer   = "\nEnter to confirm · Esc to cancel\n"
		composer = "\n────────────────────\n❯ \n────────────────────\n  ? for shortcuts\n"
	)
	for _, tt := range []struct {
		name    string
		content string
		want    claudeFolderTrustVerdict
	}{
		// Complete: the whole dialog, every row af reads by on one row.
		{"the dialog", question + options + footer, claudeFolderTrustComplete},
		{"the question inside its sentence", "  Quick safety check: Is this a project you created or one you trust? (Like\n  your own code.\n\n" + options + footer, claudeFolderTrustComplete},
		{"numbered rows", question + "❯ 1. No, exit\n  2. Yes, I trust this folder\n" + footer, claudeFolderTrustComplete},
		{"the cursor on an option a later build added", question + "  No, exit\n❯ No, and don't ask again\n  Yes, I trust this folder\n" + footer, claudeFolderTrustComplete},
		{"no cursor, caught mid-repaint: the parse refuses it", question + "  No, exit\n  Yes, I trust this folder\n" + footer, claudeFolderTrustComplete},
		{"framed", "╭──────────╮\n│ Is this a project you created or one you trust? │\n│ │\n│ ❯ No, exit │\n│   Yes, I trust this folder │\n│ │\n│ Enter to confirm · Esc to cancel │\n╰──────────╯\n", claudeFolderTrustComplete},
		// #7275 review, R2: a closing frame directly above the footer is the
		// dialog's own, as in the box the repo's legacy fixtures draw.
		{"the box closed above the footer", "╭──────────╮\n│ Is this a project you created or one you trust? │\n│ │\n│ ❯ 1. No, exit │\n│   2. Yes, I trust this folder │\n│ │\n╰──────────╯\n   Enter to confirm · Esc to exit\n", claudeFolderTrustComplete},
		{"a closing rule straight under the options", question + options + "────────\nEnter to confirm · Esc to cancel\n", claudeFolderTrustComplete},

		// Partial: a proper prefix, the last row possibly cut short.
		{"the question and body only", question + "Claude Code'll be able to read files here.\n", claudeFolderTrustPartial},
		{"the cursor row only", question + "❯ No, exit\n", claudeFolderTrustPartial},
		{"the ❯ glyph only", question + "❯\n", claudeFolderTrustPartial},
		{"the options, no footer yet", question + options, claudeFolderTrustPartial},
		{"the footer cut short", question + options + "\nEnter to con\n", claudeFolderTrustPartial},
		{"the question quoted with prose below and no composer", "The docs ask: \"Is this a project you created or one you trust?\"\nHere is a summary.\n", claudeFolderTrustPartial},

		// Partial too: a repaint after af's own Down caught half done, with the
		// footer still painted (#7280, item 1).
		{"the affirmative row erased mid-repaint", question + "❯ No, exit\n\n" + footer, claudeFolderTrustPartial},
		{"the affirmative row erased with the cursor", question + "  No, exit\n\n" + footer, claudeFolderTrustPartial},
		{"the ❯ row half-written mid-repaint", question + "  No, exit\n❯ Yes, I tr\n" + footer, claudeFolderTrustPartial},

		// Partial too: a ❯ above the question is not the agent's composer
		// below the dialog, so the dialog holds and is never answered
		// (#7280, item A).
		{"a ❯ above the question", "❯ hello\n" + question + options + footer, claudeFolderTrustPartial},
		{"a composer above the dialog", composer + question + options + footer, claudeFolderTrustPartial},

		// Partial too: a ❯ row cut short of one of the dialog's own labels is
		// the dialog's, never a composer (#7280, item B).
		{"a ❯ row cut short of an option label below the options", question + options + "\n❯ No, ex\n" + footer, claudeFolderTrustPartial},
		// A ❯ draft that reads like the footer is no footer (#7280, item D).
		{"a ❯ draft reading like the footer, straight under the options", question + options + "❯ Enter to confirm · Esc to cancel\n", claudeFolderTrustPartial},
		// ...and no option row either: a row that reads as the footer breaks
		// the grammar in the body or the option block, as on master (#7284
		// review, R1). Before, its run's affirmative made it an option, and af
		// sent Down or Up toward the affirmative.
		{"a ❯ footer-text row above the affirmative", question + "❯ Enter to confirm · Esc to cancel\n    Yes, I trust this folder\n" + footer, claudeFolderTrustPartial},
		{"a ❯ footer-text row below the affirmative", question + "    Yes, I trust this folder\n  ❯ Enter to confirm · Esc to cancel\n" + footer, claudeFolderTrustPartial},
		// Composer evidence counts only with no row the dialog paints — an
		// option row or its footer — below it (#7284 review, R2).
		{"a ❯ footer-text row between the options and the dialog's own footer", question + "    No, exit\n  ❯ Yes, I trust this folder\n\n❯ Enter to confirm · Esc to cancel\n  Enter to confirm · Esc to cancel\n", claudeFolderTrustPartial},
		{"an empty ❯ row between the options and the footer", question + options + "\n❯\n" + footer, claudeFolderTrustPartial},
		{"another dialog's options under the question, with the footer below", question + "❯ Use this MCP server\n  Continue without using this MCP server\n" + footer, claudeFolderTrustPartial},
		// C's two rules are the composer's own, plain horizontal rules: a box's
		// corners and tees are the dialog's (#7284 review, R5).
		{"a ❯ row between a box's tee and its corner", "╭──────────╮\n│ Is this a project you created or one you trust? │\n│ │\n│ ❯ 1. No, exit │\n│   2. Yes, I trust this folder │\n├──────────┤\n│ ❯ │\n╰──────────╯\n", claudeFolderTrustPartial},
		{"a ❯ row in a box under the question", question + "╭──────────╮\n│ ❯ │\n╰──────────╯\n", claudeFolderTrustPartial},

		// Partial too: a row the dialog does not paint, in or below its footer
		// slot, with no composer below to show a working agent. The live
		// dialog can paint one (#7275 review, R1).
		{"the Ctrl-C footer swap", question + options + "\nPress Ctrl-C again to exit\n", claudeFolderTrustPartial},
		{"a tip row after a blank below the footer", question + options + footer + "\n※ Tip: run /terminal-setup\n", claudeFolderTrustPartial},
		{"a tip row between the options and the footer", question + options + "\n※ Tip: press Esc twice to rewind\n" + footer, claudeFolderTrustPartial},
		{"a notice straight below the footer", question + options + footer + "Update available: run claude update\n", claudeFolderTrustPartial},
		{"a right-aligned status row", question + options + footer + "\n                              ◐ medium · /effort\n", claudeFolderTrustPartial},
		{"a stray stderr line", question + options + footer + "(node:4242) Warning: an unhandled stderr line\n", claudeFolderTrustPartial},
		{"a leftover footer fragment", question + options + footer + "cancel\n", claudeFolderTrustPartial},
		{"quoted with output below and no composer", question + options + footer + "\nThat is the dialog af answers at startup.\n", claudeFolderTrustPartial},
		{"quoted with output straight below the footer and no composer", question + options + "\nEnter to confirm · Esc to cancel\nThat is the dialog.\n", claudeFolderTrustPartial},
		// #7275 review, R2: a rule anywhere but the closing frame is no
		// working agent by itself.
		{"a separator between the body and the options", "Quick safety check:\nIs this a project you created or one you trust?\n────────\n" + options + footer, claudeFolderTrustPartial},
		{"a rule between the options and the footer slot", question + options + "────────\n\nEnter to confirm · Esc to cancel\n", claudeFolderTrustPartial},
		{"a rule below the footer, then prose", question + options + footer + "────────\nThat is the dialog.\n", claudeFolderTrustPartial},
		{"a composer above the options is not below them", "Quick safety check:\nIs this a project you created or one you trust?\n" + composer + "\n" + options + footer, claudeFolderTrustPartial},

		// Unreadable: the whole dialog, a row af reads by wrapped.
		{"the question wrapped", "  Quick safety check: Is this a project you\n  created or one you trust? (Like your own\n  code.\n\n" + options + footer, claudeFolderTrustUnreadable},
		{"the affirmative label wrapped", question + "❯ No, exit\n  Yes, I trust this\n  folder\n" + footer, claudeFolderTrustUnreadable},
		{"the cursor on the wrapped label", question + "  No, exit\n❯ Yes, I trust this\n  folder\n" + footer, claudeFolderTrustUnreadable},
		{"the footer wrapped", question + options + "\nEnter to confirm · Esc to\ncancel\n", claudeFolderTrustUnreadable},
		// Unreadable too: the question scrolled off the top of a pane too
		// short for the dialog, its own option block and footer the last
		// content (#7280, item 2).
		{"the question scrolled off the top", "  your own code, a well-known open source project.\n\n  Claude Code'll be able to read files here.\n\n" + options + footer, claudeFolderTrustUnreadable},
		{"the question scrolled off, the box closed above the footer", "│ your own code. │\n│ │\n│ ❯ 1. No, exit │\n│   2. Yes, I trust this folder │\n│ │\n╰──────────╯\n   Enter to confirm · Esc to exit\n", claudeFolderTrustUnreadable},
		{"only the options and the footer on screen", options + footer, claudeFolderTrustUnreadable},
		// Unreadable too: a pane too short to show any option row whole, so
		// that only the footer is left, whole or wrapped, with at most blank
		// rows and the tail of the wrapped affirmative above it (#7285).
		{"only the footer on screen", "  Enter to confirm · Esc to cancel\n", claudeFolderTrustUnreadable},
		{"only blank rows and the footer on screen", "\n\n  Enter to confirm · Esc to cancel\n\n", claudeFolderTrustUnreadable},
		{"only the wrapped footer on screen", "  Enter to confirm ·\n  Esc to cancel\n", claudeFolderTrustUnreadable},
		{"the affirmative's tail and the wrapped footer", "    folder\n\n  Enter to confirm ·\n  Esc to cancel\n", claudeFolderTrustUnreadable},
		{"a longer tail of the affirmative and the footer", "    trust this\n    folder\n\n  Enter to confirm · Esc to cancel\n", claudeFolderTrustUnreadable},

		// None: the agent's composer below the dialog's text — its ❯ prompt
		// row, or the boxed composer's "> " row under its rule — or no dialog
		// at all.
		{"quoted above the composer", question + options + footer + composer, claudeFolderTrustNone},
		{"quoted above the boxed composer", question + options + footer + "\n╭──────────╮\n│ > Type your message here │\n╰──────────╯\n? for shortcuts\n", claudeFolderTrustNone},
		{"quoted above a permission prompt", question + options + footer + "\n────────\n Bash command\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No, and tell Claude what to do differently (esc)\n", claudeFolderTrustNone},
		{"quoted above prose and then the composer", question + options + footer + "\nThat is the dialog af answers at startup.\n" + composer, claudeFolderTrustNone},
		{"quoted with the composer straight below the footer", question + options + "\nEnter to confirm · Esc to cancel\n❯ draft\n", claudeFolderTrustNone},
		{"the question above a composer draft", question + "❯ I will select \"Yes, I trust this folder\" for you\n", claudeFolderTrustNone},
		{"the question above the composer's rule", question + composer, claudeFolderTrustNone},
		{"a composer above and below the dialog", composer + question + options + footer + composer, claudeFolderTrustNone},
		// A picker of the agent's own whose ❯ row reads as a prefix of an
		// option ("Yes") is still a working agent: its run holds rows the
		// dialog never paints (#7280, item B's scope).
		{"quoted above a picker whose ❯ row reads as a prefix", question + options + footer + "\n❯ Yes\n  Yes, and don't ask again\n", claudeFolderTrustNone},
		// #7280, item C: a ❯ row between two rules is the composer, even as the
		// last row painted.
		{"a quote ending in the composer between its rules", question + options + footer + "────────\n❯ \n────────\n", claudeFolderTrustNone},
		// The composer's rules are "─" alone, as the measured composer draws
		// them (#7285, the #7284 review's N2): a ❯ between heavier or double
		// rules is not the composer, so it holds.
		{"a ❯ row between double rules", question + options + footer + "════════\n❯ \n════════\n", claudeFolderTrustPartial},
		{"a ❯ row between heavy rules", question + options + footer + "━━━━━━━━\n❯ \n━━━━━━━━\n", claudeFolderTrustPartial},
		// #7280, item D: a ❯ row is never the footer, so the rule above it is
		// no closing frame, and the row between rules is the composer.
		{"a ❯ draft reading like the footer, between rules", question + options + "\n────────\n❯ Enter to confirm · Esc to cancel\n────────\n", claudeFolderTrustNone},
		// #7280, item 2: a pane without the question holds only on the
		// dialog's own rows, with nothing above them that the dialog does not
		// paint.
		{"the question scrolled off, quoted above the composer", "  your own code.\n\n" + options + footer + composer, claudeFolderTrustNone},
		{"the question scrolled off, a ❯ above the options", "❯ hello\n  your own code.\n\n" + options + footer, claudeFolderTrustNone},
		{"the question scrolled off, a rule above the options", "────────\n  your own code.\n\n" + options + footer, claudeFolderTrustNone},
		{"options and a footer without the affirmative", "  some text\n\n❯ No, exit\n  No, and don't ask again\n" + footer, claudeFolderTrustNone},
		{"only the footer, under a quote's last row", "  That is the dialog.\n" + footer, claudeFolderTrustNone},
		{"only the footer, under a rule", "────────\n" + footer, claudeFolderTrustNone},
		{"only the footer, under a ❯ row", "❯ draft\n" + footer, claudeFolderTrustNone},
		{"only the footer, under a label that is not the affirmative's tail", "    exit\n" + footer, claudeFolderTrustNone},
		{"the question in a composer draft", "────\n❯ Is this a project you created or one you trust?\n  Yes, I trust this folder\n────\n", claudeFolderTrustNone},
		{"the legacy dialog", "Do you trust the files in this folder?\n❯ Yes  No\n", claudeFolderTrustNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, claudeFolderTrustDialogOf(visibleTextOf(tt.content)), "\n%s", tt.content)
		})
	}
}

// A dialog in the region at the bottom of the pane decides, whatever is quoted
// above it: the region opens at the LAST question.
func TestClaudeFolderTrustDialogOf_ReadsTheLastQuestion(t *testing.T) {
	live := "Quick safety check:\nIs this a project you created or one you trust?\n\n" +
		"❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
	quotedAbove := "Earlier: Is this a project you created or one you trust?\nsome output\n\n" + live
	require.Equal(t, claudeFolderTrustComplete, claudeFolderTrustDialogOf(visibleTextOf(quotedAbove)))
}

// The synthetic dialog's verdict at every width agrees with whether its rows
// fit, and a Complete one always parses to the row that owns the cursor.
func TestClaudeFolderTrustDialogOf_SyntheticDialogAtEveryWidth(t *testing.T) {
	options := []string{claudeTrustNoLabel, claudeTrustYesLabel}
	for width := 24; width <= 120; width++ {
		for selected := range options {
			for _, link := range []string{"", "\x1b\\", "\a"} {
				frame := renderSyntheticClaudeFolderTrust(width, options, selected, link)
				t.Run(fmt.Sprintf("%d columns, cursor on %d, link %q", width, selected, link), func(t *testing.T) {
					screen := visibleTextOf(frame.text)
					got := claudeFolderTrustDialogOf(screen)
					if !frame.readable {
						require.Equal(t, claudeFolderTrustUnreadable, got)
						return
					}
					require.Equal(t, claudeFolderTrustComplete, got)
					dialog, err := parseClaudeFolderTrustDialog(screen)
					require.NoError(t, err)
					require.Equal(t, options[selected], dialog.selectedLabel())
					require.False(t, strings.Contains(string(screen), "\x1b"))
				})
			}
		}
	}
}
