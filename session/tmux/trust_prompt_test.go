package tmux

import "testing"

// claudeTrustPromptPresent decides whether the daemon's continuous, visible-only
// pane poll should tap Enter to dismiss a Claude Code launch gate. It must fire
// on the real folder-trust dialog (both the old and reworded wording) and the
// MCP-server prompt, but NEVER on a stray mention of the reworded question in
// ordinary agent output — otherwise the poll injects a spurious Enter (#blank-pane).
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

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"reworded full dialog", rewordedDialog, true},
		{"reworded phrase without dialog marker", rewordedMention, false},
		{"old folder-trust wording", oldDialog, true},
		{"MCP prompt titlecase", "New MCP server found. Do you trust this new MCP server?", true},
		{"MCP prompt lowercase", "new mcp server found. do you trust this new mcp server?", true},
		{"MCP mention in ordinary output", "I configured the mcp server in .mcp.json for you.", false},
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
