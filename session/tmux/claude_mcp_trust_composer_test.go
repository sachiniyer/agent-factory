package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7341. A drawn composer's draft rows, and a status row
// under it that reads "new", held as the old MCP trust modal being painted, so
// a create spent its trust budget on a pane with no dialog. The first fix (921331ddf3)
// read any pane with a composer on it clear, which un-held real modals; every
// frame #7334's round-2 review found that way is pinned here as a hold.

const composerRule = "────────────────"

// A composer excuses the rows it explains: the rows of its own draft, and one
// word on the row straight under it, which is the agent's status row.
func TestCheckAndHandleTrustPrompt_ComposerDraftRowsAreNotTheOldMCPModal(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		// R1: a status row under the composer that reads "new".
		{"a status row that reads new", composerRule + "\n❯ \n" + composerRule + "\n  new\n"},
		{"a status row that reads new, boxed composer", "╭────────────────╮\n│ > Type here     │\n╰────────────────╯\n  new\n"},
		{"a status row that reads NEW", composerRule + "\n❯ \n" + composerRule + "\n  NEW\n"},
		{"a status row that reads new, under a prompt with text", composerRule + "\n❯ /exit\n" + composerRule + "\n  new\n"},
		// R2: the later rows of a draft that spell the modal's start.
		{"a draft's later rows", composerRule + "\n❯ Explain this:\n  New MCP server\n  found. Do you\n" + composerRule + "\n"},
		{"a boxed draft's later rows", "╭────────────────╮\n│ > Explain this: │\n│   New MCP server │\n│   found. Do you  │\n╰────────────────╯\n"},
		{"a draft whose last row is new", composerRule + "\n❯ make the branch\n  new\n" + composerRule + "\n"},
		{"a boxed draft whose last row is new", "╭───────────────────╮\n│ > make the branch │\n│   new             │\n╰───────────────────╯\n"},
		{"a draft holding the whole preamble row", composerRule + "\n❯ Explain this:\n  New MCP server found.\n" + composerRule + "\n"},
		{"a draft holding the preamble and the question", composerRule + "\n❯ Explain this:\n  New MCP server found.\n  Do you trust this new MCP server?\n" + composerRule + "\n"},
		// #7464: a composer drawn last explains every row of its own draft, the
		// modal's option rows among them, and a row under it that reads new is
		// its status row whatever its draft holds. Claude Code paints the modal
		// in place of the composer, never inside it.
		{"the word new under a composer that holds a draft", composerRule + "\n❯ fix it\n  please\n" + composerRule + "\n  new"},
		{"a draft of the modal's words, then a row that reads new", composerRule + "\n❯ Explain this:\n  New MCP server found.\n  Do you trust this new MCP server?\n" + composerRule + "\n  new\n"},
		{"a draft that reaches the option rows", composerRule + "\n❯ Explain this:\n  New MCP server found.\n  Do you trust this new MCP server?\n  1. Yes\n" + composerRule},
		{"a draft that reaches an option's ordinal, wrapped at 6 columns", composerRule + "\n❯ Explain this:\n  New\n  MCP\n  server\n  found.\n  Do you\n  trust\n  this\n  new\n  MCP\n  server?\n    1.\n" + composerRule},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.False(t, handled, "no dialog is on this pane; a hold spends the create's trust budget on it (#7341):\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds))
		})
	}
}

