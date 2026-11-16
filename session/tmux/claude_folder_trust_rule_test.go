package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7380, item 1. Claude Code 2.1.322 paints its reworded
// folder-trust dialog under a plain "─" rule of its own, the rule
// renderSyntheticClaudeFolderTrust draws above the title. The re-wrap matcher
// read any plain rule anywhere in the pane as a composer's, so on a pane too
// narrow to read the dialog as rows (3 to 13 columns) the fully painted dialog
// read clear, and the create typed its prompt into it.

// The measured shape, under its own rule, with the cursor on either row, in
// either render order, styled or not: never a clear pane, and no key unless
// the dialog reads as rows.
func TestCheckAndHandleTrustPrompt_FolderTrustUnderItsOwnRuleHoldsAtAnyWidth(t *testing.T) {
	for width := 3; width <= 80; width++ {
		for _, options := range [][]string{
			{claudeTrustNoLabel, claudeTrustYesLabel},
			{claudeTrustYesLabel, claudeTrustNoLabel},
		} {
			for selected := range options {
				for _, link := range []string{"", "\x1b\\"} {
					frame := renderSyntheticClaudeFolderTrust(width, options, selected, link)
					name := fmt.Sprintf("%d columns, cursor on %q, linked %v", width, options[selected], link != "")
					handled, cmds := pollStaticPane(t, frame.text, 4)
					require.True(t, handled, "%s read clear; the dialog's own rule is not a composer's (#7380):\n%s", name, frame.text)
					if !frame.readable {
						require.Empty(t, sentKeystrokes(cmds), "%s: no key reaches a dialog af cannot read as rows", name)
					}
				}
			}
		}
	}
}

// The whole legacy dialog under a rule of its own, below the width its
// question fits: never a clear pane, and no key. Under 6 columns it read clear.
// (Scrolled past the rule, it is
// TestCheckAndHandleTrustPrompt_LegacyDialogIsNeverClearUnderItsQuestionWidth.)
func TestCheckAndHandleTrustPrompt_LegacyDialogUnderARuleHolds(t *testing.T) {
	for width := 3; width < len("Do you trust the files in this folder?"); width++ {
		content := strings.Repeat("─", width) + "\n" + strings.Join(legacyTrustDialogRows(width, true), "\n")
		handled, cmds := pollStaticPane(t, content, 2)
		require.True(t, handled, "%d columns read clear:\n%s", width, content)
		require.Empty(t, sentKeystrokes(cmds), "%d columns get no key:\n%s", width, content)
	}
}
