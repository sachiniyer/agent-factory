package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for the scrolled-off old-MCP trust modal (#3579 class).
//
// The old single-server MCP trust modal ("New MCP server found. Do you trust
// this new MCP server? ❯ 1. Yes … Enter to confirm") is one of the launch
// gates CheckAndHandleTrustPrompt answers. Its preamble and question sit at the
// top of the modal; on a pane too short for it they scroll off, leaving only the
// option rows and the "Enter to confirm" footer on screen:
//
//	❯ 1. Yes
//	  2. No
//	Enter to confirm
//
// claudeMCPTrustFooterIsLast requires the question AND preamble, so the
// scrolled-off frame read as dialog-free; claudeMCPDialogPartiallyRendered bailed
// on the missing preamble too. A false return is the create path's permission to
// type the user's prompt and press Enter — on the modal's preselected "1. Yes" —
// silently auto-trusting an MCP server that can execute code. The fix,
// claudeMCPTrustTailOf, mirrors the folder-trust twin
// (claudeFolderTrustFooterOnly, a32e070c): fail closed — hold without a key.
// #7306 widened it to the rest of the modal's unreadable shapes; their
// end-to-end tests are in claude_mcp_trust_tail_test.go.

// mcpScrolledOffOptionsFooter is the canonical scrolled-off frame: the two
// option rows and the footer, nothing else, exactly as a too-short pane leaves
// the old MCP modal once its preamble and question have scrolled into scrollback.
const mcpScrolledOffOptionsFooter = "❯ 1. Yes\n  2. No\nEnter to confirm"

// TestClaudeMCPTrustTailOf pins the predicate directly: what it holds on, what
// it reports seeing, and what it leaves alone. It is asked only after every
// clause that answers a dialog has declined the frame, so the whole modal never
// reaches it through CheckAndHandleTrustPrompt.
func TestClaudeMCPTrustTailOf(t *testing.T) {
	question := "Do you trust this new MCP server?"
	prefix := "New MCP server found. " + question
	held := func(option, footer string, below int) *claudeMCPTrustTail {
		return &claudeMCPTrustTail{option: option, footer: footer, below: below}
	}
	const footer = "Enter to confirm"
	tests := []struct {
		name    string
		content string
		want    *claudeMCPTrustTail // nil: not held
	}{
		// The modal's footer under one of its own option rows.
		{"options and footer only, contiguous", mcpScrolledOffOptionsFooter, held("No", footer, 0)},
		{"options and footer, blank before footer", "❯ 1. Yes\n  2. No\n\nEnter to confirm", held("No", footer, 0)},
		{"banner rows above, options and footer last", "Welcome to Claude Code\n\n❯ 1. Yes\n  2. No\nEnter to confirm", held("No", footer, 0)},
		{"question without preamble above", question + "\n\n❯ 1. Yes\n  2. No\nEnter to confirm", held("No", footer, 0)},
		{"lowercase options and footer", "❯ 1. yes\n  2. no\nenter to confirm", held("no", "enter to confirm", 0)},
		{"No preselected", "  1. Yes\n❯ 2. No\nEnter to confirm", held("No", footer, 0)},
		{"one option row, No", "  2. No\nEnter to confirm", held("No", footer, 0)},
		{"one option row, Yes", "❯ 1. Yes\nEnter to confirm", held("Yes", footer, 0)},
		// The preamble on screen is evidence for the dialog, never against it.
		{"the whole modal", prefix + "\n❯ 1. Yes\n  2. No\nEnter to confirm", held("No", footer, 0)},
		{"the question wrapped", "New MCP server found. Do you trust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to confirm", held("No", footer, 0)},
		{"preamble mid-scrollback, options and footer last", "Earlier: new mcp server found in the logs.\n\n❯ 1. Yes\n  2. No\nEnter to confirm", held("No", footer, 0)},
		// A footer that is not one row, and rows below it.
		{"a longer footer", "❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel", held("No", "Enter to confirm · Esc to cancel", 0)},
		{"the footer wrapped onto two rows", "❯ 1. Yes\n  2. No\nEnter to\nconfirm", held("No", footer, 0)},
		{"the footer wrapped onto three rows", "  2. No\nEnter\nto\nconfirm", held("No", footer, 0)},
		{"the footer wrapped across the closing frame", "│   2. No  │\n│ Enter to │\n╰──────────╯\n  confirm", held("No", footer, 0)},
		{"a longer footer wrapped", "❯ 1. Yes\n  2. No\nEnter to confirm · Esc\nto cancel", held("No", "Enter to confirm · Esc", 1)},
		{"a hint row below the footer", "❯ 1. Yes\n  2. No\nEnter to confirm\n\n? for shortcuts", held("No", footer, 1)},
		{"prose below the footer and no composer", "  2. No\nEnter to confirm\nThat is the dialog.\nIt asks once.", held("No", footer, 2)},
		{"a second footer below the first", "  2. No\nEnter to confirm\nEnter to confirm", held("No", footer, 1)},
		// A pane too short for an option row: the footer is its first content.
		{"only the footer", "Enter to confirm", held("", footer, 0)},
		{"only the footer, wrapped", "Enter to\nconfirm", held("", footer, 0)},
		{"only the footer, under the closing frame", "╰────────────────╯\n  Enter to confirm", held("", footer, 0)},

		// A composer below the footer: the modal's rows are quoted output.
		{"quote with the boxed composer below", mcpScrolledOffOptionsFooter + "\n\n╭────────────────────────────────────╮\n│ > Type your message here           │\n╰────────────────────────────────────╯\n? for shortcuts\n", nil},
		{"quote with the ruled composer below", mcpScrolledOffOptionsFooter + "\n\n────────\n❯ \n────────\n", nil},
		{"wrapped footer with the ruled composer below", "  2. No\nEnter to\nconfirm\n────────\n❯ draft\n────────\n", nil},
		// Rows that are not the modal's.
		{"unrelated picker whose options do not match yes/no", prefix + "\n❯ Resume previous conversation\n  Start new conversation\nEnter to confirm", nil},
		{"suffix-matching options are not exact yes/no", prefix + "\n❯ Always yes\n  Definitely no\nEnter to confirm", nil},
		{"no footer (still painting)", "❯ 1. Yes\n  2. No\n", nil},
		{"the footer's first words and nothing after", "  2. No\nEnter to", nil},
		{"the footer's first word, then another row", "  2. No\nEnter\nthe room", nil},
		{"a blank pane", "\n\n", nil},
		{"MCP phrase in agent prose, no picker", "I see the New MCP server found prompt. Do you trust this new MCP server?", nil},
		{"generic confirm affordance, not the modal footer", "I added a new MCP server entry.\nPress Enter to confirm the change.", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := claudeMCPTrustTailOf(visibleTextOf(tt.content))
			if tt.want == nil {
				require.False(t, ok, "not the modal's tail, got %+v:\n%s", got, tt.content)
				return
			}
			require.True(t, ok, "the modal's tail must hold:\n%s", tt.content)
			require.Equal(t, *tt.want, got, "\n%s", tt.content)
		})
	}
}

