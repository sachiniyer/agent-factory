package ui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountsSelectionIdentitySurvivesInsertion(t *testing.T) {
	for _, selected := range []AccountRow{
		{Agent: "codex", Name: "work"},
		{Agent: "codex", Register: true},
		{Agent: "gemini", Register: true},
	} {
		t.Run(selected.Agent+"/"+selected.Name, func(t *testing.T) {
			pane := accountsPane(t, []AccountRow{{Agent: "codex", Name: "work"}}, []string{"codex", "gemini"})
			selectAccount(t, pane, selected.Agent, selected.Name)
			pane.SetAccounts([]AccountRow{{Agent: "codex", Name: "new"}, {Agent: "codex", Name: "work"}}, []string{"codex", "gemini"}, nil)
			require.Equal(t, &selected, pane.selectedAccount(), "the selection follows account identity, not its previous index")
		})
	}
}

func TestAccountsRefreshPreservesOpenRegisterField(t *testing.T) {
	pane := accountsPane(t, nil, []string{"codex", "gemini"})
	selectAccount(t, pane, "gemini", "")
	pane.HandleKeyPress(accountKey("enter"))
	pane.HandleKeyPress(accountKey("my-account"))
	pane.SetAccounts([]AccountRow{{Agent: "codex", Name: "new"}}, []string{"codex", "gemini"}, nil)
	require.Equal(t, &AccountRow{Agent: "gemini", Register: true}, pane.selectedAccount())
	require.True(t, pane.IsEditing())
	require.Contains(t, pane.String(), "my-account", "the active name field must remain visible after rows move")
	pane.HandleKeyPress(accountKey("enter"))
	require.Equal(t, AccountRequest{Kind: AccountRequestRegister, Agent: "gemini", Name: "my-account"}, pane.TakeAccountRequest())
}
