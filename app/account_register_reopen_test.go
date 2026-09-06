package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func openAccountPane(t *testing.T, h *home) tea.Cmd {
	t.Helper()
	_, cmd := h.showConfigEditor()
	require.NotNil(t, cmd)
	sizeConfigPane(h)
	require.True(t, h.configPane.HasFocus())
	return cmd
}

func accountReopenList(name string) daemon.ListAccountsResponse {
	return daemon.ListAccountsResponse{
		Entries: []daemon.AccountEntry{{Agent: "codex", Name: name}},
		Agents:  []string{"codex"},
	}
}

func TestAccountRegisterPendingSurvivesReopen(t *testing.T) {
	h := remoteAccountLoadHome(t)
	registerCalls := 0
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return accountReopenList("existing"), nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			registerCalls++
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	h.Update(openAccountPane(t, h)())
	pending := h.handleAccountRegister("codex", "fresh")
	require.NotNil(t, pending)
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	reopened := openAccountPane(t, h)
	require.True(t, h.configPane.AccountsBusy(), "the pending mutation survives closing the overlay")
	require.Contains(t, h.configPane.String(), `Registering codex account "fresh"…`)
	h.Update(reopened())
	require.True(t, h.configPane.AccountsBusy(), "the initial read must not release a pending mutation")
	require.Contains(t, h.configPane.String(), `Registering codex account "fresh"…`)
	accountPaneLastRow(h)
	for i := 0; i < 2; i++ {
		_, cmd := h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyEnter})
		require.Nil(t, cmd, "Enter must not submit an overlapping registration")
		require.False(t, h.configPane.IsEditing(), "Enter must not reopen the registration field")
	}
	require.Nil(t, h.handleAccountRegister("codex", "second"), "the home model also gates direct registration requests")
	require.Zero(t, registerCalls)
	h.Update(pending())
	require.Equal(t, 1, registerCalls)
}

func TestAccountRegisterReopenCompletionReconcilesFreshList(t *testing.T) {
	h := remoteAccountLoadHome(t)
	listed := "existing"
	listCalls := 0
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			listCalls++
			return accountReopenList(listed), nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	h.Update(openAccountPane(t, h)())
	pending := h.handleAccountRegister("codex", "fresh")
	require.NotNil(t, pending)
	listed = "stale-register-snapshot"
	completion := pending()
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	reopened := openAccountPane(t, h)
	listed = "stale-reopen-snapshot"
	oldLoad := reopened()
	h.Update(oldLoad)
	h.configPane.SetAccountStatus("Current operator feedback", false)
	_, refresh := h.Update(completion)
	require.NotNil(t, refresh, "a mutation from a previous opening needs a fresh read")
	require.False(t, h.configPane.AccountsBusy())
	require.Contains(t, h.configPane.String(), "Current operator feedback")
	require.NotContains(t, h.configPane.String(), "stale-register-snapshot")
	before := h.configPane.String()
	listed = "fresh"
	loaded := refresh()
	require.Equal(t, before, h.configPane.String(), "the reconciliation worker must not mutate the pane")
	h.Update(loaded)
	require.Equal(t, 4, listCalls, "reconciliation must fetch again after the registration completion")
	require.Contains(t, h.configPane.String(), "fresh")
	require.NotContains(t, h.configPane.String(), "stale-register-snapshot")
	require.NotContains(t, h.configPane.String(), "stale-reopen-snapshot")
	require.Contains(t, h.configPane.String(), "Current operator feedback")
	current := h.configPane.String()
	h.Update(oldLoad)
	require.Equal(t, current, h.configPane.String(), "a late initial read must not replace reconciliation's newer list")
	require.NotNil(t, h.handleAccountRegister("codex", "next"), "completion releases the home-level registration gate")
}

func TestAccountRegisterClosedCompletionReleasesPendingMutation(t *testing.T) {
	h := remoteAccountLoadHome(t)
	t.Cleanup(SetAccountSeamsForTest(
		func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
			return accountReopenList("existing"), nil
		},
		func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
			return registeredAccountResponse(req.Name), nil
		}, nil,
	))
	h.Update(openAccountPane(t, h)())
	pending := h.handleAccountRegister("codex", "fresh")
	require.NotNil(t, pending)
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, h.handleAccountRegister("codex", "second"), "closing must not release the home-level mutation gate")
	closed := h.configPane.String()
	_, cmd := h.Update(pending())
	require.Nil(t, cmd, "closed panes do not need reconciliation")
	require.Equal(t, closed, h.configPane.String())
	require.False(t, h.configPane.HasFocus())
	h.Update(openAccountPane(t, h)())
	require.False(t, h.configPane.AccountsBusy(), "the closed-pane completion releases the pending flag")
	require.NotContains(t, h.configPane.String(), `Registering codex account "fresh"…`)
	require.NotNil(t, h.handleAccountRegister("codex", "next"))
}

func TestAccountRegisterReopenFailurePreservesNewerStatus(t *testing.T) {
	for _, status := range []string{"empty", "pending", "newer"} {
		t.Run(status, func(t *testing.T) {
			h := remoteAccountLoadHome(t)
			t.Cleanup(SetAccountSeamsForTest(
				func(daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
					return accountReopenList("existing"), nil
				},
				func(daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
					return daemon.RegisterAccountResponse{}, errors.New("registration of fresh failed")
				}, nil,
			))
			h.Update(openAccountPane(t, h)())
			pending := h.handleAccountRegister("codex", "fresh")
			require.NotNil(t, pending)
			h.Update(tea.KeyMsg{Type: tea.KeyEsc})
			h.Update(openAccountPane(t, h)())
			require.True(t, h.configPane.AccountsBusy(), "the reopened pane remains busy until failure arrives")
			switch status {
			case "empty":
				h.configPane.SetAccountStatus("", false)
			case "newer":
				h.configPane.SetAccountStatus("Newer feedback", false)
			}
			_, cmd := h.Update(pending())
			require.Nil(t, cmd, "a failed mutation does not need reconciliation")
			require.False(t, h.configPane.AccountsBusy())
			if status == "newer" {
				require.Contains(t, h.configPane.String(), "Newer feedback")
				require.NotContains(t, h.configPane.String(), "registration of fresh failed")
			} else {
				require.Contains(t, h.configPane.String(), "registration of fresh failed")
			}
			require.NotNil(t, h.handleAccountRegister("codex", "next"), "failure also releases the home-level gate")
		})
	}
}
