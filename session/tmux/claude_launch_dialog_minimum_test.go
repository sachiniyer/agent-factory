package tmux

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7350's composer-draft class. The folder-dialog matcher
// held on a closed composer whose draft's last rows began the trust question.
// That pane shows no dialog, and the hold spends a create's trust budget.

// The later rows of a composer's draft are the composer's, however much of the
// question they spell.
func TestCheckAndHandleTrustPrompt_ComposerDraftIsNotATrustQuestion(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a ruled composer draft ending Do you", composerRule + "\n❯ Explain this:\n  Do you\n" + composerRule + "\n"},
		{"a ruled composer draft ending Do you trust", composerRule + "\n❯ Explain this:\n  Do you trust\n" + composerRule + "\n"},
		{"a ruled composer draft ending Do you trust the files", composerRule + "\n❯ Explain this:\n  Do you trust the files\n" + composerRule + "\n"},
		{"a boxed composer draft ending Do you trust", "╭────────────────╮\n│ > Explain this: │\n│   Do you trust  │\n╰────────────────╯\n"},
		{"a boxed composer draft holding the wrapped question", "╭──────────────────╮\n│ > Explain this:   │\n│   Do you trust the │\n│   files in this    │\n│   folder?          │\n╰──────────────────╯\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "no dialog is on this pane; a hold spends the create's trust budget on it (#7350):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// "Do you" as the pane's last content still holds, with or without wider rows
// above it. On a narrow pane it is the whole first row of a real trust
// question, and Claude Code wraps this dialog short of the pane's last column
// by a margin af does not know, so no row on the pane and no pane width shows
// that the words are not a wrap (#7351 review).
func TestCheckAndHandleTrustPrompt_DoYouAsTheLastRowStillHolds(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"Do you alone", "Do you"},
		{"Do you with the dialog's own indent", "  Do you"},
		{"a 14-column rule above Do you", "──────────────\n  Do you"},
		{"a 15-column rule above Do you", "───────────────\n  Do you"},
		{"Do and you on two rows", "  Do\n  you"},
		{"Do you / trust", "Do you\ntrust"},
		{"a sentence whose last row is Do you", "● Before I go on I have a question.\n  Do you"},
		{"the MCP modal's rows above Do you", "New MCP\nserver\nfound.\nDo you"},
		{"a stale composer above Do you", composerRule + "\n❯ /exit\n" + composerRule + "\nDo you"},
		{"Do you trust under a prompt row, not indented, a rule below", composerRule + "\n❯ run me\nDo you trust\n" + composerRule},
		{"a draft with no closing rule", composerRule + "\n❯ Explain this:\n  Do you trust"},
		{"a draft with no opening rule", "❯ Explain this:\n  Do you trust\n" + composerRule},
		{"an empty prompt above the rows", composerRule + "\n❯ \n  Do you trust\n" + composerRule},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "a trust question may be being painted:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a hold sends no key; got %v", cmds)
		})
	}
}

// Text shorter than "Do you" was never a hold, and stays clear.
func TestCheckAndHandleTrustPrompt_LessThanDoYouStaysClear(t *testing.T) {
	for _, content := range []string{"Do yo", "Do", "● Before I go on I have a question.\n  Do yo"} {
		handled, cmds := pollStaticPane(t, content, 4)
		require.False(t, handled, "%q", content)
		require.Empty(t, sentKeystrokes(cmds))
	}
}

// The legacy question's first rows, as a pane of each width under the
// question's paints them, hold: bare, and with the 2-column indent Claude Code
// 2.1.322 gives a dialog's rows.
func TestCheckAndHandleTrustPrompt_LegacyQuestionFirstRowsHoldAtEveryNarrowWidth(t *testing.T) {
	question := "Do you trust the files in this folder?"
	for width := 6; width < len(question); width++ {
		rows := wrapAtSpaces(question, width)
		for painted := 1; painted < len(rows); painted++ {
			for _, indent := range []string{"", "  "} {
				content := indent + strings.Join(rows[:painted], "\n"+indent)
				handled, cmds := pollStaticPane(t, content, 2)
				require.True(t, handled, "%d columns, %d rows of the question painted read clear:\n%s", width, painted, content)
				require.Empty(t, sentKeystrokes(cmds))
			}
		}
	}
}
