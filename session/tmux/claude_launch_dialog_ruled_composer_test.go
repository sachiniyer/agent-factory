package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7388. A composer decides about a dialog only by its own
// rows, and only when it explains every row on screen outside it: nothing af
// cannot place between it and the options, and nothing above it. PR 3 (#7412)
// gave boxes that rule. These pin it for the composer's other forms: a ❯ row,
// the 2.1.322 ruled composer, a prompt row bounded by a plain rule, and a stale
// composer above the multi-server picker.

// newWordingDialog is a folder-trust dialog in wording af does not know, so only
// the re-wrap tail reads it, and a pane showing it must hold.
var newWordingDialog = []string{
	"Opening a workspace:", "", "/home/u/proj", "",
	"Before you start: do you want Claude to work in this folder?", "",
	"Security guide", "",
	"❯ 1. Yes, I trust this folder", "  2. No, exit", "",
	"Enter to confirm · Esc to cancel",
}

// rewordedDialog is today's reworded folder-trust dialog.
var rewordedDialog = []string{
	"Accessing workspace:", "", "/home/u/proj", "",
	"Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.", "",
	"Security guide", "",
	"❯ 1. Yes, I trust this folder", "  2. No, exit", "",
	"Enter to confirm · Esc to cancel",
}

// ruleTopDialog is dialog wrapped at width under its own top rule, as Claude
// Code 2.1.322 paints its launch dialogs.
func ruleTopDialog(width int, dialog []string) []string {
	return append([]string{strings.Repeat("─", width)}, indentedRows(width, dialog...)...)
}

// requireHeldWithoutAKey asserts the pane holds: a live dialog is in the way and
// af sends it no key.
func requireHeldWithoutAKey(t *testing.T, content, why string) {
	t.Helper()
	handled, cmds := pollStaticPane(t, content, 4)
	require.True(t, handled, "%s:\n%s", why, content)
	require.Empty(t, sentKeystrokes(cmds), "%s:\n%s", why, content)
}

