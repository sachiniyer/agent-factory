package api

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"

	"github.com/stretchr/testify/require"
)

// TestProjectsDeleteJSONReportsDeregistration guards #2645's remaining CLI
// contract: the daemon already reports whether it removed the durable project
// record, so the command must not hide that outcome from JSON callers.
func TestProjectsDeleteJSONReportsDeregistration(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "moved-project")

	original := deleteProjectViaDaemon
	deleteProjectViaDaemon = func(daemon.DeleteProjectRequest) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, Deregistered: true}, nil
	}
	t.Cleanup(func() { deleteProjectViaDaemon = original })

	out := captureStdout(t, func() {
		require.NoError(t, projectsDeleteCmd.RunE(projectsDeleteCmd, []string{target}))
	})
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	require.Equal(t, true, payload["deregistered"],
		"projects delete JSON must expose the daemon's durable-registration outcome")
}

// TestProjectsDeleteJSONReportsUnrestorableCount guards the #4407 change
// request: project deletion preserves a row whose title claims the reserved
// root name, but restore refuses it. The daemon reports that row apart from
// the restorable archived_count, and the command must pass the number through
// rather than let archived_count read as "everything can come back".
func TestProjectsDeleteJSONReportsUnrestorableCount(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "legacy-project")

	for _, tc := range []struct {
		name         string
		unrestorable int
	}{
		{name: "reserved row preserved", unrestorable: 1},
		{name: "nothing unrestorable", unrestorable: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := deleteProjectViaDaemon
			deleteProjectViaDaemon = func(daemon.DeleteProjectRequest) (daemon.DeleteProjectResponse, error) {
				return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 2, UnrestorableCount: tc.unrestorable}, nil
			}
			t.Cleanup(func() { deleteProjectViaDaemon = original })

			out := captureStdout(t, func() {
				require.NoError(t, projectsDeleteCmd.RunE(projectsDeleteCmd, []string{target}))
			})
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &payload))
			require.Equal(t, float64(2), payload["archived_count"])
			require.Contains(t, payload, "unrestorable_count",
				"the key is always present so a script can read it without a default")
			require.Equal(t, float64(tc.unrestorable), payload["unrestorable_count"],
				"projects delete JSON must report the sessions it preserved but cannot restore")
		})
	}
}
