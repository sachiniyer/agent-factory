package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7380, item 2. A box above a folder-trust dialog's
// options is a composer by what is inside it: its first row past any pad rows
// is a prompt in one of the composer's modes, "> " (prompt), "! " (bash) or
// "# " (memory). The test it replaces asked whether a "> " row sat next to the
// box's edge, on either side, so it misread boxes both ways.

const quotedFolderOptions = "❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"

// Over-hold: quoted options and footer, nothing below, under a composer box
// the "> "-only, edge-adjacent test did not read as one.
func TestCheckAndHandleTrustPrompt_QuotedOptionsUnderAComposerBoxStayClear(t *testing.T) {
	for _, tt := range []struct{ name, box string }{
		{"a bash-mode composer", "╭──────────────────────╮\n│ ! ls                 │\n╰──────────────────────╯"},
		{"a memory-mode composer", "╭──────────────────────╮\n│ # note               │\n╰──────────────────────╯"},
		{"a composer whose prompt sits behind a pad row", "╭──────────────────────╮\n│                      │\n│ > npm test           │\n│                      │\n╰──────────────────────╯"},
		{"the prompt composer", "╭──────────────────────╮\n│ > Type your message  │\n╰──────────────────────╯"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.box + "\n" + quotedFolderOptions
			handled, cmds := pollStaticPane(t, content, 4)
			require.False(t, handled, "a composer above quoted options is no dialog:\n%s", content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// Under-hold: a row outside the box never decides it. A shell's "> claude"
// line straight above a live framed dialog's top edge is not the dialog's
// prompt row, and the dialog holds.
func TestCheckAndHandleTrustPrompt_FramedDialogUnderAShellLineHolds(t *testing.T) {
	framed := "╭───────────╮\n│ Security  │\n│ guide     │\n│           │\n│ ❯ 1. Yes, I │\n│ trust this │\n│ folder    │\n│   2. No,  │\n│ exit      │\n╰───────────╯\nEnter to\nconfirm · Esc\nto cancel"
	for _, tt := range []struct{ name, content string }{
		{"the shell line straight above the box", "> claude\n" + framed},
		{"the shell line, a blank row, the box", "> claude\n\n" + framed},
		{"the box alone", framed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "a live framed dialog is in the way whatever sits above its box:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "no key reaches a dialog af cannot read as rows:\n%s", tt.content)
		})
	}
}

// Unchanged: a box that is no composer (a tool box, a banner, or an edge whose
// box's first row is off screen) does not make quoted options clear. Those
// hold, as they did, because af cannot tell them from a framed dialog's body.
func TestCheckAndHandleTrustPrompt_QuotedOptionsUnderABoxThatIsNoComposerStillHold(t *testing.T) {
	for _, tt := range []struct{ name, box string }{
		{"a tool box", "╭─ Bash ───────────────╮\n│ cat dialog.txt       │\n╰──────────────────────╯\nSecurity guide\n"},
		{"a lone bottom edge", "╰──────────────────────╯"},
		{"a composer whose top edge and prompt scrolled off", "│   and the second row │\n╰──────────────────────╯"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.box + "\n" + quotedFolderOptions
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "%s", strings.TrimSpace(content))
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// staleComposerOverDialog is a composer box whose prompt is in one of the
// composer's modes, left on screen above the whole reworded dialog, wrapped at
// width: the frame an account swap or a respawn leaves when it relaunches
// Claude in the same pane (#7388).
func staleComposerOverDialog(prompt string, width int) string {
	rows := []string{"╭──────────────────────╮", "│ " + prompt + strings.Repeat(" ", max(0, 21-len(prompt))) + "│", "╰──────────────────────╯"}
	for _, row := range []string{
		"Quick safety check: Is this a project you created or one you trust? (Like your own code.)", "",
		"Security guide", "",
		"❯ 1. Yes, I trust this folder", "2. No, exit", "",
		"Enter to confirm · Esc to cancel",
	} {
		if row == "" {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, wrapAtSpaces(row, width)...)
	}
	return strings.Join(rows, "\n")
}

// A composer explains only its own rows (#7388). One with the dialog's own
// first rows on screen below it (its frame's top edge, or its question) is a
// stale frame above a live dialog: an account swap or a respawn relaunches
// Claude in the same pane, below the old composer. The dialog holds, whatever
// mode the stale composer was in. Below 46 columns the question does not fit a
// row, so af cannot answer the dialog and sends no key.
func TestCheckAndHandleTrustPrompt_LiveDialogUnderAStaleComposerBoxHolds(t *testing.T) {
	framed := "╭───────────╮\n│ Security  │\n│ guide     │\n│           │\n│ ❯ 1. Yes, I │\n│ trust this │\n│ folder    │\n│   2. No,  │\n│ exit      │\n╰───────────╯\nEnter to\nconfirm · Esc\nto cancel"
	for _, prompt := range []string{"> /exit", "! ls", "# note"} {
		stale := "╭──────────────────────╮\n│ " + prompt + strings.Repeat(" ", 21-len(prompt)) + "│\n╰──────────────────────╯\n"
		// The stale composer's closing edge can be gone too, overwritten by
		// the relaunch, so the dialog's own top edge follows the composer's
		// prompt row directly (#7412 review).
		unclosed := strings.SplitAfterN(stale, "\n", 3)
		cases := map[string]string{
			"its frame's top edge below the composer":                    stale + framed,
			"its frame's top edge below a composer with no closing edge": unclosed[0] + unclosed[1] + framed,
			"its frame's top edge below a composer with a pad row and no closing edge": unclosed[0] + unclosed[1] +
				"│                      │\n" + framed,
		}
		for width := 3; width <= 24; width++ {
			cases[fmt.Sprintf("its question below the composer at %d columns", width)] = staleComposerOverDialog(prompt, width)
		}
		for name, content := range cases {
			t.Run(prompt+", "+name, func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "a live dialog under a stale composer is in the way:\n%s", content)
				require.Empty(t, sentKeystrokes(cmds), "no key reaches a dialog af cannot read as rows:\n%s", content)
			})
		}
	}
}

// A composer box's prompt row is its mode's glyph followed by nothing or by the
// space the composer paints after it, and 2.1.322 paints U+00A0 there
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi). A composer box
// whose prompt is "!", "#" or ">" then U+00A0 is a composer like any other, and
// the quoted options right under it stay clear (#7391 review).
func TestCheckAndHandleTrustPrompt_ComposerPromptAfterANonBreakingSpaceStaysClear(t *testing.T) {
	for _, prompt := range []string{"!\u00a0ls", "#\u00a0note", ">\u00a0npm test", "!\u00a0"} {
		content := boxedComposer(prompt) + quotedFolderOptions
		t.Run(fmt.Sprintf("prompt %q", prompt), func(t *testing.T) {
			handled, cmds := pollStaticPane(t, content, 4)
			require.False(t, handled, "a composer above quoted options is no dialog:\n%s", content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// boxedComposer is a closed composer box whose prompt row is prompt.
func boxedComposer(prompt string) string {
	return "╭──────────────────────╮\n│ " + prompt + strings.Repeat(" ", max(0, 21-len([]rune(prompt)))) + "│\n╰──────────────────────╯\n"
}

// A ruled composer whose prompt follows U+00A0 is a composer too (#7454), and
// it explains only its own rows (#7388): a live framed dialog under it, with
// its heading on screen, holds (#7412 review, R2).
func TestCheckAndHandleTrustPrompt_LiveDialogUnderARuledComposerAfterANonBreakingSpaceHolds(t *testing.T) {
	const rule = "─────────────"
	framed := "╭───────────╮\n│ Security  │\n│ guide     │\n│           │\n│ ❯ 1. Yes, I │\n│ trust this │\n│ folder    │\n│   2. No,  │\n│ exit      │\n╰───────────╯\nEnter to\nconfirm · Esc\nto cancel"
	for _, prompt := range []string{"!\u00a0ls", "#\u00a0note", ">\u00a0x"} {
		for _, gap := range []string{"", "\n"} {
			content := rule + "\n" + prompt + "\n" + rule + "\n" + gap + framed
			t.Run(fmt.Sprintf("prompt %q, gap %q", prompt, gap), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "a live dialog under a ruled composer is in the way:\n%s", content)
				require.Empty(t, sentKeystrokes(cmds))
			})
		}
	}
}

// A composer decides only over rows af can place as quoted: none, or the bare
// options. A row af cannot read between it and the options may be the dialog's
// question, in wording af does not know, so the dialog holds (#7412 review,
// R3). So it does when that wording is in the composer box's own rows below its
// prompt.
func TestCheckAndHandleTrustPrompt_DialogInWordingAfCannotReadUnderAComposerBoxHolds(t *testing.T) {
	body := []string{"Opening a workspace:", "", "/home/u/proj", "", "Before you start: do you want Claude to", "work in this folder?", "", "Security guide", "", "❯ 1. Yes, I trust this folder", "  2. No, exit"}
	framedBy := func(top string) string {
		rows := []string{top}
		for _, row := range body {
			rows = append(rows, "│ "+row+strings.Repeat(" ", max(0, 44-len([]rune(row))))+" │")
		}
		return strings.Join(append(rows, "╰"+strings.Repeat("─", 46)+"╯"), "\n") + "\nEnter to confirm · Esc to cancel"
	}
	edge := strings.Repeat("─", 46)
	dialogs := map[string]string{
		"unframed":                 strings.Join(body, "\n") + "\nEnter to confirm · Esc to cancel",
		"in a box":                 framedBy("╭" + edge + "╮"),
		"in a box with a title":    framedBy("╭─ Trust " + strings.Repeat("─", 38) + "╮"),
		"in a box opened by a tee": framedBy("├" + edge + "┤"),
	}
	for _, prompt := range []string{"! ls", "# note", "!\u00a0ls"} {
		for name, dialog := range dialogs {
			content := boxedComposer(prompt) + dialog
			t.Run(fmt.Sprintf("prompt %q, the dialog %s", prompt, name), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "a dialog af cannot read under a composer is still in the way:\n%s", content)
				require.Empty(t, sentKeystrokes(cmds))
			})
		}
		around := "╭──────────────────────╮\n│ " + prompt + strings.Repeat(" ", max(0, 21-len([]rune(prompt)))) + "│\n"
		for _, row := range body[:9] {
			around += "│ " + row + strings.Repeat(" ", max(0, 40-len([]rune(row)))) + "│\n"
		}
		around += "╰──────────────────────╯\n" + strings.Join(body[9:], "\n") + "\nEnter to confirm · Esc to cancel"
		t.Run(fmt.Sprintf("prompt %q, that wording in the composer box's own rows", prompt), func(t *testing.T) {
			handled, cmds := pollStaticPane(t, around, 4)
			require.True(t, handled, "a dialog af cannot read in a composer box is still in the way:\n%s", around)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// A row that reads as the end of a question af knows is no evidence that a
// composer hides its start. Under a stale composer, a dialog in wording af
// does not know, headed by such a row, holds (#7412 review, R5).
func TestCheckAndHandleTrustPrompt_DialogHeadedLikeTheEndOfAKnownQuestionUnderAComposerHolds(t *testing.T) {
	// The review's frame: a new-wording MCP dialog under a stale bash-mode box.
	review := "╭──────────────────────────────────╮\n│ ! ls                             │\n╰──────────────────────────────────╯\n\nMCP server found in this project:\n\n  Use this MCP server\n  Use this and all future MCP\n  servers in this project\n❯ Continue without using this MCP\nserver\n\nEnter to confirm · Esc to cancel"
	t.Run("the review's MCP heading under a bash-mode box", func(t *testing.T) {
		handled, cmds := pollStaticPane(t, review, 4)
		require.True(t, handled, "a dialog headed like the end of a known question is still in the way:\n%s", review)
		require.Empty(t, sentKeystrokes(cmds))
	})
	options := map[string]string{
		"folder": quotedFolderOptions,
		"legacy": "❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit",
		"MCP":    "  Use this MCP server\n  Use this and all future MCP servers in this project\n❯ Continue without using this MCP server\n\nEnter to confirm · Esc to cancel",
	}
	composers := []string{
		boxedComposer("! ls"),
		boxedComposer("# note"),
		boxedComposer("!\u00a0ls"),
		"╭──────────────────────╮\n│                      │\n│ > npm test           │\n│                      │\n╰──────────────────────╯\n",
	}
	for _, heading := range []string{"MCP server found in this project:", "in this folder?", "workspace:", "one you trust", "project:", "?"} {
		for name, opts := range options {
			t.Run(fmt.Sprintf("%q over the %s options", heading, name), func(t *testing.T) {
				for _, composer := range composers {
					content := composer + "\n" + heading + "\n\n" + opts
					handled, cmds := pollStaticPane(t, content, 4)
					require.True(t, handled, "a dialog headed like the end of a known question is still in the way:\n%s", content)
					require.Empty(t, sentKeystrokes(cmds))
				}
			})
		}
	}
}

// A top-cropped composer, its prompt row straight above its box's own bottom
// edge and its top edge off screen, explains only its own rows too (#7412
// review, R4). Over bare options it is the agent's composer above quoted
// output, and stays clear (#7399). Over a live dialog, framed or not, it is a
// stale frame above a relaunched Claude, and the dialog holds at every width.
func TestCheckAndHandleTrustPrompt_LiveDialogUnderATopCroppedComposerHolds(t *testing.T) {
	const question = "Quick safety check: Is this a project you created or one you trust? (Like your own code, …)."
	dialogs := []struct {
		name string
		rows func(width int) []string
	}{
		{"a framed dialog, footer inside", func(w int) []string { return framedRewordedDialogRows(w, "inside") }},
		{"a framed dialog, footer outside", func(w int) []string { return framedRewordedDialogRows(w, "outside") }},
		{"a framed dialog after a blank row", func(w int) []string { return append([]string{""}, framedRewordedDialogRows(w, "inside")...) }},
		{"an unframed dialog", func(w int) []string {
			return append(append(wrapAtSpaces(question, w), ""), rewordedTrustBodyAndOptionRows(w)...)
		}},
	}
	for _, prompt := range []string{"> /exit", ">", "> x"} {
		cropped := func(width int) string {
			box := max(width, 24)
			return "│ " + prompt + strings.Repeat(" ", box-4-len(prompt)) + " │\n╰" + strings.Repeat("─", box-2) + "╯\n"
		}
		for _, dialog := range dialogs {
			t.Run(fmt.Sprintf("prompt %q, %s", prompt, dialog.name), func(t *testing.T) {
				for width := 8; width <= 40; width++ {
					content := cropped(width) + strings.Join(dialog.rows(width), "\n")
					handled, cmds := pollStaticPane(t, content, 4)
					require.True(t, handled, "%d columns: a live dialog under a top-cropped composer is in the way:\n%s", width, content)
					require.Empty(t, sentKeystrokes(cmds))
				}
			})
		}
		t.Run(fmt.Sprintf("prompt %q, bare options", prompt), func(t *testing.T) {
			for width := 8; width <= 40; width++ {
				content := cropped(width) + quotedFolderOptions
				handled, cmds := pollStaticPane(t, content, 4)
				require.False(t, handled, "%d columns: a top-cropped composer above quoted options is no dialog:\n%s", width, content)
				require.Empty(t, sentKeystrokes(cmds))
			}
		})
	}
}

// boxStyles are the corner, edge and side glyphs of the four box styles a
// composer or a dialog paints: top-left, top-right, bottom-left, bottom-right,
// edge, side.
var boxStyles = map[string][6]string{
	"round":  {"╭", "╮", "╰", "╯", "─", "│"},
	"square": {"┌", "┐", "└", "┘", "─", "│"},
	"heavy":  {"┏", "┓", "┗", "┛", "━", "┃"},
	"double": {"╔", "╗", "╚", "╝", "═", "║"},
}

// styledBox frames rows in a box width columns wide, in box style style. A row
// wider than the box wraps at its spaces, and an empty row is a pad row.
func styledBox(style string, width int, rows ...string) []string {
	g := boxStyles[style]
	inner := width - 4
	box := []string{g[0] + strings.Repeat(g[4], width-2) + g[1]}
	for _, row := range rows {
		pieces := []string{row}
		if len([]rune(row)) > inner {
			pieces = wrapAtSpaces(row, inner)
		}
		for _, piece := range pieces {
			box = append(box, g[5]+" "+piece+strings.Repeat(" ", max(0, inner-len([]rune(piece))))+" "+g[5])
		}
	}
	return append(box, g[2]+strings.Repeat(g[4], width-2)+g[3])
}

// indentedRows wraps each row at width as Claude Code's renderer does, keeping
// the row's indent on each piece.
func indentedRows(width int, rows ...string) []string {
	var out []string
	for _, row := range rows {
		if row == "" {
			out = append(out, "")
			continue
		}
		indent := row[:len(row)-len(strings.TrimLeft(row, " "))]
		for _, piece := range wrapAtSpaces(row, max(1, width-len(indent))) {
			out = append(out, indent+piece)
		}
	}
	return out
}

// dialogHeads are a dialog's own first rows, each with the options and footer
// that follow them.
var dialogHeads = []struct {
	name          string
	head, options []string
}{
	{"the folder question", []string{"Before you start: do you want Claude to work in this folder?"},
		[]string{"❯ 1. Yes, I trust this folder", "  2. No, exit", "", "Enter to confirm · Esc to cancel"}},
	{"the reworded title and question", []string{"Accessing workspace:", "", "/home/u/proj", "", "Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team)."},
		[]string{"❯ 1. Yes, I trust this folder", "  2. No, exit", "", "Enter to confirm · Esc to cancel"}},
	{"the legacy question", []string{"Do you trust the files in this folder?"},
		[]string{"❯ 1. Yes, proceed", "  2. No, exit", "", "Enter to confirm · Esc to exit"}},
	{"the MCP preamble", []string{"New MCP server found in this project: probe"},
		[]string{"  Use this MCP server", "  Use this and all future MCP servers in this project", "❯ Continue without using this MCP server", "", "Enter to confirm · Esc to cancel"}},
}

// A composer explains only the rows below it. The dialog's own question above
// a composer box that sits between it and the options is a row af cannot tell
// from transcript, so the box decides nothing and the dialog holds, in every
// composer mode, padded or not (#7412 review, R7). A composer box with nothing
// above it but blank rows and frame rules still stays clear over quoted options.
func TestCheckAndHandleTrustPrompt_DialogQuestionAboveAComposerBoxHolds(t *testing.T) {
	const question = "Before you start: do you want Claude to\nwork in this folder?\n"
	review := map[string]string{
		"the review's bash-mode box": question +
			"╭──────────────────────────────────────╮\n│ ! ls                                 │\n╰──────────────────────────────────────╯\n" + quotedFolderOptions,
		"the review's padded prompt-mode box": question +
			"╭──────────────────────────────────────╮\n│                                      │\n│ > x                                  │\n│                                      │\n╰──────────────────────────────────────╯\n" + quotedFolderOptions,
	}
	for name, content := range review {
		t.Run(name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "a dialog's question above a composer box is still in the way:\n%s", content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
	composers := map[string][]string{
		"a bash-mode box":            {"! ls"},
		"a memory-mode box":          {"# note"},
		"a bash-mode box after NBSP": {"! ls"},
		"a prompt-mode box":          {"> x"},
		"an empty prompt-mode box":   {">"},
		"a padded prompt-mode box":   {"", "> x", ""},
	}
	for _, dialog := range dialogHeads {
		for name, composer := range composers {
			t.Run(fmt.Sprintf("%s above %s", dialog.name, name), func(t *testing.T) {
				for style := range boxStyles {
					for _, width := range []int{16, 40} {
						for _, gap := range [][]string{nil, {""}} {
							rows := append(indentedRows(width, dialog.head...), gap...)
							rows = append(append(rows, styledBox(style, width, composer...)...), gap...)
							content := strings.Join(append(rows, indentedRows(width, dialog.options...)...), "\n")
							handled, cmds := pollStaticPane(t, content, 4)
							require.True(t, handled, "%s, %d columns: a dialog's question above a composer box is still in the way:\n%s", style, width, content)
							require.Empty(t, sentKeystrokes(cmds))
						}
					}
				}
			})
		}
	}
	for name, composer := range composers {
		t.Run(fmt.Sprintf("%s, only blank rows and frame rules above it", name), func(t *testing.T) {
			for style, g := range boxStyles {
				for _, above := range []string{"\n", g[2] + strings.Repeat(g[4], 22) + g[3] + "\n", strings.Repeat("─", 24) + "\n\n"} {
					content := above + strings.Join(styledBox(style, 24, composer...), "\n") + "\n" + quotedFolderOptions
					handled, cmds := pollStaticPane(t, content, 4)
					require.False(t, handled, "%s: a composer box with only blank rows and frame rules above quoted options is no dialog:\n%s", style, content)
					require.Empty(t, sentKeystrokes(cmds))
				}
			}
		})
	}
}

// A box whose last row is a prompt, straight above its bottom edge, is a
// top-cropped composer only when its top is off screen: every line above its
// prompt row is one of the box's own pad rows. With the box's top edge, its
// first row or the dialog's question on screen above that row, the box is no
// top-cropped composer and decides nothing, so the dialog holds (#7412 review,
// R6). That covers a whole composer box whose own rows hold the question, and
// a box of the dialog's own rows that ends in a "> " row. #7399's top-cropped
// composer, with nothing above its prompt row but its own pad rows, stays
// clear.
func TestCheckAndHandleTrustPrompt_BoxEndingInAPromptRowWithItsTopOnScreenHolds(t *testing.T) {
	review := "╭──────────────────────────────────────╮\n│ ! ls                                 │\n│ Before you start: do you want Claude │\n│ to work in this folder?              │\n│ > x                                  │\n╰──────────────────────────────────────╯\n" + quotedFolderOptions
	t.Run("the review's bash-mode box holding the question", func(t *testing.T) {
		handled, cmds := pollStaticPane(t, review, 4)
		require.True(t, handled, "a dialog's question inside a box that ends in a prompt row is still in the way:\n%s", review)
		require.Empty(t, sentKeystrokes(cmds))
	})
	without := strings.Replace(review, "│ ! ls                                 │\n", "", 1)
	t.Run("the review's box without its bash-mode row", func(t *testing.T) {
		handled, cmds := pollStaticPane(t, without, 4)
		require.True(t, handled, "a dialog's question inside a box that ends in a prompt row is still in the way:\n%s", without)
		require.Empty(t, sentKeystrokes(cmds))
	})
	shapes := map[string]func(style string, width int, head []string, prompt string) []string{
		"a bash-mode box whose own rows hold the head": func(style string, width int, head []string, prompt string) []string {
			return styledBox(style, width, append(append([]string{"! ls"}, head...), prompt)...)
		},
		"the dialog's own box ending in the prompt row": func(style string, width int, head []string, prompt string) []string {
			return styledBox(style, width, append(append([]string{}, head...), prompt)...)
		},
		"the dialog's own box, its top edge cropped": func(style string, width int, head []string, prompt string) []string {
			return styledBox(style, width, append(append([]string{}, head...), prompt)...)[1:]
		},
		"the head unframed above a bordered prompt row": func(style string, width int, head []string, prompt string) []string {
			return append(indentedRows(width, head...), styledBox(style, width, prompt)[1:]...)
		},
		"the head unframed above a bare prompt row": func(style string, width int, head []string, prompt string) []string {
			box := styledBox(style, width, prompt)
			return append(indentedRows(width, head...), prompt, box[len(box)-1])
		},
	}
	for _, dialog := range dialogHeads {
		for name, shape := range shapes {
			t.Run(fmt.Sprintf("%s, %s", dialog.name, name), func(t *testing.T) {
				for style := range boxStyles {
					for _, prompt := range []string{"> x", ">", "> /home/u/proj"} {
						for _, width := range []int{16, 40} {
							for _, gap := range [][]string{nil, {""}} {
								rows := append(shape(style, width, dialog.head, prompt), gap...)
								content := strings.Join(append(rows, indentedRows(width, dialog.options...)...), "\n")
								handled, cmds := pollStaticPane(t, content, 4)
								require.True(t, handled, "%s, %q, %d columns: a dialog's own row above the prompt row is still in the way:\n%s", style, prompt, width, content)
								require.Empty(t, sentKeystrokes(cmds))
							}
						}
					}
				}
			})
		}
	}
	t.Run("a top-cropped composer, only its own pad rows above its prompt", func(t *testing.T) {
		for style := range boxStyles {
			for _, pads := range [][]string{nil, {""}, {"", ""}} {
				box := styledBox(style, 24, append(append([]string{}, pads...), "> x")...)
				content := strings.Join(box[1:], "\n") + "\n" + quotedFolderOptions
				handled, cmds := pollStaticPane(t, content, 4)
				require.False(t, handled, "%s: a top-cropped composer above quoted options is no dialog (#7399):\n%s", style, content)
				require.Empty(t, sentKeystrokes(cmds))
			}
		}
	})
}

// An edge between a composer box and the options that carries no corner, a
// tee, a junction or a rule in a style other than the composer's ─, neither
// opens nor closes a box. It is an edge inside the dialog's frame or the
// dialog's own rule, so the composer decides nothing over it and the dialog
// holds (Codex review on #7412). The same composer straight over the dialog's
// bordered options, with no edge between, stays clear.
func TestCheckAndHandleTrustPrompt_DialogEdgeBetweenAComposerBoxAndTheOptionsHolds(t *testing.T) {
	// Codex's frame: a stale bash-mode box over a tee-edged dialog.
	codex := boxedComposer("! ls") + "├──────────────────────┤\n│ ❯ 1. Yes, I trust    │\n│ this folder          │\n│   2. No, exit        │\n╰──────────────────────╯\nEnter to confirm · Esc to cancel"
	t.Run("Codex's tee under a bash-mode box", func(t *testing.T) {
		handled, cmds := pollStaticPane(t, codex, 4)
		require.True(t, handled, "a dialog's own edge under a composer box is still in the way:\n%s", codex)
		require.Empty(t, sentKeystrokes(cmds))
	})
	edges := []struct{ name, edge, side, bottom string }{
		{"a tee", "├" + strings.Repeat("─", 22) + "┤", "│", "╰" + strings.Repeat("─", 22) + "╯"},
		{"a double tee", "╠" + strings.Repeat("═", 22) + "╣", "║", "╚" + strings.Repeat("═", 22) + "╝"},
		{"a heavy rule", strings.Repeat("━", 24), "", ""},
		{"a double rule", strings.Repeat("═", 24), "", ""},
		{"a dashed rule", strings.Repeat("┄", 24), "", ""},
		{"a dotted rule", strings.Repeat("┈", 24), "", ""},
		{"a junction row", strings.Repeat("─", 11) + "┬" + strings.Repeat("─", 12), "", ""},
	}
	composers := map[string]string{
		"a bash-mode box":            boxedComposer("! ls"),
		"a memory-mode box":          boxedComposer("# note"),
		"a bash-mode box after NBSP": boxedComposer("! ls"),
		"a prompt-mode box":          boxedComposer("> x"),
		"a padded prompt-mode box":   "╭──────────────────────╮\n│                      │\n│ > npm test           │\n│                      │\n╰──────────────────────╯\n",
	}
	framed := func(side, bottom string, options []string, footerInside bool) []string {
		var rows []string
		for _, row := range indentedRows(20, options[:len(options)-2]...) {
			rows = append(rows, side+" "+row+strings.Repeat(" ", max(0, 20-len([]rune(row))))+" "+side)
		}
		footer := options[len(options)-1]
		if footerInside {
			for _, row := range append([]string{""}, wrapAtSpaces(footer, 20)...) {
				rows = append(rows, side+" "+row+strings.Repeat(" ", max(0, 20-len([]rune(row))))+" "+side)
			}
			return append(rows, bottom)
		}
		return append(append(rows, bottom), wrapAtSpaces(footer, 24)...)
	}
	for name, composer := range composers {
		for _, edge := range edges {
			t.Run(fmt.Sprintf("%s over %s", name, edge.name), func(t *testing.T) {
				for _, dialog := range dialogHeads {
					var bodies [][]string
					if edge.side == "" {
						bodies = [][]string{indentedRows(24, dialog.options...)}
					} else {
						bodies = [][]string{framed(edge.side, edge.bottom, dialog.options, true), framed(edge.side, edge.bottom, dialog.options, false)}
					}
					for _, body := range bodies {
						for _, gap := range []string{"", "\n"} {
							content := composer + gap + edge.edge + "\n" + strings.Join(body, "\n")
							handled, cmds := pollStaticPane(t, content, 4)
							require.True(t, handled, "a dialog's own edge under a composer box is still in the way:\n%s", content)
							require.Empty(t, sentKeystrokes(cmds))
						}
					}
				}
			})
		}
	}
	bare := strings.Replace(codex, "├──────────────────────┤\n", "", 1)
	t.Run("the bash-mode box straight over the bordered options", func(t *testing.T) {
		handled, cmds := pollStaticPane(t, bare, 4)
		require.False(t, handled, "a composer straight over quoted options is no dialog:\n%s", bare)
		require.Empty(t, sentKeystrokes(cmds))
	})
}