// TestClaudeMCPTrustTailOf_StyledHolds pins that the hold sees through per-word
// SGR styling: the predicate reads the visibleText, stripped once by
// visibleTextOf, as every sibling does.
func TestClaudeMCPTrustTailOf_StyledHolds(t *testing.T) {
	styled := styleWordsPerWord("❯ 1. Yes\n  2. No\nEnter to confirm")
	_, ok := claudeMCPTrustTailOf(visibleTextOf(styled))
	require.True(t, ok, "a styled scrolled-off frame must hold the same as a plain one")
}

// TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalHolds is the end-to-end
// regression: every scrolled-off shape af's daemon poll can capture must block
// (return true) and receive no keystroke. Before claudeMCPTrustTailOf each
// of these read as dialog-free and the create path typed the prompt and pressed
// Enter on the preselected "1. Yes".
func TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalHolds(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"options and footer only, contiguous", mcpScrolledOffOptionsFooter},
		{"options and footer, blank before footer", "❯ 1. Yes\n  2. No\n\nEnter to confirm"},
		{"banner rows above, options and footer last", "Welcome to Claude Code\n\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"banner and blank before footer", "Welcome to Claude Code\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm"},
		{"question without preamble above, blank separator", "Do you trust this new MCP server?\n\n❯ 1. Yes\n  2. No\nEnter to confirm"},
		{"question without preamble above, no blank separator", "Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled,
				"a scrolled-off MCP modal is still in the way; reporting false lets the create "+
					"path paste the prompt and press Enter on the preselected Yes (#3579 class)")
			require.Empty(t, sentKeystrokes(cmds),
				"af must hold without a key on a frame it cannot disambiguate from a quote; got %v", cmds)
		})
	}
}

// TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalHoldsStyled is the styled
// end-to-end: the same scrolled-off shape rendered with per-word SGR must hold.
func TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalHoldsStyled(t *testing.T) {
	styled := styleWordsPerWord("❯ 1. Yes\n  2. No\nEnter to confirm")
	handled, cmds := pollStaticPane(t, styled, 4)
	require.True(t, handled,
		"a styled scrolled-off MCP modal must hold, not be read as dialog-free")
	require.Empty(t, sentKeystrokes(cmds), "no key on a held frame; got %v", cmds)
}

// TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalReportsOncePerPoll confirms
// the hold does not silently turn into a refusal that consumes the budget
// differently: across several polls on an unchanging scrolled-off pane, af
// reports the pane in the way every time (true) and never injects a key. This
// is the shape that lets the create's trust-prompt budget expire loudly, exactly
// as the folder-trust twin's hold does.
func TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalReportsInWayEveryPoll(t *testing.T) {
	handled, cmds := pollStaticPane(t, mcpScrolledOffOptionsFooter, 6)
	require.True(t, handled, "the scrolled-off modal is in the way on every poll")
	require.Empty(t, sentKeystrokes(cmds), "no key across the whole budget; got %v", cmds)
}

// TestCheckAndHandleTrustPrompt_FullMCPCModalStillFiresEnterBesideScrolledHold
// is the control: the hold must not widen to swallow the full modal. A pane
// showing the complete old MCP modal — preamble, question, options and footer
// in place — still fires the historical Enter tap, because
// claudeMCPTrustFooterIsLast claims it before claudeMCPTrustTailOf is asked.
func TestCheckAndHandleTrustPrompt_FullMCPCModalStillFiresEnterBesideScrolledHold(t *testing.T) {
	full := "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm"
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, full)
	require.True(t, handled, "the full MCP modal is in the way")
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
		"the full modal keeps the historical Enter tap; the scrolled-off hold must not widen past it")
}

// TestClaudeMCPTrustPaintingOf pins the mid-paint predicate directly (#7330):
// rows that spell the modal's words from the first, in order, down to the last
// content in the pane.
func TestClaudeMCPTrustPaintingOf(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    bool
	}{
		{"the first row of a wrapped preamble", "New MCP server", true},
		{"cut inside the wrapped question", "New MCP server found.\nDo you trust this new MCP", true},
		{"both options, no footer", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No", true},
		{"the footer's first row", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to", true},
		{"the whole modal, wrapped", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ 1. Yes\n  2. No\nEnter to\nconfirm", true},
		{"the cursor on an ordinal a narrow pane left alone", "New MCP\nserver\nfound.\nDo you\ntrust\nthis\nnew MCP\nserver?\n❯ 1.", true},
		{"output above", "Welcome\n\nNew MCP server\nfound. Do you", true},
		{"upper case", "NEW MCP SERVER\nFOUND. DO YOU", true},

		{"a blank pane", "\n\n", false},
		{"words after the footer", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc", false},
		{"a ❯ row before the options", "❯ New MCP server\n  found. Do you", false},
		{"a bare ❯ after the question", "New MCP server\nfound. Do you\ntrust this new MCP\nserver?\n❯ ", false},
		{"one word, no composer", "New", true},
		{"one word in lower case, no composer", "new", true},
		{"the modal's own ninth word on a 6-column pane", "trust\nthis\nnew", true},
		{"one word straight under a drawn composer", "────────\n❯ \n────────\n  new\n", false},
		{"the modal's first row straight under a drawn composer", "────────\n❯ \n────────\n  New\n", true},
		{"one word under a composer that holds a draft", "────────\n❯ fix it\n  please\n────────\n  new\n", true},
		{"one word under a rule with no composer above it", "New MCP server found.\n────────\n  new\n", true},
		{"one word under a composer, a blank row between", "────────\n❯ \n────────\n\n  new\n", true},
		{"two words under a drawn composer", "────────\n❯ \n────────\n  new mcp\n", true},
		{"a boxed prompt row that is the modal's option", "╭──────────────────────╮\n│ > 1. Yes             │\n│ New MCP server found. │\n╰──────────────────────╯\n", true},
		{"the later rows of a composer draft", "────────\n❯ Explain this:\n  New MCP server\n  found. Do you\n────────\n", false},
		{"the later rows of a boxed composer draft", "╭────────────────╮\n│ > Explain this: │\n│   New MCP server │\n╰────────────────╯\n", false},
		{"draft rows that are not indented", "────────\n❯ run me\nNew MCP server\n────────\n", true},
		{"a draft that reaches an option row", "────────\n❯ Explain this:\n  New MCP server found. Do you trust this new MCP server?\n  1. Yes\n────────\n", true},
		{"an ordinal before the options", "New MCP server\n1.", false},
		{"a word out of order", "New MCP server\nfound. Do they", false},
		{"not from the first word", "server found. Do you", false},
		{"other rows below", "New MCP server\nfound. Do you\n? for shortcuts", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, claudeMCPTrustPaintingOf(visibleTextOf(tt.content)), "\n%s", tt.content)
		})
	}
}
