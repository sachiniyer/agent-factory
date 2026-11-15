package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7330, the two classes of frame #7326's review found
// still reading clear. Every test drives CheckAndHandleTrustPrompt itself: a
// false return is the create path's permission to type the user's prompt and
// press Enter.

// Class 1: the old MCP trust modal caught mid-paint on a pane narrow enough to
// wrap its preamble or its question. Every proper prefix of the modal's rows,
// at every width, is a dialog still being painted: never clear, never a key.
func TestCheckAndHandleTrustPrompt_OldMCPModalMidPaintHoldsAtAnyWidth(t *testing.T) {
	for _, separate := range []bool{false, true} {
		for width := 8; width <= 60; width++ {
			rows := oldMCPModalRows(width, separate)
			for painted := 1; painted < len(rows); painted++ {
				content := strings.Join(rows[:painted], "\n")
				name := fmt.Sprintf("separate=%v, %d columns, %d of %d rows painted", separate, width, painted, len(rows))
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
			}
		}
	}
}

func TestCheckAndHandleTrustPrompt_OldMCPModalMidPaintHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"18 columns, both options painted, no footer", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No"},
		{"18 columns, one option painted", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes"},
		{"18 columns, the preamble's first row only", "New MCP server"},
		{"18 columns, cut inside the question", "New MCP server\nfound. Do you"},
		{"30 columns, question on its own rows, cut inside it", "New MCP server found.\nDo you trust this new MCP"},
		{"14 columns, the footer's first row painted", "New MCP server\nfound. Do you\ntrust this new\nMCP server?\n❯ 1. Yes\n  2. No\nEnter to"},
		{"the cursor on No", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n  1. Yes\n❯ 2. No"},
		{"blank rows around the options", "New MCP server\nfound.\nDo you trust this\nnew MCP server?\n\n❯ 1. Yes\n  2. No\n"},
		{"framed", "╭────────────────────╮\n│ New MCP server     │\n│ found. Do you      │\n│ trust this new MCP │\n│ server?            │\n│ ❯ 1. Yes           │"},
		{"7 columns, the cursor row wrapped off its label", "New MCP\nserver\nfound.\nDo you\ntrust\nthis\nnew MCP\nserver?\n❯ 1.\nYes\n  2. No"},
		{"a ❯ row of output straight above the modal", "❯ run me\nNew MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes"},
		{"output above the modal", "Welcome to Claude Code\n\nNew MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "the old MCP trust modal is being painted; a clear pane lets the create "+
				"type its prompt, and its Enter lands on the preselected Yes (#7330):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a dialog still being painted gets no key; got %v", cmds)
		})
	}
}

