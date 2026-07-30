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
