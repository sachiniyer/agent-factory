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
	require.Equal(t, "VS Code (web UI)", newTabChoices[1].label)
	help := xansi.Strip((helpTypeGeneral{}).toContent())
	require.Contains(t, help, "Hand off")
	require.Contains(t, help, "Search sessions")
}
