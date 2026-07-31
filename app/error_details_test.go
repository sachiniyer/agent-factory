package app

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorDetailsOverlayShowsFullTruncatedError(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	msg := "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS); PR URL: https://example.invalid/pr/987"
	cmd := h.handleError(errors.New(msg))
	require.NotNil(t, cmd, "handleError still returns the normal clear-message command")
	require.Contains(t, h.errBox.String(), "E details",
		"truncated status error should advertise the details key")

	_, detailsCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
	require.Nil(t, detailsCmd)
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)

	rendered := h.textOverlay.Render()
	assert.Contains(t, rendered, "Message details")
	assert.Contains(t, rendered, "https://example.invalid/pr/987",
		"full fallback data must be recoverable from the details overlay")
}

func TestErrorDetailsSurviveTransientNoticeTimeout(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	msg := "alpha · ◆ Agent hidden — too narrow for 2 panes; resize wider or use `s` open pane"
	cmd := h.handleError(errors.New(msg))
	require.NotNil(t, cmd)
	noticeID := h.transientNoticeID
	require.Contains(t, h.errBox.String(), "E details",
		"the clipped live notice must advertise its recovery key")

	_, _ = h.Update(hideErrMsg{noticeID: noticeID})
	require.Empty(t, strings.TrimSpace(h.errBox.String()),
		"the transient status line still disappears after its visual timeout")

	_, detailsCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("E")})
	require.Nil(t, detailsCmd)
	require.NotNil(t, h.textOverlay,
		"the details overlay must remain available after the status line disappears")

	rendered := h.textOverlay.Render()
	assert.Contains(t, rendered, "Message details")
	assert.Contains(t, rendered, "resize wider or use `s` open pane",
		"the clipped tail must remain recoverable after the visual timeout")
}
