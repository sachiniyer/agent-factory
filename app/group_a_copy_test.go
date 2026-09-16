package app

import (
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestGroupACopyNamesExistingActionsAndDefaults(t *testing.T) {
	choices := accountChoicesFrom(daemon.ListAccountsResponse{Defaults: map[string]string{"claude": "work"}}, "claude")
	require.Equal(t, "Use configured default (work)", choices[0].label)
	require.Equal(t, ambientAccount, choices[0].value)
	ambient := accountChoicesFrom(daemon.ListAccountsResponse{}, "claude")
	require.Equal(t, "Use the agent's own login (nothing to route)", ambient[0].label)
	require.Equal(t, ambientAccount, ambient[0].value)
	// A present-but-empty default_accounts entry means ambient ON PURPOSE — the
	// create launches the agent's own login — so the routable row must not
	// promise a pool pick beside logged-in accounts (#4404 review).
	optedOut := accountChoicesFrom(daemon.ListAccountsResponse{
		Entries:        []daemon.AccountEntry{{Agent: "claude", Name: "work", LoggedIn: true}},
		AmbientOptOuts: map[string]bool{"claude": true},
	}, "claude")
	require.Equal(t, "Use the ambient identity (routing is off)", optedOut[0].label)
	require.Equal(t, "VS Code (web UI)", newTabChoices[1].label)
	help := xansi.Strip((helpTypeGeneral{}).toContent())
	require.Contains(t, help, "Hand off")
	require.Contains(t, help, "Search sessions")
}
