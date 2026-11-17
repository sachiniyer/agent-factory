package tmux

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7453. A dialog's head, then an empty 2.1.322 composer
// straight above the dialog's options: its ❯ alone between its plain rules.
// The ❯ has no text, so the option block read it as its own first row, and the
// composer's closing rule as a composer below the options. The pane read
// clear. With a draft in the composer, the same pane held (#7412 round-3
// review, R7). A clear pane lets the create type its prompt and press Enter,
// which confirms the dialog's preselected row.

// issue7453Frame is the issue's frame. The composer's prompt row is its ❯ and
// the U+00A0 2.1.322 paints after it.
const issue7453Frame = "Before you start: do you want Claude to\nwork in this folder?\n" +
	"────────────────────────────────────────\n❯ \n────────────────────────────────────────\n" +
	"❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel"

// emptyComposerPrompts are an empty composer's prompt row as a pane shows it:
// its ❯, then the U+00A0 2.1.322 paints, a space, or nothing.
var emptyComposerPrompts = []string{"❯ ", "❯ ", "❯"}

// emptyComposerForms are the composer's rows straight above the options: its
// prompt row between its plain rules, or under its opening rule or over its
// closing rule alone.
var emptyComposerForms = map[string]func(width int, prompt string) []string{
	"ruled":             ruledComposers["ruled"],
	"opening rule only": ruledComposers["opening rule only"],
	"closing rule only": func(w int, prompt string) []string { return []string{prompt, strings.Repeat("─", w)} },
}

// dialogOptionSets are the three option blocks dialogHeads carry, with their
// footers: the reworded folder dialog's, the legacy one's, and the MCP server
// dialog's.
var dialogOptionSets = [][]string{dialogHeads[0].options, dialogHeads[2].options, dialogHeads[3].options}

// Every dialog head, over an empty composer in each of its forms, straight
// above each dialog's options, at each width, with a blank row under the head
// or none: the issue's frame and its 120-frame acceptance set, for each prompt
// row and composer form.
func TestCheckAndHandleTrustPrompt_Issue7453EmptyComposerOverTheOptionsUnderADialogsHeadHolds(t *testing.T) {
	requireHeldWithoutAKey(t, issue7453Frame, "the issue's frame")
	for _, dialog := range dialogHeads {
		for form, composer := range emptyComposerForms {
			t.Run(fmt.Sprintf("%s over an empty composer, %s", dialog.name, form), func(t *testing.T) {
				for _, options := range dialogOptionSets {
					for _, width := range []int{12, 16, 24, 40, 60} {
						for _, gap := range [][]string{nil, {""}} {
							for _, prompt := range emptyComposerPrompts {
								rows := append(indentedRows(width, dialog.head...), gap...)
								rows = append(append(rows, composer(width, prompt)...), indentedRows(width, options...)...)
								requireHeldWithoutAKey(t, strings.Join(rows, "\n"), fmt.Sprintf(
									"%d columns, prompt %q: an empty composer between a dialog's head and its options explains neither", width, prompt))
							}
						}
					}
				}
			})
		}
	}
}

// The same panes caught while Claude Code paints the footer: the options over
// the footer's first words (#7390).
func TestCheckAndHandleTrustPrompt_Issue7453EmptyComposerOverTheOptionsAndTheirFooterStartHolds(t *testing.T) {
	for _, dialog := range dialogHeads {
		for _, options := range dialogOptionSets {
			for _, width := range []int{16, 40} {
				rows := indentedRows(width, dialog.head...)
				rows = append(append(rows, ruledComposers["ruled"](width, "❯ ")...), indentedRows(width, options[:len(options)-1]...)...)
				rows = append(rows, "Enter to confirm ·")
				requireHeldWithoutAKey(t, strings.Join(rows, "\n"), fmt.Sprintf(
					"%s at %d columns: the options over the footer's first words, under an empty composer", dialog.name, width))
			}
		}
	}
}

// The hold says why, once, and names the dialog the options are.
func TestCheckAndHandleTrustPrompt_Issue7453RefusalNamesTheDialog(t *testing.T) {
	mcp := strings.Join(slices.Concat(dialogHeads[3].head, ruledComposers["ruled"](40, "❯ "), dialogHeads[3].options), "\n")
	for _, tt := range []struct{ name, content, dialog string }{
		{"the issue's frame", issue7453Frame, claudeFolderTrustDialogName},
		{"the MCP server dialog's", mcp, claudeMCPServerDialogName},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
			logged := errLog.String()
			require.Equal(t, 1, strings.Count(logged, "refusing to answer"), "the hold is reported once, not once per poll; got:\n%s", logged)
			require.Contains(t, logged, "refusing to answer the "+tt.dialog+" dialog")
			require.Contains(t, logged, "its option rows and its footer under rows af cannot read as the dialog's question")
		})
	}
}

// An empty composer still decides where it explains the screen, and a pane
// that is no dialog stays clear: the composer straight over quoted options
// with nothing above it, and the issue's frame with a working agent's composer
// or any row under its footer, or a plain rule under its options.
func TestCheckAndHandleTrustPrompt_Issue7453EmptyComposerThatExplainsThePaneStaysClear(t *testing.T) {
	type frame struct{ name, content string }
	var frames []frame
	for _, prompt := range emptyComposerPrompts {
		for _, options := range dialogOptionSets {
			composer := strings.Join(ruledComposers["ruled"](40, prompt), "\n")
			frames = append(frames,
				frame{fmt.Sprintf("an empty composer %q straight over quoted options", prompt), composer + "\n" + strings.Join(options, "\n")},
				frame{fmt.Sprintf("an empty composer %q under blank rows, over quoted options", prompt), "\n\n" + composer + "\n" + strings.Join(options, "\n")})
		}
	}
	frames = append(frames,
		frame{"the issue's frame above a working agent's ruled composer", issue7453Frame + ruledComposerBelow},
		frame{"the issue's frame above a working agent's boxed composer", issue7453Frame + boxedComposerBelow},
		frame{"the issue's frame with a row under its footer", issue7453Frame + "\n  ? for shortcuts"},
		frame{"the issue's frame with a plain rule under its options", strings.Replace(issue7453Frame, "No, exit\n\n", "No, exit\n"+strings.Repeat("─", 40)+"\n", 1)},
	)
	for _, tt := range frames {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}
