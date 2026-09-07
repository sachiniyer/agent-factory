package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfiguredNewRemoteOpensBackendField(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = repoDeclaringBackend(t, "")
	recordPlaceholderProvision(t)
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"new_remote": {"alt+n"}}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
	stubAccounts(t, twoAgentsWithAccounts(), nil)
	calls := stubBackends(t, twoUsableBackends(), nil)
	_, cmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}, Alt: true})
	// Replay the menu-highlight hop before inspecting the creation outcome.
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if key, ok := msg.(tea.KeyMsg); ok {
			_, cmd = h.handleKeyPress(key)
		}
	}
	requireNamingFormOpened(t, h, "configured new_remote must open creation without a local hooks precheck")
	require.Equal(t, stateNew, h.state)
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if catalog, ok := msg.(backendCatalogMsg); ok {
			h.handleBackendCatalog(catalog)
		}
	}
	require.Equal(t, 1, *calls, "compatibility binding must request the creation form backend field")
	require.Equal(t, stateSelectBackend, h.state)
	assert.Empty(t, h.pendingBackend, "the user chooses the backend")
	pickBackend(t, h, "docker")
	assert.Equal(t, "docker", h.pendingBackend)
	assert.Equal(t, stateNew, h.state)
}
