package app

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfiguredNewRemoteOpensBackendField(t *testing.T) {
	h := newTestHome(t)
	got := recordStartRequest(t)
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
	// Enter may arrive before the async catalog; it must not submit a default
	// backend and end naming before the promised picker can open.
	h.pendingProgram = "sh"
	require.NoError(t, h.namingInstance.SetTitle("choose-backend"))
	_, enterCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	for _, msg := range drainCmd(t, enterCmd, 4*time.Second) {
		if key, ok := msg.(tea.KeyMsg); ok {
			_, replay := h.handleKeyPress(key)
			drainCmd(t, replay, 4*time.Second)
		}
	}
	assert.Empty(t, got.Title, "no create request may precede the backend catalog")
	require.Equal(t, stateNew, h.state, "Enter must leave the pending picker form open")
	assert.Equal(t, "Loading backends…", h.errBox.FullError())
	for _, key := range []tea.KeyType{tea.KeyTab, tea.KeyShiftTab, tea.KeyCtrlR, tea.KeyCtrlO} {
		h.handleStateNew(tea.KeyMsg{Type: key})
		require.Equal(t, stateNew, h.state, "switching fields must not strand the pending catalog")
	}
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
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "choose-backend", got.Title)
	assert.Equal(t, "docker", got.Backend)
}

func TestNewRemotePendingCatalogFailureAllowsSubmit(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = repoDeclaringBackend(t, "")
	recordPlaceholderProvision(t)
	got := recordStartRequest(t)
	h.startNewInstanceAtBackend()
	requireNamingFormOpened(t, h, "compatibility form")
	h.pendingProgram = "sh"
	require.NoError(t, h.namingInstance.SetTitle("catalog-failed"))
	h.handleBackendCatalog(backendCatalogMsg{naming: h.namingInstance, err: errors.New("catalog offline")})
	require.Equal(t, stateNew, h.state)
	assert.Contains(t, h.errBox.FullError(), "cannot list backends for this repo: catalog offline")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "catalog-failed", got.Title, "fetch failure must release the submit guard")
}

func TestNewRemotePendingCatalogCancelDoesNotBlockNextForm(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			h := newTestHome(t)
			h.repoRoot = repoDeclaringBackend(t, "")
			recordPlaceholderProvision(t)
			got := recordStartRequest(t)
			h.startNewInstanceAtBackend()
			requireNamingFormOpened(t, h, "compatibility form")
			oldNaming := h.namingInstance
			h.handleStateNew(tea.KeyMsg{Type: key})
			require.Equal(t, stateDefault, h.state)
			h.startNewInstance()
			requireNamingFormOpened(t, h, "ordinary form after cancellation")
			h.handleBackendCatalog(backendCatalogMsg{naming: oldNaming, catalog: twoUsableBackends()})
			require.Equal(t, stateNew, h.state, "stale reply must not open a picker")
			h.pendingProgram = "sh"
			require.NoError(t, h.namingInstance.SetTitle("ordinary-create"))
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			assert.Equal(t, "ordinary-create", got.Title)
		})
	}
}
