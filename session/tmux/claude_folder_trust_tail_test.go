package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7338. Every test drives CheckAndHandleTrustPrompt: a
// false return is the create path's permission to type the user's prompt and
// press Enter, which the legacy dialog takes on its preselected "Yes, proceed".

func TestCheckAndHandleTrustPrompt_FolderTrustTailHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		// The legacy dialog's option block over its footer.
		{"legacy, both option rows", "❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit"},
		{"legacy, the cursor on the exit row", "  1. Yes, proceed\n❯ 2. No, exit\n\nEnter to confirm · Esc to exit"},
		{"legacy, no blank row above the footer", "❯ 1. Yes, proceed\n  2. No, exit\nEnter to confirm · Esc to exit"},
		{"legacy, the closing frame above the footer", "│ ❯ 1. Yes, proceed │\n│   2. No, exit    │\n╰──────────────────╯\n  Enter to confirm · Esc to exit"},
		{"legacy, a body row above the options", "Claude Code may read files in this folder.\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit"},
		{"legacy at 12 columns", "❯ 1. Yes,\n  proceed\n  2. No, exit\n\nEnter to\nconfirm ·\nEsc to exit"},
		// The reworded dialog under 16 columns, where its footer wraps.
		{"one option row at 13 columns", "❯ 2. No, exit\n\nEnter to\nconfirm · Esc\nto cancel"},
		{"both option rows at 12 columns", "❯ 1. No, exit\n  2. Yes, I\n  trust this\n  folder\n\nEnter to\nconfirm ·\nEsc to\ncancel"},
		{"one option row at 6 columns", "❯ 2.\nNo,\nexit\n\nEnter\nto\nconfirm\n· Esc\nto\ncancel"},
		{"the top of the pane cuts the exit row", "exit\n\nEnter to\nconfirm · Esc\nto cancel"},
		{"the top of the pane cuts the affirmative row", "  trust this\n  folder\n  2. No, exit\n\nEnter to\nconfirm · Esc\nto cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "a folder-trust dialog is on screen; a clear pane lets the create "+
				"type its prompt into it (#7338):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a dialog af cannot read whole gets no key; got %v", cmds)
			require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
				"the hold is reported once, not once per poll; got:\n%s", errLog.String())
		})
	}
}

// At every width, a pane that shows only a folder-trust dialog's option block
// and footer, from any row of the block down, is never clear and gets no key.
func TestCheckAndHandleTrustPrompt_FolderTrustTailIsNeverClearAtAnyWidth(t *testing.T) {
	for _, dialog := range []struct {
		name    string
		options []string
		footer  string
	}{
		{"reworded", []string{"❯ 1. No, exit", "2. Yes, I trust this folder"}, "Enter to confirm · Esc to cancel"},
		{"reworded, affirmative first", []string{"❯ 1. Yes, I trust this folder", "2. No, exit"}, "Enter to confirm · Esc to cancel"},
		{"legacy", []string{"❯ 1. Yes, proceed", "2. No, exit"}, "Enter to confirm · Esc to exit"},
	} {
		for width := 6; width <= 60; width++ {
			var rows []string
			for _, option := range dialog.options {
				rows = append(rows, wrapAtSpaces(option, width)...)
			}
			rows = append(rows, "")
			footer := wrapAtSpaces(dialog.footer, width)
			rows = append(rows, footer...)
			// Every suffix that shows a row of the option block.
			for height := len(footer) + 2; height <= len(rows); height++ {
				content := strings.Join(rows[len(rows)-height:], "\n")
				name := fmt.Sprintf("%s, %d columns, %d rows", dialog.name, width, height)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
			}
		}
	}
}

// A working agent's composer below the rows, or a row above them that is no
// part of the dialog's option block, and the pane stays clear, as before.
func TestCheckAndHandleTrustPrompt_FolderTrustTailAmongOtherRowsStaysClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"legacy, the composer below", "❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit" + ruledComposerBelow},
		{"legacy, the boxed composer below", "❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit" + boxedComposerBelow},
		{"narrow, the composer below", "❯ 2. No, exit\n\nEnter to\nconfirm · Esc\nto cancel" + ruledComposerBelow},
		{"another picker's row in the block", "❯ Resume previous conversation\n  No, exit\n\nEnter to\nconfirm · Esc\nto cancel"},
		{"a draft row above, narrow", "❯ fix it\n  No, exit\n\nEnter to\nconfirm · Esc\nto cancel"},
		{"prose above, narrow", "That is the dialog.\n  No, exit\n\nEnter to\nconfirm · Esc\nto cancel"},
		{"a hint row below the footer, narrow", "❯ 2. No, exit\n\nEnter to\nconfirm · Esc\nto cancel\n? for shortcuts"},
		{"the footer is another dialog's", "❯ 1. Yes, proceed\n  2. No, exit\n\nPress Enter to continue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// The whole legacy dialog is still answered with its one Enter.
func TestCheckAndHandleTrustPrompt_WholeLegacyDialogIsStillAnswered(t *testing.T) {
	content := "Do you trust the files in this folder?\n\n/home/u/proj\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit"
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
	require.True(t, handled)
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)))
}
