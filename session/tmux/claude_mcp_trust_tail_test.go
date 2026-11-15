package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7306, the frames #7315's review found still reading
// clear (R1 to R4). Every test here drives CheckAndHandleTrustPrompt itself: a
// false return is the create path's permission to type the user's prompt and
// press Enter, and on each of these frames that Enter lands on the old MCP
// trust modal's preselected "1. Yes".

const (
	oldMCPPreamble = "New MCP server found."
	oldMCPQuestion = "Do you trust this new MCP server?"
	oldMCPFooter   = "Enter to confirm"

	boxedComposerBelow = "\n\n╭────────────────────────────────────╮\n" +
		"│ > Type your message here           │\n" +
		"╰────────────────────────────────────╯\n  ? for shortcuts\n"
	ruledComposerBelow = "\n\n────────────────────────────────────\n❯ \n" +
		"────────────────────────────────────\n  ? for shortcuts\n"
)

// wrapAtSpaces breaks row at its spaces so no piece is wider than width, the
// way Claude Code's renderer wraps a row a pane is too narrow for.
func wrapAtSpaces(row string, width int) []string {
	var out []string
	current := ""
	for _, word := range strings.Fields(row) {
		switch {
		case current == "":
			current = word
		case len([]rune(current))+1+len([]rune(word)) <= width:
			current += " " + word
		default:
			out = append(out, current)
			current = word
		}
	}
	return append(out, current)
}

// oldMCPModalRows renders the old MCP trust modal on a pane width columns
// wide. separate puts the preamble and the question on rows of their own, with
// the blank rows the picker layout draws around the options.
func oldMCPModalRows(width int, separate bool) []string {
	var rows []string
	add := func(row string) { rows = append(rows, wrapAtSpaces(row, width)...) }
	if separate {
		add(oldMCPPreamble)
		add(oldMCPQuestion)
		rows = append(rows, "")
	} else {
		add(oldMCPPreamble + " " + oldMCPQuestion)
	}
	add("❯ 1. Yes")
	add("2. No")
	if separate {
		rows = append(rows, "")
	}
	add(oldMCPFooter)
	return rows
}

func TestCheckAndHandleTrustPrompt_OldMCPModalTailHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		// R1: the whole modal on a pane 21 to 53 columns wide. The question
		// wraps, so no row holds it, and the preamble is on screen.
		{"the question wrapped at 47 columns", "New MCP server found. Do you trust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"the question wrapped on its own rows at 30 columns", "New MCP server found.\nDo you trust this new MCP\nserver?\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm"},
		{"the question wrapped, framed", "╭──────────────────────────────╮\n│ New MCP server found. Do you │\n│ trust this new MCP server?   │\n│ ❯ 1. Yes                     │\n│   2. No                      │\n╰──────────────────────────────╯\n  Enter to confirm"},
		{"the preamble in output above a scrolled modal", "Earlier: new mcp server found in the logs.\n\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		// R2: a pane two or three rows tall shows one option row.
		{"the No row and the footer", "  2. No\nEnter to confirm"},
		{"the No row, a blank row and the footer", "  2. No\n\nEnter to confirm"},
		{"the cursor on the No row, and the footer", "❯ 2. No\nEnter to confirm"},
		{"the Yes row and the footer", "❯ 1. Yes\nEnter to confirm"},
		// R3: a footer af cannot read as one row, and a row below the footer.
		{"the footer wrapped under 16 columns", "❯ 1. Yes\n  2. No\nEnter to\nconfirm"},
		{"the footer wrapped one word a row", "  2. No\nEnter\nto\nconfirm"},
		{"the whole modal at 13 columns", "New MCP\nserver found.\nDo you trust\nthis new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to\nconfirm"},
		{"a longer footer wrapped", "❯ 1. Yes\n  2. No\nEnter to confirm · Esc\nto cancel"},
		{"a wrapped footer split by the closing frame", "│ ❯ 1. Yes │\n│   2. No  │\n│ Enter to │\n╰──────────╯\n  confirm"},
		{"a hint row below the footer", "❯ 1. Yes\n  2. No\nEnter to confirm\n\n? for shortcuts"},
		{"a hint row below the whole modal's footer", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm\n? for shortcuts"},
		// This one read clear by design before #7306 ("quoted with trailing
		// prose"). af cannot tell a row of prose under the footer from a hint
		// row the modal draws there, and nothing below it shows a working
		// agent, so it holds. The folder-trust twin holds the same frame
		// ("quoted with output below and no composer").
		{"a row of prose below the whole modal's footer", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm\nThat is the MCP trust dialog af answers.\n"},
		// A pane too short for any option row, the footer wrapped or under
		// the closing frame.
		{"only the footer, wrapped", "Enter to\nconfirm"},
		{"only the footer, under the closing frame", "╰────────────────╯\n  Enter to confirm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "the old MCP trust modal is on screen; a clear pane lets the create "+
				"type its prompt and press Enter on the preselected Yes (#7306):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a frame af cannot read whole gets no key; got %v", cmds)
		})
	}
}

