package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPendingBackendInputCannotReplayIntoPicker(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEnter, tea.KeyTab, tea.KeyShiftTab, tea.KeyCtrlR, tea.KeyCtrlO} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			h := newTestHome(t)
			h.repoRoot = repoDeclaringBackend(t, "")
			recordPlaceholderProvision(t)
			h.startNewInstanceAtBackend()
			require.Equal(t, stateNew, h.state)
			_, cmd := h.handleKeyPress(tea.KeyMsg{Type: key})
			// The catalog lands after the physical press, before its command runs.
			h.Update(backendCatalogMsg{naming: h.namingInstance, catalog: twoUsableBackends()})
			require.Equal(t, stateSelectBackend, h.state)
			for _, msg := range drainCmd(t, cmd, 4*time.Second) {
				if replay, ok := msg.(tea.KeyMsg); ok {
					assert.Fail(t, "pending field input must not schedule a key replay")
					h.handleKeyPress(replay)
				}
			}
			require.Equal(t, stateSelectBackend, h.state, "old input must not accept the newly opened picker")
			assert.Equal(t, 0, h.selectionOverlay.GetSelectedIndex())
			assert.False(t, h.keySent)
			pickBackend(t, h, "docker")
			assert.Equal(t, stateNew, h.state)
			assert.Equal(t, "docker", h.pendingBackend)
		})
	}
}