// A composer excuses nothing else. Every frame here shows the modal, or cannot
// be told from it, with composer-shaped rows on the pane: it holds, no key.
func TestCheckAndHandleTrustPrompt_ComposerRowsDoNotUnholdTheOldMCPModal(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		// #7341's "frames that must still hold".
		{"a stale boxed composer above a mid-paint modal", "╭────────────────────────╮\n│ > /exit                │\n╰────────────────────────╯\n\nNew MCP server found."},
		{"a boxed composer above the whole modal on a narrow pane", "╭────────────────────────╮\n│ > Try \"fix the bug\"    │\n╰────────────────────────╯\nNew MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"printed rules and a ❯ row above the scrolled tail", "# Usage\n" + composerRule + "\n❯ af sessions list\n" + composerRule + "\nThen pick one.\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm"},
		{"the modal's own cursor row wrapped at 6 columns", "──────\n❯ 1.\nYes\n2. No\n──────\nEnter\nto\nconfirm"},
		{"a boxed composer straight above the framed modal", "╭────────────────────╮\n│ > hi               │\n╭────────────────────╮\n│ New MCP server     │\n│ found. Do you      │"},
		// Round 2's other frames.
		{"a stale ruled composer above a mid-paint modal", composerRule + "\n❯ /exit\n" + composerRule + "\n\nNew MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No"},
		{"a rule and a prompt row, the wrapped modal, a rule", composerRule + "\n❯ run me\nNew MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No\n" + composerRule},
		{"the modal's own cursor row under a rule, a rule below", composerRule + "\n❯ 1. Yes\n  2. No\n" + composerRule + "\nEnter to confirm"},
		{"the cursor on No under a rule, a rule below", composerRule + "\n  1. Yes\n❯ 2. No\n" + composerRule + "\nEnter to confirm"},
		// What the excuse does not reach.
		{"a composer straight above the modal's first row", composerRule + "\n❯ \n" + composerRule + "\nNew MCP server found."},
		// #7344 review: the modal paints "new" itself, as its ninth word, so
		// the word alone is the modal wherever no composer explains it.
		{"the modal mid-paint on a pane 6 columns wide and 3 rows tall", "trust\nthis\nnew"},
		{"a modal prefix above a rule and a row that reads new, no composer", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\n" + composerRule + "\n  new"},
		{"the preamble above a rule and a row that reads new", "New MCP server found.\n" + composerRule + "\n  new"},
		{"the modal's first row, New, straight under a composer", composerRule + "\n❯ \n" + composerRule + "\nNew"},
		{"the word new as the last row, no composer", "● Switched to branch\n  new\n"},
		{"the word new under a composer, a blank row between", composerRule + "\n❯ \n" + composerRule + "\n\n  new"},
		{"two of the modal's words under a composer", composerRule + "\n❯ \n" + composerRule + "\n  New MCP"},
		{"a boxed prompt row that is the modal's option row", "╭────────────────────────╮\n│ > 1. Yes               │\n│ New MCP server found.  │\n╰────────────────────────╯"},
		{"a rule and a prompt row, unindented modal rows, a rule", composerRule + "\n❯ run me\nNew MCP server\nfound. Do you\n" + composerRule},
		{"a rule and an empty prompt, modal rows, a rule", composerRule + "\n❯ \n  New MCP server\n  found. Do you\n" + composerRule},
		{"a boxed prompt, rows with no borders, the box's bottom", "╭────────────────╮\n│ > hi           │\nNew MCP server\nfound. Do you\n╰────────────────╯"},
		{"a prompt row with no rule above it", "❯ Explain this:\n  New MCP server\n  found. Do you\n" + composerRule},
		{"a draft with no closing rule", composerRule + "\n❯ Explain this:\n  New MCP server\n  found. Do you"},
		{"a framed modal between a prompt row and a rule", composerRule + "\n❯ run me\n╭────────────────────╮\n│ New MCP server     │\n" + composerRule},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "the modal is on screen, or the pane cannot be told from one that shows it:\n%s", tt.content)
			require.Empty(t, sentKeystrokes(cmds), "a hold sends no key; got %v", cmds)
		})
	}
}

// The whole modal under a stale composer is still answered with its one Enter.
func TestCheckAndHandleTrustPrompt_WholeOldMCPModalUnderAComposerIsStillAnswered(t *testing.T) {
	content := composerRule + "\n❯ /exit\n" + composerRule + "\nNew MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm"
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, content)
	require.True(t, handled)
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)))
}
