package tmux

import (
	"strings"
	"testing"
)

// claudeTrustPromptPresent decides whether the daemon's continuous, visible-only
// pane poll should tap Enter to dismiss a Claude Code launch gate. It must fire
// on the real folder-trust dialog (both the old and reworded wording) and the
// MCP-server prompt, but NEVER on a stray mention of the reworded question in
// ordinary agent output — otherwise the poll injects a spurious Enter (#blank-pane).
//
// The MCP branch additionally requires its "Enter to confirm" footer to be the
// last non-blank content in the pane (claudeMCPTrustFooterIsLast), so a quoted
// mention of the phrase with the composer painted below it does not fire.
func TestClaudeTrustPromptPresent(t *testing.T) {
	// Full reworded folder-trust modal, as Claude Code now renders it.
	rewordedDialog := `╭──────────────────────────────────────────────╮
│ Quick safety check:                            │
│ Is this a project you created or one you trust?│
│                                                │
│ ❯ 1. Yes, I trust this folder                  │
│   2. No, cancel                                │
│                                                │
│ Enter to confirm · Esc to cancel               │
╰──────────────────────────────────────────────╯`

	// The reworded question appears in scrollback/agent output with no dialog
	// chrome — must NOT be treated as a live prompt.
	rewordedMention := `The onboarding docs ask: "Is this a project you created or one you trust?"
Here is a summary of what I changed in the repo...`

	// Old folder-trust wording (kept for older Claude Code builds).
	oldDialog := `Do you trust the files in this folder?
❯ Yes  No`

	// Ordinary Claude input box — no gate.
	normalUI := `╭─────────────────────────────────────────╮
│ > Type your message here                  │
╰─────────────────────────────────────────╯
? for shortcuts`

	// Full MCP trust modal, as Claude Code renders it — the footer is the last
	// non-blank line, so a live modal is accepted.
	mcpModal := "New MCP server found. Do you trust this new MCP server?\n" +
		"❯ 1. Yes\n  2. No\nEnter to confirm"

	// The MCP phrase in agent output with the composer painted below it — the
	// footer is NOT last, so quoted output is refused.
	mcpQuotedWithComposer := mcpModal + "\n\n" +
		"╭────────────────────────────────────╮\n" +
		"│ > Type your message here           │\n" +
		"╰────────────────────────────────────╯\n" +
		"? for shortcuts\n"

	// The MCP phrase in agent prose — no footer at all.
	mcpProse := "I see the New MCP server found prompt. Do you trust this new MCP server?"

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"reworded full dialog", rewordedDialog, true},
		{"reworded phrase without dialog marker", rewordedMention, false},
		{"old folder-trust wording", oldDialog, true},
		{"MCP modal footer last", mcpModal, true},
		{"MCP modal lowercase footer last", strings.ToLower(mcpModal), true},
		{"MCP phrase without footer", "New MCP server found. Do you trust this new MCP server?\n❯ 1. Yes", false},
		{"MCP phrase quoted above composer", mcpQuotedWithComposer, false},
		{"MCP phrase in agent prose", mcpProse, false},
		{"MCP mention without unique question", "I added a new MCP server entry to .mcp.json for you.", false},
		{"MCP mention plus generic confirm affordance", "I added a new MCP server entry.\nPress Enter to confirm the change.", false},
		{"normal claude input box", normalUI, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeTrustPromptPresent(tt.content); got != tt.want {
				t.Errorf("claudeTrustPromptPresent() = %v, want %v", got, tt.want)
			}
		})
	}
}
