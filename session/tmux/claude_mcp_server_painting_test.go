package tmux

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7390, two follow-ups from the independent review of
// #7382. Claude Code 2.1.322 paints its MCP server dialog top-down, and two
// frames of that paint read clear: the paint stopped right after an option's
// ❯ glyph, and the paint stopped inside the footer on a pane too short for the
// dialog's top. A clear pane lets the create type its prompt and press Enter,
// which confirms the preselected row once the dialog finishes painting.

// mcpServerOptionRows returns the row each option begins on, and the row
// after the last option, in a dialog drawn below lead rows of chrome with its
// preamble and body wrapped at text columns and its options at option
// columns: the layout mcpServerDialogRows (lead 2) and
// framedMCPServerDialogRows (lead 1) draw.
func mcpServerOptionRows(lead, text, option int) (starts []int, end int) {
	at := lead + len(wrapAtSpaces(mcpServerPreamble+" probe", text)) + 1 + len(wrapAtSpaces(mcpServerBody, text)) + 1
	for _, label := range mcpServerOptions {
		starts = append(starts, at)
		at += len(wrapAtSpaces(label, option))
	}
	return starts, at
}

// requireOptionRowsAt pins the layout mcpServerOptionRows computed: each
// option's first row is where it says, so a renderer change fails here, not
// as a frame the tests below never meant to draw.
func requireOptionRowsAt(t *testing.T, rows []string, starts []int, option int) {
	t.Helper()
	for i, label := range mcpServerOptions {
		require.Contains(t, rows[starts[i]], wrapAtSpaces(label, option)[0], "option %d's first row", i)
	}
}

// footerStarts returns each frame of a footer's paint that ends at a word, or
// after its first letter, short of the whole footer: rows is the footer as
// painted whole, and each start is its rows painted that far.
func footerStarts(rows []string, footer string) [][]string {
	var starts [][]string
	for r, row := range rows {
		runes := []rune(row)
		for c := 1; c <= len(runes); c++ {
			atWordEnd := c == len(runes) || runes[c] == ' '
			firstLetter := r == 0 && strings.TrimSpace(string(runes[:c])) == footer[:1]
			if runes[c-1] == ' ' || !(atWordEnd || firstLetter) || strings.Trim(string(runes[:c]), "│ ") == "" {
				// A cut inside the spaces, or before the row's first letter:
				// a box's left edge alone shows none of the footer.
				continue
			}
			start := append(slices.Clone(rows[:r]), string(runes[:c]))
			painted := ""
			for _, piece := range start {
				painted += strings.Trim(piece, "│ ")
			}
			if strings.Contains(claudeSquash(painted), claudeSquash(footer)) {
				// The whole footer: #7382's tests pin it.
				continue
			}
			starts = append(starts, start)
		}
	}
	return starts
}

// Item 1: the paint stops right after the ❯ of the option the cursor is on,
// under the options painted before it, or under the body for the first. A
// bare ❯ there is an option being painted. Master read it clear at every
// width.
func TestCheckAndHandleTrustPrompt_Issue7390MCPServerDialogPaintedToItsCursorHolds(t *testing.T) {
	for width := 6; width <= 80; width++ {
		starts, _ := mcpServerOptionRows(2, max(1, width-4), max(1, width-5))
		for selected := range mcpServerOptions {
			rows := mcpServerDialogRows(width, selected)
			requireOptionRowsAt(t, rows, starts, max(1, width-5))
			painted := append(slices.Clone(rows[:starts[selected]]), "  "+claudeTrustSelectionGlyph)
			requireMCPServerDialogHeld(t, fmt.Sprintf("%d columns, painted as far as the ❯ of option %d", width, selected), painted)
		}
	}
	for width := 8; width <= 54; width += 2 {
		content := width - 4
		starts, _ := mcpServerOptionRows(1, content, max(1, content-2))
		for selected := range mcpServerOptions {
			rows := framedMCPServerDialogRows(width, selected, true)
			requireOptionRowsAt(t, rows, starts, max(1, content-2))
			painted := append(slices.Clone(rows[:starts[selected]]), "│ "+claudeTrustSelectionGlyph)
			requireMCPServerDialogHeld(t, fmt.Sprintf("framed, %d columns, painted as far as the ❯ of option %d", width, selected), painted)
		}
	}
}

// Item 2: a pane too short for the dialog's top, caught while Claude Code
// paints the footer, shows whole option labels over the start of the footer.
// The same rows over the whole footer held; these read clear. Each frame is
// shown from below the preamble's first row (the rest read as the dialog's
// body) and from the first option row down, with the footer painted as far as
// each of its words, or its first letter.
func TestCheckAndHandleTrustPrompt_Issue7390MCPServerDialogScrolledOverItsFooterStartHolds(t *testing.T) {
	for width := 6; width <= 80; width++ {
		selected := width % len(mcpServerOptions)
		rows := mcpServerDialogRows(width, selected)
		starts, end := mcpServerOptionRows(2, max(1, width-4), max(1, width-5))
		requireOptionRowsAt(t, rows, starts, max(1, width-5))
		footer := end + 1
		for _, start := range footerStarts(rows[footer:], rewordedTrustFooter) {
			for _, top := range []int{3, starts[0]} {
				requireMCPServerDialogHeld(t,
					fmt.Sprintf("%d columns, cursor on option %d, rows %d on, the footer painted as far as %q", width, selected, top, start[len(start)-1]),
					append(slices.Clone(rows[top:footer]), start...))
			}
		}
	}
	for width := 8; width <= 54; width += 2 {
		selected := width % len(mcpServerOptions)
		content := width - 4
		starts, end := mcpServerOptionRows(1, content, max(1, content-2))
		inside := framedMCPServerDialogRows(width, selected, true)
		requireOptionRowsAt(t, inside, starts, max(1, content-2))
		// Inside the box: the paint has not reached its closing edge.
		for _, start := range footerStarts(inside[end+1:len(inside)-1], rewordedTrustFooter) {
			requireMCPServerDialogHeld(t, fmt.Sprintf("framed, %d columns, the footer inside painted as far as %q", width, start[len(start)-1]),
				append(slices.Clone(inside[starts[0]:end+1]), start...))
		}
		// Under the box's closing edge.
		outside := framedMCPServerDialogRows(width, selected, false)
		for _, start := range footerStarts(outside[end+1:], rewordedTrustFooter) {
			requireMCPServerDialogHeld(t, fmt.Sprintf("framed, %d columns, the footer outside painted as far as %q", width, start[len(start)-1]),
				append(slices.Clone(outside[starts[0]:end+1]), start...))
		}
	}
}

