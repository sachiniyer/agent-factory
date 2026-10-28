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

func TestProjectDefaultUpdatesOpenAccountPickerSubmission(t *testing.T) {
	h := newTestHome(t)
	got := recordStartRequest(t)
	naming := startNaming(t, h, "default-before-account-enter")
	resp := withDefaults(map[string]string{"claude": "work"})
	_, _ = h.Update(accountRegistryMsg{naming: naming, agent: "claude", resp: resp})
	require.Equal(t, stateSelectAccount, h.state)
	require.Equal(t, ambientAccount, h.accountPickerChoices[h.selectionOverlay.GetSelectedIndex()].value)

	_, _ = h.Update(accountDefaultMsg{naming: naming, agent: "claude", resp: resp})
	assert.Equal(t, "work", h.accountPickerChoices[h.selectionOverlay.GetSelectedIndex()].value,
		"the open picker must visibly select the arriving default")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state)
	assert.True(t, h.pendingAccountChosen)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "default-before-account-enter", got.Title, "the form must issue a create request")
	assert.Equal(t, "work", got.Account, "Enter without moving must submit the visible project default")
}

func TestProjectDefaultLeavesOpenAccountSelectionWhenNotApplicable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preselect string
		chosen    bool
	}{
		{"absent row", "missing", false},
		{"already chosen", "work", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			naming := startNaming(t, h, "keep-account-selection")
			h.pendingAccount = "personal"
			h.pendingAccountChosen = tc.chosen
			_, _ = h.Update(accountRegistryMsg{naming: naming, agent: "claude", resp: twoAgentsWithAccounts()})
			require.Equal(t, stateSelectAccount, h.state)
			selected := h.selectionOverlay.GetSelectedIndex()
			_, _ = h.Update(accountDefaultMsg{naming: naming, agent: "claude",
				resp: withDefaults(map[string]string{"claude": tc.preselect})})
			assert.Equal(t, selected, h.selectionOverlay.GetSelectedIndex())
			if tc.chosen {
				assert.Equal(t, "personal", h.pendingAccount, "the user's decision must survive the late default")
			}
		})
	}
}