// The modal's words anywhere but in a run that ends the pane, or in a
// composer, are no modal being painted: the pane stays clear, as before.
func TestCheckAndHandleTrustPrompt_OldMCPModalWordsThatAreNotAPaintStayClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"the words as a composer draft", "────────────────\n❯ New MCP server\n  found. Do you\n────────────────\n"},
		{"the words on the later rows of a composer draft", "────────────────\n❯ Explain this:\n  New MCP server\n  found. Do you\n────────────────\n"},
		{"the words on the later rows of a boxed composer draft", "╭────────────────╮\n│ > Explain this: │\n│   New MCP server │\n│   found. Do you  │\n╰────────────────╯\n"},
		{"the whole modal as the later rows of a draft, no footer", "────────────────\n❯ Explain this:\n  New MCP server found. Do you trust this new MCP server?\n  1. Yes\n  2. No\n────────────────\n  ? for shortcuts\n"},
		{"a status line under the composer that reads new", "────────────────\n❯ \n────────────────\n  new\n"},
		{"a status line under the composer that spells the preamble", "────────────────\n❯ \n────────────────\n  New MCP server\n"},
		{"a status line under the boxed composer", "╭────────────────╮\n│ > Type here     │\n╰────────────────╯\n  New MCP\n"},
		{"a status line under the composer that is the whole preamble", "────────────────\n❯ \n────────────────\n  New MCP server found.\n"},
		{"the preamble and question as the later rows of a draft", "────────────────\n❯ Explain this:\n  New MCP server found.\n  Do you trust this new MCP server?\n────────────────\n"},
		{"a draft whose wrapped last row is new", "────────────────\n❯ make the branch\n  new\n────────────────\n"},
		{"the question quoted above an empty composer", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n\n────────────────\n❯ \n────────────────\n"},
		{"the question quoted above a bare ❯", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n\n❯ "},
		{"the words, then other prose", "New MCP server\nis what the log printed."},
		{"the words inside a sentence", "It printed New MCP server\nfound. Do you"},
		{"another picker under the wrapped question", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ Resume previous conversation\n  Start new conversation"},
		{"a third option row", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No\n  3. Maybe"},
		{"a mid-paint modal quoted above the composer", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No" + ruledComposerBelow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// Class 2: the folder-trust dialog on a pane too short to show its affirmative
// row. The footer under nothing but the dialog's own option rows holds with one
// option row as it does with two, and af says why once.
func TestCheckAndHandleTrustPrompt_FolderTrustOneOptionRowAndFooterHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"the exit row, a blank row and the footer", "  2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"the cursor on the exit row", "❯ 2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"the exit row, the closing frame and the footer", "│   2. No, exit │\n╰───────────────╯\n  Enter to confirm · Esc to cancel"},
		{"the exit row and the footer wrapped", "  2. No, exit\n\nEnter to confirm ·\nEsc to cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "the folder-trust dialog is on screen; a clear pane lets the create "+
				"type its prompt into it (#7330):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a dialog af cannot read whole gets no key; got %v", cmds)
			require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer the "+claudeFolderTrustDialogName),
				"the hold is reported once, not once per poll; got:\n%s", errLog.String())
		})
	}
}

// The exit row over the footer with anything else on screen is not, as far as
// the pane shows, only the dialog's own rows: it stays clear, as before.
func TestCheckAndHandleTrustPrompt_FolderTrustExitRowAmongOtherRowsStaysClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"prose above the exit row", "  some text\n\n  2. No, exit\n\nEnter to confirm · Esc to cancel"},
		{"another picker's row in the run", "❯ No, exit\n  No, and don't ask again\nEnter to confirm · Esc to cancel"},
		{"the composer below", "  2. No, exit\n\nEnter to confirm · Esc to cancel" + ruledComposerBelow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// #7334 review, R1 and R2, for #7326's tail clause: the modal's option row and
// footer inside a composer drawn whole, or under one, are draft or chrome.
func TestCheckAndHandleTrustPrompt_OldMCPModalTailInsideADrawnComposerIsClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"the tail as the later rows of a draft", "────────────────\n❯ Explain this:\n  2. No\n  Enter to confirm\n────────────────\n"},
		{"the tail as the later rows of a boxed draft", "╭──────────────────╮\n│ > Explain this:   │\n│   2. No           │\n│   Enter to confirm │\n╰──────────────────╯\n"},
		{"the tail under the composer", "────────────────\n❯ \n────────────────\n  2. No\n  Enter to confirm\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// A rule straight above the modal's own cursor row is not a composer: the
// scrolled modal still holds.
func TestCheckAndHandleTrustPrompt_OldMCPModalTailUnderARuleStillHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a rule above the option rows", "────────────────\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"a rule above the options and one below the footer", "────────────────\n❯ 1. Yes\n  2. No\nEnter to confirm\n────────────────\n"},
		{"a ❯ row under a rule inside the framed modal, closed by the modal's frame", "╭──────────────────────────────────────╮\n│ New MCP server found. Do you trust     │\n────────────\n❯ \n│ this new MCP server?                   │\n│ ❯ 1. Yes                               │\n│   2. No                                │\n╰──────────────────────────────────────╯\n  Enter to confirm"},
		{"a bare > row under the framed modal's top edge", "╭────────────────────╮\n> \n│ ❯ 1. Yes           │\n│   2. No            │\n╰────────────────────╯\n  Enter to confirm"},
		{"a ❯ row of output above, no composer drawn", "❯ run me\n\n❯ 1. Yes\n  2. No\nEnter to confirm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}
