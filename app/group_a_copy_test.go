package app

import (
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestGroupACopyNamesExistingActionsAndDefaults(t *testing.T) {
	choices := accountChoicesFrom(daemon.ListAccountsResponse{Defaults: map[string]string{"claude": "work"}, PoolRouting: true}, "claude", true)
	require.Equal(t, "Use configured default (work)", choices[0].label)
	require.Equal(t, ambientAccount, choices[0].value)
	// Routed-but-nothing-to-route: the daemon has the router and an empty
	// claude registry, so the first row lands on the agent's own login and
	// says so (#4404 review).
	ambient := accountChoicesFrom(daemon.ListAccountsResponse{PoolRouting: true}, "claude", true)
	require.Equal(t, "Use the agent's own login (nothing to route)", ambient[0].label)
	require.Equal(t, ambientAccount, ambient[0].value)
	// Version skew, kept deliberately: a daemon that does not send
	// pool_routing predates the router, so its empty account IS the ambient
	// identity and the row must not promise a pick or carry a pin beside it.
	preRouter := accountChoicesFrom(daemon.ListAccountsResponse{}, "claude", true)
	require.Equal(t, "Use the agent's own login", preRouter[0].label)
	require.Len(t, preRouter, 1, "a pre-router daemon gets no ambient pin row")
	// A present-but-empty default_accounts entry means ambient ON PURPOSE — the
	// create launches the agent's own login — so the routable row must not
	// promise a pool pick beside logged-in accounts (#4404 review).
	optedOut := accountChoicesFrom(daemon.ListAccountsResponse{
		Entries:        []daemon.AccountEntry{{Agent: "claude", Name: "work", LoggedIn: true}},
		AmbientOptOuts: map[string]bool{"claude": true},
		PoolRouting:    true,
	}, "claude", true)
	require.Equal(t, "Use the ambient identity (routing is off)", optedOut[0].label)
	require.Equal(t, "VS Code (web UI)", newTabChoices[1].label)
	help := xansi.Strip((helpTypeGeneral{}).toContent())
	require.Contains(t, help, "Hand off")
	require.Contains(t, help, "Search sessions")
}
