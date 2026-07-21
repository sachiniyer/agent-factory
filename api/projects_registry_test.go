package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProjectsRegistryCommandsRegistered is the production-wiring regression
// for #2216's registry slice. A registry package exercised only by its own unit
// tests would not make stable project identity reachable by users; these are
// the actual Cobra commands wired into `af projects` at startup.
func TestProjectsRegistryCommandsRegistered(t *testing.T) {
	want := map[string]bool{
		"list":     false,
		"register": false,
		"rebind":   false,
	}
	for _, cmd := range ProjectsCmd.Commands() {
		if _, ok := want[cmd.Name()]; ok {
			want[cmd.Name()] = true
		}
	}
	for name, registered := range want {
		require.Truef(t, registered, "af projects %s is not wired into the production command", name)
	}
}

func TestProjectsListMissingHomeIsReadOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	out := captureJSON(t, func() error { return projectsListCmd.RunE(projectsListCmd, nil) })
	var projects []map[string]any
	require.NoError(t, json.Unmarshal(out, &projects))
	require.Empty(t, projects)
	_, err := os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist, "the production list command must not create config or log files")
}