// The option-block tail is the folder-trust dialogs' too, and reads the start
// of their footers the same way: the options from the cursor row down, over
// the footer painted as far as each of its words.
func TestCheckAndHandleTrustPrompt_Issue7390FolderTrustOptionsOverTheirFooterStartHold(t *testing.T) {
	for width := 6; width <= 60; width += 3 {
		for _, tt := range []struct {
			name, footer string
			rows         []string
		}{
			{"reworded", rewordedTrustFooter, rewordedTrustBodyAndOptionRows(width)},
			{"legacy", legacyTrustFooter, legacyTrustDialogRows(width, true)},
		} {
			cursor := slices.IndexFunc(tt.rows, func(row string) bool { return strings.HasPrefix(row, claudeTrustSelectionGlyph+" 1.") })
			footer := len(tt.rows) - len(wrapAtSpaces(tt.footer, width))
			require.Positive(t, cursor, "%s at %d columns: the cursor row", tt.name, width)
			for _, start := range footerStarts(tt.rows[footer:], tt.footer) {
				content := strings.Join(append(slices.Clone(tt.rows[cursor:footer]), start...), "\n")
				requireHeldWithoutAKey(t, content, fmt.Sprintf("the %s dialog's options at %d columns over the footer painted as far as %q (#7390)",
					tt.name, width, start[len(start)-1]))
			}
		}
	}
}

// Each hold says why, once, and names the dialog: the issue's own two frames.
func TestCheckAndHandleTrustPrompt_Issue7390RefusalsNameWhatThePaneShows(t *testing.T) {
	for _, tt := range []struct{ name, content, saw string }{
		{"painted as far as a ❯", "  New MCP server found in this project: probe\n\n" +
			"  MCP servers may execute code or access system resources. All tool calls\n" +
			"  require approval. Learn more in the MCP documentation.\n\n" +
			"    Use this MCP server\n    Use this and all future MCP servers in this project\n  ❯",
			"its options, still being painted"},
		{"scrolled over the footer's first words", "    in this project\n  ❯ Continue without using this MCP\n    server\n\n  Enter to confirm ·",
			"the start of its footer, still being painted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
			logged := errLog.String()
			require.Equal(t, 1, strings.Count(logged, "refusing to answer"), "the hold is reported once, not once per poll; got:\n%s", logged)
			require.Contains(t, logged, "refusing to answer the "+claudeMCPServerDialogName+" dialog")
			require.Contains(t, logged, tt.saw)
		})
	}
}

// What is no dialog being painted stays clear: the same rows quoted above a
// working agent's composer, a ❯ a rule sits straight above, a ❯ with no option
// left to paint or while another option holds the cursor, and a footer's
// first words that are not the last content, begin no row, or carry a ❯.
func TestCheckAndHandleTrustPrompt_Issue7390RowsThatAreNoPaintStayClear(t *testing.T) {
	const width = 40
	rows := mcpServerDialogRows(width, 2)
	starts, end := mcpServerOptionRows(2, width-4, width-5)
	requireOptionRowsAt(t, rows, starts, width-5)
	toCursor := strings.Join(append(slices.Clone(rows[:starts[2]]), "  ❯"), "\n")
	options := strings.Join(rows[starts[0]:end+1], "\n")
	framed := framedMCPServerDialogRows(width, 2, true)
	noCursor := mcpServerDialogRowsWith(width, -1, false)
	framedStarts, _ := mcpServerOptionRows(1, width-4, width-6)
	for _, tt := range []struct{ name, content string }{
		{"painted to its ❯, above the ruled composer", toCursor + ruledComposerBelow},
		{"painted to its ❯, above the boxed composer", toCursor + boxedComposerBelow},
		{"the options over the footer's first words, above the ruled composer", options + "\n  Enter to confirm ·" + ruledComposerBelow},
		{"the options over the footer's first words, above the boxed composer", options + "\n  Enter to confirm ·" + boxedComposerBelow},
		{"a composer's ❯ under its own rule, below the options", strings.Join(rows[:starts[2]], "\n") + "\n" + strings.Repeat("─", width) + "\n❯"},
		{"a ❯ under a box's closing edge", strings.Join(framed[:framedStarts[2]], "\n") + "\n╰" + strings.Repeat("─", width-2) + "╯\n❯"},
		{"a ❯ under all three options", strings.Join(noCursor[:end], "\n") + "\n  ❯"},
		{"a ❯ under the options while the first holds the cursor", strings.Join(mcpServerDialogRows(width, 0)[:starts[2]], "\n") + "\n  ❯"},
		{"the footer's first words, then a row below them", options + "\n  Enter to confirm ·\n  ? for shortcuts"},
		{"the footer's first words inside a row", options + "\n  That is all. Enter to confirm"},
		{"the footer's first words behind a ❯", options + "\n  ❯ Enter to confirm ·"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}
