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
		cases := map[string]string{"its frame's top edge below the composer": stale + framed}
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

// A composer's prompt row is its mode's glyph followed by nothing or by the
// space the composer paints after it, and 2.1.322 paints U+00A0 there
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi). A ruled or boxed
// composer whose prompt is "!", "#" or ">" then U+00A0 is a composer like any
// other, and the quoted options under it stay clear (#7391 review).
func TestCheckAndHandleTrustPrompt_ComposerPromptAfterANonBreakingSpaceStaysClear(t *testing.T) {
	const rule = "────────────"
	for _, prompt := range []string{"! ls", "# note", "> npm test", "! "} {
		for _, tt := range []struct{ name, content string }{
			{"ruled", rule + "\n" + prompt + "\n" + rule + "\nsome output\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to\nexit"},
			{"boxed", "╭──────────────────────╮\n│ " + prompt + strings.Repeat(" ", max(0, 21-len([]rune(prompt)))) + "│\n╰──────────────────────╯\n" + quotedFolderOptions},
		} {
			t.Run(fmt.Sprintf("%s composer, prompt %q", tt.name, prompt), func(t *testing.T) {
				handled, cmds := pollStaticPane(t, tt.content, 4)
				require.False(t, handled, "a composer above quoted options is no dialog:\n%s", tt.content)
				require.Empty(t, sentKeystrokes(cmds))
			})
		}
	}
}
