package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeTrustPromptPresent must recognize every folder-trust / MCP phrasing af
// dismisses, across Claude Code versions, without firing on the normal input
// UI. The reworded dialog ("Is this a project you created or one you trust?")
// is the #1714 regression: matching only the prior wording left fresh worktrees
// hung on the trust screen, rendering a blank pane.
func TestClaudeTrustPromptPresent(t *testing.T) {
	cases := []struct {
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
			// The reworded phrase is natural language; seeing it in ordinary agent
			// output (no dialog markers) must NOT fire a dismissal Enter (#1714 /
			// #1715 Greptile). Only the real dialog, which co-renders a marker,
			// dismisses.
			name:    "reworded phrase in ordinary output without dialog markers",
			content: "Summary: the gate asks \"Is this a project you created or one you trust\" and then waits.\n❯ ",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, claudeTrustPromptPresent(tc.content))
		})
	}
}