// The frames #7388 pins in both directions. c7 and s3 hold: a stale composer
// box, or a shell's ❯ prompt line, above a live framed dialog. c1, c2, c10 and
// #7391's ruled composers over bare quoted options (n1s, n3) stay clear, and
// c12 holds, as PR 3 left them. h2 and h3, a shell's ">" or "#" prompt line
// echoed straight above the dialog's own top rule, hold.
func TestCheckAndHandleTrustPrompt_Issue7388PinnedFramesKeepTheirVerdicts(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		hold          bool
	}{
		{"c7: a stale composer box above a live framed dialog", "╭──────────────────────╮\n│ > /exit              │\n╰──────────────────────╯\n╭──────────────────────╮\n│ Security guide       │\n│                      │\n│ ❯ 1. Yes, I trust    │\n│ this folder          │\n│   2. No, exit        │\n╰──────────────────────╯\nEnter to confirm · Esc\nto cancel", true},
		{"s3: a shell's ❯ prompt line above a live framed dialog", "❯ claude\n╭───────────╮\n│ Security  │\n│ guide     │\n│           │\n│ ❯ 1. Yes, I │\n│ trust this │\n│ folder    │\n│   2. No,  │\n│ exit      │\n╰───────────╯\nEnter to\nconfirm · Esc\nto cancel", true},
		{"h2: a shell's > prompt line above the dialog's own top rule", "> claude\n────────────\n  Accessing workspace:\n\n  /home/u/proj\n\n  ❯ 1. Yes, I trust this folder\n    2. No, exit\n\n  Enter to confirm · Esc to cancel", true},
		{"h3: a shell's # prompt line above the dialog's own top rule", "# claude\n────────────\n  Accessing workspace:\n\n  /home/u/proj\n\n  ❯ 1. Yes, I trust this folder\n    2. No, exit\n\n  Enter to confirm · Esc to cancel", true},
		{"c12: a box draft holding a body row", "╭──────────────────────╮\n│                      │\n│ > paste:             │\n│ Security guide       │\n│ ❯ 1. Yes, I trust    │\n│ this folder          │\n│   2. No, exit        │\n│ Enter to confirm ·   │\n│ Esc to cancel        │\n╰──────────────────────╯", true},
		{"c1: a composer box above quoted options", "╭──────────────────────╮\n│ > Type your message  │\n╰──────────────────────╯\n❯ 1. Yes, I trust this folder\n2. No, exit\n\nEnter to confirm · Esc to cancel", false},
		{"c2: a bash-mode composer box above quoted options", "╭──────────────────────╮\n│ ! ls                 │\n╰──────────────────────╯\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel", false},
		{"c10: a composer box drawn around quoted options", "╭──────────────────────╮\n│ > paste:             │\n│ ❯ 1. Yes, I trust    │\n│ this folder          │\n│   2. No, exit        │\n│ Enter to confirm ·   │\n│ Esc to cancel        │\n╰──────────────────────╯", false},
		{"n1s: a ruled bash-mode composer above quoted options", "────────────\n! ls\n────────────\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel", false},
		{"n3: the 2.1.322 ruled composer with a draft above quoted options", "────────────\n❯\u00a0fix it\n────────────\n❯ 1. Yes, I trust this folder\n  2. No, exit\n\nEnter to confirm · Esc to cancel", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.Equal(t, tt.hold, handled, "%s:\n%s", tt.name, tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// A prompt-shaped row straight above a launch dialog's own top rule never
// clears the dialog under it (#7399 review, case 1). The rule is the dialog's,
// so its rows below decide, and the prompt row explains none of them. So it is
// with the prompt row inside an unclosed composer box, whose rule under it is
// no edge of the box (#7412 round-2 review), in every composer mode.
func TestCheckAndHandleTrustPrompt_PromptRowAboveTheDialogsOwnTopRuleHolds(t *testing.T) {
	// The review's frames: today's reworded dialog at 9 and 13 columns.
	for _, prompt := range []string{">", "> please refactor the parser", "# comment"} {
		for _, width := range []int{9, 13} {
			t.Run(fmt.Sprintf("the review's %q at %d columns", prompt, width), func(t *testing.T) {
				content := prompt + "\n" + strings.Join(ruleTopDialog(width, rewordedDialog), "\n")
				requireHeldWithoutAKey(t, content, "a prompt row above the dialog's own top rule decides nothing")
			})
		}
	}
	for _, prompt := range []string{">", "> x", "> please refactor the parser", "# comment", "! ls", "!\u2007", "❯ claude", "hello world"} {
		t.Run(fmt.Sprintf("%q above a new-wording dialog's own top rule", prompt), func(t *testing.T) {
			for width := 3; width <= 80; width++ {
				for _, gap := range []string{"\n", "\n\n"} {
					content := prompt + gap + strings.Join(ruleTopDialog(width, newWordingDialog), "\n")
					requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: a prompt row above the dialog's own top rule decides nothing", width))
				}
			}
		})
	}
	for _, prompt := range []string{"! ls", "# note", "!\u00a0ls", "!\u2007", "> x", ">", "!"} {
		t.Run(fmt.Sprintf("an unclosed composer box's %q above a new-wording dialog's own top rule", prompt), func(t *testing.T) {
			for _, width := range []int{12, 16, 24, 40, 60, 80} {
				box := styledBox("round", width, prompt)
				content := strings.Join(append(box[:2], ruleTopDialog(width, newWordingDialog)...), "\n")
				requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: a box's prompt row above the dialog's own top rule decides nothing", width))
			}
		})
	}
}

// A prompt row between another frame's bottom edge or rule and a live dialog
// is no composer of the dialog's, and the dialog holds (#7399 review, case 2).
func TestCheckAndHandleTrustPrompt_PromptRowUnderAFrameEdgeAboveALiveDialogHolds(t *testing.T) {
	edges := map[string]func(width int) string{
		"a box's bottom edge":    func(w int) string { return "╰" + strings.Repeat("─", w-2) + "╯" },
		"a heavy rule":           func(w int) string { return strings.Repeat("━", w) },
		"a banner's bottom edge": func(w int) string { return "╚" + strings.Repeat("═", w-2) + "╝" },
	}
	for name, edge := range edges {
		for _, prompt := range []string{"> x", ">", "! ls"} {
			t.Run(fmt.Sprintf("%s, then %q", name, prompt), func(t *testing.T) {
				for _, width := range []int{8, 9, 10, 11, 12, 13, 14, 24, 40} {
					for _, dialog := range [][]string{indentedRows(width, newWordingDialog...), ruleTopDialog(width, newWordingDialog)} {
						content := edge(width) + "\n" + prompt + "\n" + strings.Join(dialog, "\n")
						requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: a prompt row under another frame's edge decides nothing", width))
					}
				}
			})
		}
	}
}

// ruledComposers are the composer's ruled forms: its prompt row between plain
// rules, or under its opening rule with its closing rule not painted.
var ruledComposers = map[string]func(width int, prompt string) []string{
	"ruled": func(w int, prompt string) []string {
		return []string{strings.Repeat("─", w), prompt, strings.Repeat("─", w)}
	},
	"opening rule only": func(w int, prompt string) []string {
		return []string{strings.Repeat("─", w), prompt}
	},
}

// A ruled composer, in any mode and the 2.1.322 ❯ form among them, above a live
// dialog explains none of the dialog's rows, and the dialog holds (#7388). Over
// bare quoted options, with nothing between, it still decides, and the pane
// stays clear, as #7391 left it.
func TestCheckAndHandleTrustPrompt_LiveDialogUnderARuledComposerHolds(t *testing.T) {
	for name, composer := range ruledComposers {
		for _, prompt := range []string{"❯ x", "❯\u00a0", "❯ claude", "> x", "! ls", "# note"} {
			t.Run(fmt.Sprintf("%s %q above a live dialog", name, prompt), func(t *testing.T) {
				for _, width := range []int{16, 24, 40, 60} {
					dialogs := [][]string{
						indentedRows(width, newWordingDialog...),
						ruleTopDialog(width, newWordingDialog),
						framedRewordedDialogRows(width, "inside"),
						framedRewordedDialogRows(width, "outside"),
					}
					for _, dialog := range dialogs {
						for _, gap := range [][]string{nil, {""}} {
							rows := append(append(composer(width, prompt), gap...), dialog...)
							content := strings.Join(rows, "\n")
							handled, cmds := pollStaticPane(t, content, 4)
							require.True(t, handled, "%d columns: a live dialog under a ruled composer is in the way:\n%s", width, content)
							require.Empty(t, sentKeystrokes(cmds))
						}
					}
				}
			})
		}
		for _, prompt := range []string{"❯ x", "❯ claude", "> x", "! ls", "# note"} {
			t.Run(fmt.Sprintf("%s %q above quoted options", name, prompt), func(t *testing.T) {
				for _, options := range []string{quotedFolderOptions, "❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit"} {
					content := strings.Join(composer(40, prompt), "\n") + "\n" + options
					handled, cmds := pollStaticPane(t, content, 4)
					require.False(t, handled, "a ruled composer directly over quoted options is no dialog:\n%s", content)
					require.Empty(t, sentKeystrokes(cmds))
				}
			})
		}
	}
}

// A ruled or ❯ composer explains only the rows below it, as a composer box
// does (#7412 round-3 review, R7). A dialog's own head above it, with the
// composer between the head and the options, holds.
func TestCheckAndHandleTrustPrompt_DialogHeadAboveARuledComposerHolds(t *testing.T) {
	composers := map[string]func(width int) []string{
		"a ruled ❯ composer":      func(w int) []string { return ruledComposers["ruled"](w, "❯ x") },
		"a ruled > composer":      func(w int) []string { return ruledComposers["ruled"](w, "> x") },
		"a ruled ! composer":      func(w int) []string { return ruledComposers["ruled"](w, "! ls") },
		"an opening-rule ❯ row":   func(w int) []string { return ruledComposers["opening rule only"](w, "❯ x") },
		"a shell's ❯ prompt line": func(int) []string { return []string{"❯ claude"} },
	}
	for _, dialog := range dialogHeads {
		for name, composer := range composers {
			t.Run(fmt.Sprintf("%s above %s", dialog.name, name), func(t *testing.T) {
				for _, width := range []int{16, 40} {
					for _, gap := range [][]string{nil, {""}} {
						rows := append(indentedRows(width, dialog.head...), gap...)
						rows = append(append(rows, composer(width)...), indentedRows(width, dialog.options...)...)
						content := strings.Join(rows, "\n")
						requireHeldWithoutAKey(t, content, fmt.Sprintf("%d columns: a dialog's head above a composer is still in the way", width))
					}
				}
			})
		}
	}
}

// A stale composer above a live multi-server picker explains none of the
// picker's rows. The pane holds, and af sends no Esc into it: the picker's
// strict grammar still refuses a pane with a ❯ row above its header (#7388).
func TestCheckAndHandleTrustPrompt_StaleComposerAboveALiveMCPServersPickerHolds(t *testing.T) {
	for _, composer := range []string{mcpServerComposerFixture, mcpServersRejectedFixture} {
		for _, picker := range []string{
			mcpServerMultiServerFixture, mcpServersThreeFixture, mcpServersThreeWideFixture,
			mcpServersSixFixture, mcpServersSixScrolledFixture, mcpServers40ColumnsFixture, mcpServersLongNameFixture,
		} {
			t.Run(composer+" above "+picker, func(t *testing.T) {
				content := strings.TrimRight(readTrustFixture(t, composer), "\n") + "\n" + readTrustFixture(t, picker)
				handled, cmds := pollStaticPane(t, content, 4)
				require.True(t, handled, "a live picker under a stale composer is in the way")
				require.Empty(t, sentKeystrokes(cmds), "no key reaches a picker under a stale composer")
			})
		}
	}
}

// A ruled composer explains only its own rows. A row of text between it and
// the options is one af cannot place: it may be the dialog's own body or
// question, in wording af does not know, under a stale composer. So the
// composer decides nothing and the pane holds (#7388, the "between" half of
// its property; #7412 round-1 review R3 gave composer boxes the same rule).
// These frames are the three #7391 pinned as clear under its stay-clear test,
// moved here unchanged; with the "some output" row taken out, the same
// composers straight over the options still pin clear there.
func TestCheckAndHandleTrustPrompt_RuledComposerOverARowItCannotExplainHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a ruled composer over a row of text above the options", "────────────\n> fix it\n────────────\nsome output\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
		{"a ruled bash-mode composer over a row of text above the options", "────────────\n! ls\n────────────\nsome output\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
		{"a ruled memory-mode composer over a row of text above the options", "────────────\n# keep it short\n────────────\nsome output\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireHeldWithoutAKey(t, tt.content, "a ruled composer over a row it cannot explain decides nothing")
		})
	}
}
