package app

import (
	"errors"
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
	assert.Contains(t, rendered, "Last error")
	assert.Contains(t, rendered, "https://example.invalid/pr/987",
		"full fallback data must be recoverable from the details overlay")
}
