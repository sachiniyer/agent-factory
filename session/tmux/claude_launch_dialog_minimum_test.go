package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// Regression tests for #7350. The folder-dialog matcher held on "Do you", two
// common words, wherever they ended a pane, and on a composer draft whose last
// row began the trust question. Neither pane shows a dialog, and a hold spends
// the create's trust budget on it.

// pollStaticPaneAtWidth is pollStaticPane for a pane tmux reports as width
// columns wide.
func pollStaticPaneAtWidth(t *testing.T, content string, width, polls int) (handled bool, cmds []string) {
	t.Helper()
	advance := setClaudeTrustClock(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			cmds = append(cmds, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "#{pane_width}"):
				return []byte(fmt.Sprintf("%d\n", width)), nil
			case strings.Contains(joined, "display-message"):
				return []byte("0 0 0"), nil
			}
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("width", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
	for i := 0; i < polls; i++ {
		advance(time.Second)
		handled = session.CheckAndHandleTrustPrompt()
	}
	return handled, cmds
}

// Fewer words than "Do you trust" are the question being painted only where
// the pane's width broke them. On a pane with room for the next word they are
// the end of a sentence.
func TestCheckAndHandleTrustPrompt_TwoCommonWordsAreNotATrustQuestion(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		width         int
	}{
		{"Do you alone on an 80-column pane", "Do you", 80},
		{"a sentence whose last row is Do you", "● Before I go on I have a question.\n  Do you", 80},
		{"Do and you on two rows", "● Here is a wider row of output.\nDo\nyou", 80},
		{"a numbered row", "1. Is this folder one of yours?\n2. Do you", 80},
		{"Do you on a pane just wide enough for the next word", "Do you", 12},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPaneAtWidth(t, tt.content, tt.width, 4)
			require.False(t, handled, "no dialog is on this %d-column pane; a hold spends the create's trust budget on it (#7350):\n%s", tt.width, tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// The later rows of a composer's draft are the composer's, whatever the pane's
// width and however much of the question they spell.
func TestCheckAndHandleTrustPrompt_ComposerDraftIsNotATrustQuestion(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a ruled composer draft ending Do you", composerRule + "\n❯ Explain this:\n  Do you\n" + composerRule + "\n"},
		{"a ruled composer draft ending Do you trust", composerRule + "\n❯ Explain this:\n  Do you trust\n" + composerRule + "\n"},
		{"a ruled composer draft ending Do you trust the files", composerRule + "\n❯ Explain this:\n  Do you trust the files\n" + composerRule + "\n"},
		{"a boxed composer draft ending Do you trust", "╭────────────────╮\n│ > Explain this: │\n│   Do you trust  │\n╰────────────────╯\n"},
		{"a boxed composer draft holding the wrapped question", "╭──────────────────╮\n│ > Explain this:   │\n│   Do you trust the │\n│   files in this    │\n│   folder?          │\n╰──────────────────╯\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// No width is reported: the draft is the composer's at any width.
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "no dialog is on this pane; a hold spends the create's trust budget on it (#7350):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// Text shorter than "Do you" was never a hold, and stays clear.
func TestCheckAndHandleTrustPrompt_LessThanDoYouStaysClear(t *testing.T) {
	for _, content := range []string{"Do yo", "Do", "● Before I go on I have a question.\n  Do yo"} {
		for _, width := range []int{4, 80} {
			handled, cmds := pollStaticPaneAtWidth(t, content, width, 4)
			require.False(t, handled, "%q at %d columns", content, width)
			require.Empty(t, sentKeystrokes(cmds))
		}
	}
}

// A real trust question being painted still holds: from "Do you trust" on the
// words alone, and from "Do you" on a pane narrow enough to have broken it.
func TestCheckAndHandleTrustPrompt_TrustQuestionBeingPaintedStillHolds(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		width         int
	}{
		{"Do you trust, on an 80-column pane", "Do you trust", 80},
		{"Do you trust under a wider row", "● A question for you:\n  Do you trust", 80},
		{"Do you trust the, under a wider row", "1. Is it yours?\n2. Do you trust the", 80},
		{"Do you on a 6-column pane", "Do you", 6},
		{"Do you on an 11-column pane, one short of the next word", "Do you", 11},
		{"Do you / trust on a 6-column pane", "Do you\ntrust", 6},
		{"Do / you on a 5-column pane", "Do\nyou", 5},
		{"the MCP modal's rows above Do you, 7 columns", "New MCP\nserver\nfound.\nDo you", 7},
		// After a resize the rows an earlier agent painted are still on the
		// pane, wider than the pane is now. They are no evidence of its width.
		{"a wide stale composer above Do you on a 6-column pane", composerRule + "\n❯ /exit\n" + composerRule + "\nDo you", 6},
		{"a wide stale sentence above Do you on a 6-column pane", "● Before I go on I have a question.\nDo you", 6},
		{"Do you trust under a prompt row, not indented, a rule below", composerRule + "\n❯ run me\nDo you trust\n" + composerRule, 80},
		{"a draft with no closing rule", composerRule + "\n❯ Explain this:\n  Do you trust", 80},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPaneAtWidth(t, tt.content, tt.width, 4)
			require.True(t, handled, "a trust question is being painted on this %d-column pane:\n%s", tt.width, tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a hold sends no key; got %v", cmds)
		})
	}
}

// A pane whose width af could not read holds on Do you: an unread width is no
// evidence of a wide pane.
func TestCheckAndHandleTrustPrompt_DoYouHoldsWhenThePaneWidthIsUnread(t *testing.T) {
	handled, cmds := pollStaticPane(t, "● Before I go on I have a question.\n  Do you", 4)
	require.True(t, handled)
	require.Empty(t, sentKeystrokes(cmds))
}

// The legacy question's first rows, as a pane of each width under the
// question's paints them, still hold at that width.
func TestCheckAndHandleTrustPrompt_LegacyQuestionFirstRowsHoldAtEveryNarrowWidth(t *testing.T) {
	question := "Do you trust the files in this folder?"
	for width := 6; width < len(question); width++ {
		rows := wrapAtSpaces(question, width)
		for painted := 1; painted < len(rows); painted++ {
			content := strings.Join(rows[:painted], "\n")
			handled, cmds := pollStaticPaneAtWidth(t, content, width, 2)
			require.True(t, handled, "%d columns, %d rows of the question painted read clear:\n%s", width, painted, content)
			require.Empty(t, sentKeystrokes(cmds))
		}
	}
}
