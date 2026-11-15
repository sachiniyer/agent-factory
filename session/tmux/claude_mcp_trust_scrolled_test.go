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
// claudeMCPDialogScrolledOff, mirrors the folder-trust twin
// (claudeFolderTrustFooterOnly, a32e070c): fail closed — hold without a key.

// mcpScrolledOffOptionsFooter is the canonical scrolled-off frame: the two
// option rows and the footer, nothing else, exactly as a too-short pane leaves
// the old MCP modal once its preamble and question have scrolled into scrollback.
const mcpScrolledOffOptionsFooter = "❯ 1. Yes\n  2. No\nEnter to confirm"

// TestClaudeMCPDialogScrolledOff pins the predicate directly. The hold must fire
// on every shape a too-short pane leaves of the modal (contiguous, blank before
// the footer, banner rows above, banner + blank) and the two ambiguous
// "question without preamble above a yes/no picker" shapes that read the same
// way; it must NOT fire on a frame the full modal, a quote, or an unrelated
// picker owns.
func TestClaudeMCPDialogScrolledOff(t *testing.T) {
	question := "Do you trust this new MCP server?"
	prefix := "New MCP server found. " + question
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		// The scrolled-off shapes af must hold on.
		{"options and footer only, contiguous", mcpScrolledOffOptionsFooter, true},
		{"options and footer, blank before footer", "❯ 1. Yes\n  2. No\n\nEnter to confirm", true},
		{"banner rows above, options and footer last", "Welcome to Claude Code\n\n❯ 1. Yes\n  2. No\nEnter to confirm", true},
		{"banner and blank before footer", "Welcome to Claude Code\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm", true},
		{"question without preamble above, blank separator (ambiguous)", question + "\n\n❯ 1. Yes\n  2. No\nEnter to confirm", true},
		{"question without preamble above, no blank separator (ambiguous)", question + "\n❯ 1. Yes\n  2. No\nEnter to confirm", true},
		{"lowercase options and footer", "❯ 1. yes\n  2. no\nenter to confirm", true},
		{"No preselected (cursor on the second row)", "  1. Yes\n❯ 2. No\nEnter to confirm", true},

		// Frames the predicate must leave alone.
		{"full modal with preamble (fires Enter via claudeMCPTrustFooterIsLast)", prefix + "\n❯ 1. Yes\n  2. No\nEnter to confirm", false},
		{"full modal with preamble and blank before footer", prefix + "\n❯ 1. Yes\n  2. No\n\nEnter to confirm", false},
		{"full modal with blank above and below options", prefix + "\n\n❯ 1. Yes\n  2. No\n\nEnter to confirm", false},
		{"preamble mid-scrollback, options and footer last", "Earlier: new mcp server found in the logs.\n\n❯ 1. Yes\n  2. No\nEnter to confirm", false},
		{"quote with the composer painted below", mcpScrolledOffOptionsFooter + "\n\n╭────────────────────────────────────╮\n│ > Type your message here           │\n╰────────────────────────────────────╯\n? for shortcuts\n", false},
		{"unrelated picker whose options do not match yes/no", prefix + "\n❯ Resume previous conversation\n  Start new conversation\nEnter to confirm", false},
		{"suffix-matching options are not exact yes/no", prefix + "\n❯ Always yes\n  Definitely no\nEnter to confirm", false},
		{"no footer (still painting)", "❯ 1. Yes\n  2. No\n", false},
		{"footer only, no options", "Enter to confirm", false},
		{"one option then footer (incomplete)", "❯ 1. Yes\nEnter to confirm", false},
		{"MCP phrase in agent prose, no picker", "I see the New MCP server found prompt. Do you trust this new MCP server?", false},
		{"generic confirm affordance, not the modal footer", "I added a new MCP server entry.\nPress Enter to confirm the change.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeMCPDialogScrolledOff(visibleTextOf(tt.content))
			require.Equal(t, tt.want, got, "claudeMCPDialogScrolledOff must hold only on the ambiguous scrolled-off shape")
		})
	}
}

// TestClaudeMCPDialogScrolledOff_StyledHolds pins that the hold sees through
// per-word SGR styling, the way every ANSI-stripping sibling already does: a
// too-short pane renders the option rows and footer with the same per-word SGR
// the full modal's question carries, and claudeMCPDialogScrolledOff reads the
// visibleText (stripped once by visibleTextOf) the same way.
func TestClaudeMCPDialogScrolledOff_StyledHolds(t *testing.T) {
	styled := styleWordsPerWord("❯ 1. Yes\n  2. No\nEnter to confirm")
	require.True(t, claudeMCPDialogScrolledOff(visibleTextOf(styled)),
		"a styled scrolled-off frame must hold the same as a plain one")
}

// TestCheckAndHandleTrustPrompt_ScrolledOffMCPModalHolds is the end-to-end
// regression: every scrolled-off shape af's daemon poll can capture must block
// (return true) and receive no keystroke. Before claudeMCPDialogScrolledOff each
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
// claudeMCPTrustFooterIsLast claims it before claudeMCPDialogScrolledOff runs.
func TestCheckAndHandleTrustPrompt_FullMCPCModalStillFiresEnterBesideScrolledHold(t *testing.T) {
	full := "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes\n  2. No\nEnter to confirm"
	handled, cmds := runTrustPromptCheck(t, ProgramClaude, full)
	require.True(t, handled, "the full MCP modal is in the way")
	require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
		"the full modal keeps the historical Enter tap; the scrolled-off hold must not widen past it")
}
