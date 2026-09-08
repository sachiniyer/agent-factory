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

func TestPendingBackendCancelCannotReplayIntoPicker(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			h := newTestHome(t)
			h.repoRoot = repoDeclaringBackend(t, "")
			recordPlaceholderProvision(t)
			h.startNewInstanceAtBackend()
			require.Equal(t, stateNew, h.state)
			naming := h.namingInstance
			h.pendingPrompt = "draft prompt"
			h.pendingBackend = "docker"
			h.pendingAccount = "work"

			_, cmd := h.handleKeyPress(tea.KeyMsg{Type: key})
			assert.Equal(t, stateDefault, h.state, "physical cancellation must complete synchronously")
			h.Update(backendCatalogMsg{naming: naming, catalog: twoUsableBackends()})
			for _, msg := range drainCmd(t, cmd, time.Second) {
				if replay, ok := msg.(tea.KeyMsg); ok {
					assert.Fail(t, "cancellation must not schedule a key replay")
					h.handleKeyPress(replay)
				}
			}
			assert.Equal(t, stateDefault, h.state)
			assert.Nil(t, h.namingInstance)
			assert.Nil(t, h.selectionOverlay)
			assert.NotContains(t, h.store.GetInstances(), naming)
			assert.Empty(t, h.namingPlaceholder)
			assert.Empty(t, h.pendingPrompt)
			assert.Empty(t, h.pendingBackend)
			assert.False(t, h.backendPickerPending)
			assert.Empty(t, h.pendingAccount)
		})
	}
}
