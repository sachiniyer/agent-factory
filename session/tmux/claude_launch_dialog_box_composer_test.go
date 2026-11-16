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

// U+00A0 reaches only the box test. #7391's plain-rule test keeps its ASCII
// prompt, because it has no #7388 anchor yet: a live framed dialog under a
// ruled composer whose prompt follows U+00A0 holds, as on master (#7412
// review, R2).
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

// A composer decides only over rows af can place as quoted: none, the bare
// options, or the end of one of the dialog's own first rows it hides. A row af
// cannot read between it and the options may be the dialog's question, in
// wording af does not know, so the dialog holds (#7412 review, R3). So it does
// when that wording is in the composer box's own rows below its prompt.
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
