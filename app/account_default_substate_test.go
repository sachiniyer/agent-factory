package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Account preselection belongs to the naming flow, even when a field's response
// opens its picker before the account-default response arrives (#4025).
func TestProjectDefaultArrivesWhileNamingFieldIsOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state state
		close tea.KeyType
	}{
		{"backend", stateSelectBackend, tea.KeyEsc},
		{"program", stateSelectProgram, tea.KeyEsc},
		{"account", stateSelectAccount, tea.KeyEsc},
		{"prompt", statePromptInput, tea.KeyTab},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.menu.SetSize(200, 1)
			naming := startNaming(t, h, "default-while-editing")
			resp := withDefaults(map[string]string{"claude": "work"})
			switch tc.state {
			case stateSelectBackend:
				// Force the reported ordering: catalog first, account default later.
				_, _ = h.Update(backendCatalogMsg{naming: naming, catalog: twoUsableBackends()})
			case stateSelectProgram:
				_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyTab})
			case stateSelectAccount:
				_, _ = h.Update(accountRegistryMsg{naming: naming, agent: "claude", resp: resp})
			case statePromptInput:
				_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyShiftTab})
			}
			require.Equal(t, tc.state, h.state)

			_, _ = h.Update(accountDefaultMsg{naming: naming, agent: "claude", resp: resp})
			assert.Equal(t, tc.state, h.state, "preselection must not close the field being edited")
			_, _ = h.handleKeyPress(tea.KeyMsg{Type: tc.close})

			require.Equal(t, stateNew, h.state)
			require.Equal(t, "work", h.pendingAccount, "the form must retain the project's configured account")
			assert.Contains(t, h.menu.String(), "account ✓", "the naming form must show that an account is set")

			// Reopening the account field must visibly select that same default.
			_, _ = h.Update(accountRegistryMsg{naming: naming, agent: "claude", resp: resp})
			require.Equal(t, stateSelectAccount, h.state)
			selected := h.selectionOverlay.GetSelectedIndex()
			assert.Equal(t, "work", h.accountPickerChoices[selected].value)
		})
	}
}