// At every width and every height that shows the footer's first row, the modal
// is never a clear pane. It is answered with the one Enter only when the pane
// shows the preamble and the whole question on one row, as before #7306, and
// held with no key otherwise.
func TestCheckAndHandleTrustPrompt_OldMCPModalIsNeverClearAtAnySize(t *testing.T) {
	for _, separate := range []bool{false, true} {
		for width := 8; width <= 60; width++ {
			rows := oldMCPModalRows(width, separate)
			footerRows := len(wrapAtSpaces(oldMCPFooter, width))
			for height := footerRows; height <= len(rows); height++ {
				content := strings.Join(rows[len(rows)-height:], "\n")
				name := fmt.Sprintf("separate=%v, %d columns, %d rows", separate, width, height)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%s reads clear:\n%s", name, content)
				whole := height == len(rows) && strings.Contains(content, oldMCPQuestion) &&
					strings.Contains(content, oldMCPPreamble)
				if whole {
					keys := injectedKeyNames(sentKeystrokes(cmds))
					require.NotEmpty(t, keys, "%s is the whole modal and keeps its Enter:\n%s", name, content)
					for _, key := range keys {
						require.Equal(t, "Enter", key, "%s is answered with Enter and nothing else:\n%s", name, content)
					}
				} else {
					require.Empty(t, sentKeystrokes(cmds), "%s gets no key:\n%s", name, content)
				}
			}
		}
	}
}

// A working agent paints its composer below its output, so the modal's rows
// with a composer under them are output being quoted: the pane is clear and
// gets no key.
func TestCheckAndHandleTrustPrompt_OldMCPModalTailAboveTheComposerIsClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"the wrapped modal, boxed composer", "● Here is the dialog:\n\nNew MCP server found. Do you trust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to confirm" + boxedComposerBelow},
		{"the wrapped modal, ruled composer", "New MCP server found. Do you trust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to confirm" + ruledComposerBelow},
		{"the options and footer, boxed composer", "❯ 1. Yes\n  2. No\nEnter to confirm" + boxedComposerBelow},
		{"the options and footer, ruled composer", "❯ 1. Yes\n  2. No\nEnter to confirm" + ruledComposerBelow},
		{"one option row and the footer, ruled composer", "  2. No\nEnter to confirm" + ruledComposerBelow},
		{"a wrapped footer, ruled composer with a draft", "  2. No\nEnter to\nconfirm\n\n────────────\n❯ fix the bug\n────────────\n"},
		{"a hint row below the footer, then the composer", "❯ 1. Yes\n  2. No\nEnter to confirm\n? for shortcuts" + ruledComposerBelow},
		{"only the footer, ruled composer", "Enter to confirm" + ruledComposerBelow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "a quoted modal above the agent's composer is not in the way:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// Another picker's rows over the same footer are not this modal's option rows
// and stay clear, as before.
func TestCheckAndHandleTrustPrompt_OtherPickerOverTheFooterIsNotTheOldMCPModal(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"labels that are not Yes or No", "❯ Resume previous conversation\n  Start new conversation\nEnter to confirm"},
		{"labels that end in yes and no", "❯ Always yes\n  Definitely no\nEnter to confirm"},
		{"a wrapped footer under another label", "  Start new conversation\nEnter to\nconfirm"},
		{"prose that is not the footer", "  2. No\nPress Enter to confirm the change."},
		{"the option rows with no footer under prose", "❯ 1. Yes\n  2. No\nThat is all."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// R4: the hold ends in the create's trust budget running out, so af says once
// why it is holding and what it saw, not once per poll.
func TestCheckAndHandleTrustPrompt_OldMCPModalTailHoldIsReportedOnce(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	handled, cmds := pollStaticPane(t, "  2. No\nEnter to\nconfirm", 6)
	require.True(t, handled)
	require.Empty(t, sentKeystrokes(cmds))
	logged := errLog.String()
	require.Equal(t, 1, strings.Count(logged, "refusing to answer the "+claudeLaunchGateDialogName),
		"the hold is reported once, not once per poll; got:\n%s", logged)
	require.Contains(t, logged, `"Enter to confirm"`, "the report names the footer af saw")
	require.Contains(t, logged, `"No"`, "the report names the option row af saw")
}
