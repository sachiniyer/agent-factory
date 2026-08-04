package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorDetailsOverlayShowsFullTruncatedError(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	_, errorLogs := captureHomeMessageLogs(t)

	msg := "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS); PR URL: https://example.invalid/pr/987"
	cmd := h.handleError(errors.New(msg))
	require.NotNil(t, cmd, "handleError still returns the normal clear-message command")
	require.Contains(t, errorLogs.String(), msg,
		"a real operation failure remains an ERROR while notices move to INFO")
	require.Contains(t, h.errBox.String(), "E details",
		"truncated status error should advertise the details key")

	_, detailsCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
	require.Nil(t, detailsCmd)
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)

	rendered := h.textOverlay.Render()
	assert.Contains(t, rendered, "Last error")
	assert.Contains(t, rendered, "https://example.invalid/pr/987",
		"full fallback data must be recoverable from the details overlay")
}

// TestErrorDetailsSurvivesTheNoticeVisualTimeout is #2618. `E details` is the
// only in-app way to read a notice the status bar clipped, and it used to stop
// working 3 seconds later — at exactly the moment the clipped text left the
// screen. The one case the affordance exists for was the one case it could not
// serve. The notice now outlives its render.
func TestErrorDetailsSurvivesTheNoticeVisualTimeout(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	msg := "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS); PR URL: https://example.invalid/pr/987"
	require.NotNil(t, h.handleError(errors.New(msg)))
	require.Contains(t, h.errBox.String(), "E details", "precondition: the notice is up and clipped")

	// The 3s visual timeout fires.
	_, _ = h.Update(hideErrMsg{noticeID: h.transientNoticeID})
	require.Empty(t, h.errBox.FullError(), "the bar must stop rendering the expired notice")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
	require.Equal(t, stateHelp, h.state, "E must still open the notice after it left the screen")
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "https://example.invalid/pr/987",
		"the half the bar clipped is the whole point of the affordance")
}

// A notice that was RETRACTED — not merely expired — must not stay reachable:
// it stopped being true, so re-opening it would show the user something af has
// already taken back.
func TestErrorDetailsDoesNotResurrectARetractedNotice(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, string, error) { return "af-config-1", "", nil }))

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	require.NotNil(t, cmd)
	spawned := cmd().(configAgentSpawnedMsg)
	require.Contains(t, h.errBox.FullError(), "Starting the config agent", "precondition")

	h.handleConfigAgentSpawned(spawned)
	require.Empty(t, h.errBox.FullError(), "precondition: the spawn retracts its own notice")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
	assert.Equal(t, stateDefault, h.state, "a retracted notice is gone, not merely off screen")
	assert.Nil(t, h.textOverlay)
}

// The overlay's title must match what it is showing. af hiding a pane and
// telling you how to get it back is designed behavior, not a failure, and
// filing it under "Last error" reports af working as intended as a fault
// (#2618 / #2575).
func TestErrorDetailsTitleDistinguishesNoticesFromFailures(t *testing.T) {
	tests := []struct {
		name  string
		raise func(*home) tea.Cmd
		title string
	}{
		{
			name:  "operation failure",
			raise: func(h *home) tea.Cmd { return h.handleError(errors.New("git worktree remove failed")) },
			title: "Last error",
		},
		{
			name:  "declined action",
			raise: func(h *home) tea.Cmd { return h.handleNotice(errors.New("no PR for this session yet")) },
			title: "Last notice",
		},
		{
			name:  "success message",
			raise: func(h *home) tea.Cmd { return h.showTransientMessage("Daemon restarted.") },
			title: "Last notice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			resizeHome(h, 80, 24)
			require.NotNil(t, tc.raise(h))

			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
			require.Equal(t, stateHelp, h.state)
			require.NotNil(t, h.textOverlay)
			assert.Contains(t, h.textOverlay.Render(), tc.title)
		})
	}
}
