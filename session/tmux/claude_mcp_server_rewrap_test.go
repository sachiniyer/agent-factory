package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7364. Claude Code 2.1.322's single-server MCP dialog,
// fully painted on a pane narrow enough to wrap its footer (under 36 columns),
// matched no clause, and CheckAndHandleTrustPrompt reported the pane clear. A
// false return is the create path's permission to type the user's prompt and
// press Enter. That Enter confirms whichever row is preselected, and "Use this
// and all future MCP servers in this project" lets an MCP server run code.

const (
	mcpServerPreamble = "New MCP server found in this project:"
	mcpServerBody     = "MCP servers may execute code or access system resources. All tool calls require approval. Learn more in the MCP documentation."
)

// mcpServerDialogRows renders the dialog as the measured captures paint it
// (testdata/claude_2_1_322_mcp_server_dialog_40_columns.ansi and _50_columns):
// a blank row, the dialog's own top rule across the pane, the preamble and the
// body two columns in and wrapped at width-4, the options behind "  ❯ " or four
// spaces and wrapped at width-5, and the footer like the body. selected is the
// option the cursor is on.
func mcpServerDialogRows(width, selected int) []string {
	return mcpServerDialogRowsWith(width, selected, false)
}

// mcpServerDialogRowsWith is mcpServerDialogRows, with each option behind its
// ordinal ("1. Use this MCP server") when ordinals is set, the way the other
// launch dialogs number theirs. 2.1.322 paints none, but a narrow pane leaves
// an ordinal on a row of its own, which the matcher must read as an option.
func mcpServerDialogRowsWith(width, selected int, ordinals bool) []string {
	body, options := max(1, width-4), max(1, width-5)
	rows := []string{"", strings.Repeat("─", width)}
	indented := func(pieces []string) {
		for _, piece := range pieces {
			rows = append(rows, "  "+piece)
		}
	}
	indented(wrapAtSpaces(mcpServerPreamble+" probe", body))
	rows = append(rows, "")
	indented(wrapAtSpaces(mcpServerBody, body))
	rows = append(rows, "")
	for i, option := range mcpServerOptions {
		if ordinals {
			option = fmt.Sprintf("%d. %s", i+1, option)
		}
		for j, piece := range wrapAtSpaces(option, options) {
			if i == selected && j == 0 {
				rows = append(rows, "  "+claudeTrustSelectionGlyph+" "+piece)
			} else {
				rows = append(rows, "    "+piece)
			}
		}
	}
	rows = append(rows, "")
	indented(wrapAtSpaces(rewordedTrustFooter, body))
	return rows
}

// framedMCPServerDialogRows renders the same dialog inside a box, the frame
// some Claude Code builds draw around their dialogs; the framed form must parse
// as the unframed one does (claudeTrustBoxDrawing). The footer is painted
// inside the box, or under its closing edge.
func framedMCPServerDialogRows(width, selected int, footerInside bool) []string {
	content := max(1, width-4)
	framed := func(row string) string {
		return "│ " + row + strings.Repeat(" ", max(0, content-len([]rune(row)))) + " │"
	}
	rows := []string{"╭" + strings.Repeat("─", max(1, width-2)) + "╮"}
	add := func(pieces ...string) {
		for _, piece := range pieces {
			rows = append(rows, framed(piece))
		}
	}
	add(wrapAtSpaces(mcpServerPreamble+" probe", content)...)
	add("")
	add(wrapAtSpaces(mcpServerBody, content)...)
	add("")
	for i, option := range mcpServerOptions {
		for j, piece := range wrapAtSpaces(option, max(1, content-2)) {
			if i == selected && j == 0 {
				add(claudeTrustSelectionGlyph + " " + piece)
			} else {
				add("  " + piece)
			}
		}
	}
	bottom := "╰" + strings.Repeat("─", max(1, width-2)) + "╯"
	if footerInside {
		add("")
		add(wrapAtSpaces(rewordedTrustFooter, content)...)
		return append(rows, bottom)
	}
	return append(append(rows, bottom), wrapAtSpaces(rewordedTrustFooter, width)...)
}

// The renderer reproduces both narrow captures row for row, so the widths the
// tests below sweep are the dialog as Claude Code paints it, not a guess.
func TestMCPServerDialogRowsMatchTheNarrowCaptures(t *testing.T) {
	for _, tt := range []struct {
		fixture string
		width   int
	}{
		{mcpServerDialog40Fixture, 40},
		{mcpServerDialog50Fixture, 50},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			captured := visibleTextOf(readTrustFixture(t, tt.fixture)).lines()
			for i := range captured {
				captured[i] = strings.TrimRight(captured[i], " ")
			}
			for len(captured) > 0 && captured[len(captured)-1] == "" {
				captured = captured[:len(captured)-1]
			}
			require.Equal(t, captured, mcpServerDialogRows(tt.width, 2))
		})
	}
}

// requireMCPServerDialogHeld asserts a pane showing the dialog is held: never
// reported clear, and sent no key.
func requireMCPServerDialogHeld(t *testing.T, name string, rows []string) {
	t.Helper()
	content := strings.Join(rows, "\n")
	handled, cmds := pollStaticPane(t, content, 2)
	require.True(t, handled, "%s read clear; the create would type its prompt into the dialog (#7364):\n%s", name, content)
	require.Empty(t, sentKeystrokes(cmds), "%s: af presses no key into a dialog it cannot read whole:\n%s", name, content)
}

