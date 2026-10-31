package tree

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// W8.8: an off-box snapshot row is never "started" on the TUI side, but it must
// still carry `branch (repo)` in a multi-repo rail.
func TestSandboxRowShowsBranchAndRepoInMultiRepoRail(t *testing.T) {
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: "sbx", Title: "sandboxed", Path: "/srv/projects/widgets", Branch: "feature-x",
		Program: "claude", BackendType: "docker", Liveness: session.LiveReady,
	})
	require.NoError(t, err)
	require.False(t, inst.Started(), "premise: the snapshot row is inert")

	r := NewInstanceRenderer()
	r.SetWidth(80)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, true, false), "")
	require.Contains(t, out, "feature-x (widgets)")

	single := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	require.Contains(t, single, "feature-x")
	require.NotContains(t, single, "(widgets)")
}
