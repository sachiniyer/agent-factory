package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/require"
)

func accountPaneLastRow(h *home) {
	for i := 0; i < 100; i++ {
		h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
}

func TestAccountRegisterPendingBlocksRegisterEditor(t *testing.T) {
	h := remoteAccountRegisterHome(t)
	accountPaneLastRow(h)
	_, cmd := h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	require.True(t, h.configPane.IsEditing())
	h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fresh")})
	_, pending := h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, pending)
	require.False(t, h.configPane.IsEditing())
	for i := 0; i < 2; i++ {
		_, cmd = h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
		require.Nil(t, cmd, "a pending mutation must not submit another registration")
		require.False(t, h.configPane.IsEditing(), "a pending mutation must not reopen the register editor")
		require.Contains(t, h.configPane.String(), `Registering codex account "fresh"…`)
	}
	require.Contains(t, h.configPane.String(), "┄", "busy register rows need the theme's dashed disabled cue")
}

func TestAccountRegisterCompletionPreservesMovedCursor(t *testing.T) {
	h := remoteAccountRegisterHome(t)
	h.configPane.SetAccounts([]ui.AccountRow{{Agent: "codex", Name: "existing"}}, []string{"codex", "gemini"}, nil)
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return daemon.ListAccountsResponse{
				Entries: []daemon.AccountEntry{{Agent: "codex", Name: "existing"}, {Agent: "codex", Name: "fresh"}},
				Agents:  []string{"codex", "gemini"},
			}, nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	accountPaneLastRow(h)
	h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyUp}) // codex register
	pending := h.handleAccountRegister("codex", "fresh")
	require.NotNil(t, pending)
	h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}) // gemini register
	h.Update(pending())
	h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, h.configPane.IsEditing(), "completion must release the busy state")
	h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("next")})
	h.configPane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ui.AccountRequest{Kind: ui.AccountRequestRegister, Agent: "gemini", Name: "next"}, h.configPane.TakeAccountRequest(),
		"refresh must retain the row the operator moved to, despite insertion above it")
}

func TestAccountRegisterPendingClearsOnCompletionOrClose(t *testing.T) {
	for _, outcome := range []string{"success", "error", "close"} {
		t.Run(outcome, func(t *testing.T) {
			h := remoteAccountRegisterHome(t)
			t.Cleanup(SetAccountSeamsForTest(
				func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
					return daemon.ListAccountsResponse{Agents: []string{"codex"}}, nil
				},
				func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
					if outcome == "error" {
						return daemon.RegisterAccountResponse{}, errors.New("registration unavailable")
					}
					return registeredAccountResponse(req.Name), nil
				}, nil,
			))
			accountPaneLastRow(h)
			pending := h.handleAccountRegister("codex", "fresh")
			h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
			require.False(t, h.configPane.IsEditing(), "registration is busy before completion")
			if outcome == "close" {
				h.Update(tea.KeyMsg{Type: tea.KeyEsc})
				require.False(t, h.configPane.HasFocus())
				h.configPane.SetFocus(true)
				h.state = stateConfigEditor
			} else {
				h.Update(pending())
			}
			accountPaneLastRow(h)
			h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
			require.True(t, h.configPane.IsEditing(), "completion or close must release the busy state")
		})
	}
}
