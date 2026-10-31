package tree

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// W6.4: an off-box row names its host (or runtime type) in the rail; a local
// placement's Short() is empty, covered in the session package.
func TestRailRowShowsPlacementForOffBoxRowsOnly(t *testing.T) {
	remote, err := session.FromInstanceData(session.InstanceData{
		ID: "r", Title: "remote-one", Path: "/srv/p", Branch: "feat", Program: "claude",
		BackendType: "ssh", Host: "box1", Liveness: session.LiveReady,
		Placement: &session.PlacementData{Kind: session.PlacementRemote, Host: "box1", Backend: "ssh"},
	})
	require.NoError(t, err)
	r := NewInstanceRenderer()
	r.SetWidth(80)
	out := ansiEscape.ReplaceAllString(r.Render(remote, 1, true, false, false), "")
	require.Contains(t, out, "feat · box1")

	hostless, err := session.FromInstanceData(session.InstanceData{
		ID: "h", Title: "hostless", Path: "/srv/p", Branch: "feat", Program: "claude",
		BackendType: "docker", Liveness: session.LiveReady,
	})
	require.NoError(t, err)
	out = ansiEscape.ReplaceAllString(r.Render(hostless, 1, true, false, false), "")
	require.Contains(t, out, "feat · docker", "no listed host: the runtime type names the place")
}
