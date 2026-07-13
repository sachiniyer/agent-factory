package tmux

import "testing"

// claudeTrustPromptPresent gates whether af taps Enter to dismiss Claude Code's
// folder-trust/MCP prompts. Claude Code has reworded the folder-trust dialog
// over releases, so the matcher must recognize each phrasing af users may see;
// a stale match leaves new sessions stuck on the trust screen with no input box.
func TestClaudeTrustPromptPresent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "current folder-trust wording",
			content: " Quick safety check: Is this a project you created or one you trust? (Like your\n" +
				" own code, a well-known open source project, or work from your team).\n" +
				" ❯ 1. Yes, I trust this folder\n   2. No, exit\n Enter to confirm · Esc to cancel",
			want: true,
		},
		{
			name:    "prior folder-trust wording",
			content: "Do you trust the files in this folder?",
			want:    true,
		},
		{
			name:    "mcp server prompt",
			content: "New MCP server found. Do you trust this new MCP server?",
			want:    true,
		},
		{
			name:    "normal claude ui with input box",
			content: " ▐▛███▜▌   Claude Code v2.1.207\n❯ \n  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeTrustPromptPresent(tc.content); got != tc.want {
				t.Errorf("claudeTrustPromptPresent() = %v, want %v", got, tc.want)
			}
		})
	}
}