// The whole dialog, with the cursor on any row, holds at every width below the
// one where af can read it (56 columns, where the "all future" row stops
// wrapping), framed or not.
func TestCheckAndHandleTrustPrompt_MCPServerDialogHoldsAtAnyWidth(t *testing.T) {
	for width := 6; width <= 55; width++ {
		for selected := range mcpServerOptions {
			at := fmt.Sprintf("%d columns, cursor on %q", width, mcpServerOptions[selected])
			requireMCPServerDialogHeld(t, "the whole dialog at "+at, mcpServerDialogRows(width, selected))
			if width < 8 || width > 54 {
				continue
			}
			requireMCPServerDialogHeld(t, "the framed dialog at "+at, framedMCPServerDialogRows(width, selected, true))
			requireMCPServerDialogHeld(t, "the framed dialog, footer outside, at "+at, framedMCPServerDialogRows(width, selected, false))
		}
	}
}

// Scrolled: a pane too short for the dialog shows its last rows, down to a row
// of its options over its footer. Mid-paint: Claude Code paints it top-down, so
// a capture can show any prefix, from the preamble's first row on. Neither is
// ever a clear pane.
func TestCheckAndHandleTrustPrompt_MCPServerDialogScrolledOrMidPaintHolds(t *testing.T) {
	for width := 6; width <= 55; width++ {
		for selected := range mcpServerOptions {
			rows := mcpServerDialogRows(width, selected)
			footer := len(wrapAtSpaces(rewordedTrustFooter, max(1, width-4)))
			for height := footer + 2; height < len(rows); height++ {
				if strings.TrimSpace(rows[len(rows)-height]) == "" {
					continue
				}
				requireMCPServerDialogHeld(t, fmt.Sprintf("%d columns, cursor on row %d, the last %d rows", width, selected, height),
					rows[len(rows)-height:])
			}
			// rows[0] is blank and rows[1] the top rule: the paint shows the
			// dialog from its preamble's first row.
			for painted := 3; painted < len(rows); painted++ {
				requireMCPServerDialogHeld(t, fmt.Sprintf("%d columns, cursor on row %d, %d rows painted", width, selected, painted),
					rows[:painted])
			}
		}
	}
}

// Numbered options on a pane narrow enough to leave an ordinal on a row of its
// own ("❯ 3." over "Continue"): every row of the paint still holds, including
// the frames that stop just after an ordinal.
func TestCheckAndHandleTrustPrompt_MCPServerDialogWithOrdinalsMidPaintHolds(t *testing.T) {
	for width := 6; width <= 15; width++ {
		for selected := range mcpServerOptions {
			rows := mcpServerDialogRowsWith(width, selected, true)
			for painted := 3; painted < len(rows); painted++ {
				requireMCPServerDialogHeld(t, fmt.Sprintf("numbered, %d columns, cursor on row %d, %d rows painted", width, selected, painted),
					rows[:painted])
			}
		}
	}
}

// The hold says why, once, and names the dialog and the one row af would ever
// confirm on it.
func TestCheckAndHandleTrustPrompt_NarrowMCPServerDialogRefusalNamesIt(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	content := strings.Join(mcpServerDialogRows(30, 1), "\n")
	handled, cmds := pollStaticPane(t, content, 4)
	require.True(t, handled, "%s", content)
	require.Empty(t, sentKeystrokes(cmds))
	logged := errLog.String()
	require.Equal(t, 1, strings.Count(logged, "refusing to answer"), "the hold is reported once, not once per poll; got:\n%s", logged)
	require.Contains(t, logged, "refusing to answer the "+claudeMCPServerDialogName+" dialog")
	require.Contains(t, logged, fmt.Sprintf("%q", mcpServerDeclineLabel))
}

// The dialog's text that is not the live dialog stays clear: a working agent's
// composer below it, a row below its footer, a ❯ row that begins no option, or
// its preamble's words inside other text.
func TestCheckAndHandleTrustPrompt_NarrowMCPServerDialogTextThatIsNoDialogStaysClear(t *testing.T) {
	dialog := strings.Join(mcpServerDialogRows(30, 2), "\n")
	options := strings.Join(mcpServerDialogRows(30, 2)[13:], "\n")
	for _, tt := range []struct{ name, content string }{
		{"the ruled composer below", dialog + ruledComposerBelow},
		{"the boxed composer below", dialog + boxedComposerBelow},
		{"the ruled composer below its scrolled options", options + ruledComposerBelow},
		{"a hint row below the footer", dialog + "\n  ? for shortcuts"},
		{"a ❯ draft row under the preamble", "  New MCP server found in\n  this project: probe\n\n❯ what does this mean"},
		{"the preamble's words inside a sentence", "It said: New MCP server found in\nthis project: probe"},
		{"the preamble's first words, then other words", "New MCP server found in\nthe logs today"},
		{"the options under the preamble, then prose", "New MCP server found in\nthis project: probe\n\n" +
			strings.Join(mcpServerOptions, "\n") + "\nThat is what it showed."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}
