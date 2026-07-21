package api

import (
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
