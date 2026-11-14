package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// visibleTextOf removes every escape capture-pane -e emits — SGR and OSC 8,
// terminated by ST or by BEL — and nothing a person reading the pane would see
// (#7271).
func TestVisibleTextOf(t *testing.T) {
	for _, tt := range []struct{ name, capture, want string }{
		{"plain text is unchanged", "Yes, I trust this folder\n", "Yes, I trust this folder\n"},
		{"CRLF folds to LF", "a\r\nb\r\n", "a\nb\n"},
		{"SGR per word", "\x1b[1m\x1b[38;5;220mYes,\x1b[0m \x1b[1m\x1b[38;5;220mI\x1b[0m trust", "Yes, I trust"},
		{"an SGR carried to the start of the next row", "probe\n\x1b[0m  MCP servers", "probe\n  MCP servers"},
		{"OSC 8 terminated by ST", "Learn more in the \x1b]8;id=c415zw;https://code.claude.com/docs/en/mcp\x1b\\MCP documentation\x1b]8;;\x1b\\.",
			"Learn more in the MCP documentation."},
		{"OSC 8 terminated by BEL", "Yes, I \x1b]8;;https://example.invalid/\atrust\x1b]8;;\a this folder", "Yes, I trust this folder"},
		{"OSC 8 inside a styled word", "\x1b[38;5;153m\x1b]8;;https://example.invalid/\x1b\\trust\x1b]8;;\x1b\\\x1b[39m", "trust"},
		// An OSC never spans a row: one with no terminator on its row stays,
		// rather than swallowing the composer painted below it.
		{"an unterminated OSC does not eat the rows below", "\x1b]8;;https://example.invalid/\n❯ draft\n", "\x1b]8;;https://example.invalid/\n❯ draft\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, visibleText(tt.want), visibleTextOf(tt.capture))
			require.Equal(t, visibleTextOf(tt.capture), visibleTextOf(string(visibleTextOf(tt.capture))),
				"stripping is idempotent, so text that is already visible may be stripped again")
		})
	}
}

// The measured captures carry both escape kinds: SGR on every styled word and
// an OSC 8 link in the body (testdata/claude_2_1_322_mcp_server_dialog.ansi).
// Their visible text keeps neither, and keeps every word the link shows.
func TestVisibleTextOf_MeasuredCapturesKeepNoEscape(t *testing.T) {
	for _, fixture := range []string{
		mcpServerDialogFixture, mcpServerDialog40Fixture, mcpServerDialog50Fixture,
		mcpServerMultiServerFixture, mcpServerComposerFixture,
	} {
		t.Run(fixture, func(t *testing.T) {
			raw := readTrustFixture(t, fixture)
			require.Contains(t, raw, "\x1b]8;", "precondition: the measured capture carries an OSC 8 link")
			screen := string(visibleTextOf(raw))
			require.NotContains(t, screen, "\x1b")
			require.NotContains(t, screen, "]8;", "an OSC 8 link's parameters are not visible text")
		})
	}
	require.Contains(t, string(visibleTextOf(readTrustFixture(t, mcpServerDialogFixture))),
		"Learn more in the MCP documentation.")
}
