package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlacement_LocalAndRemoteProjection(t *testing.T) {
	local := &Instance{Title: "l", backend: NewFakeBackend()}
	d := local.ToInstanceData()
	require.NotNil(t, d.Placement)
	require.Equal(t, PlacementLocal, d.Placement.Kind)
	require.Equal(t, "", d.Placement.Short())
	require.Equal(t, "local", d.Placement.Detail())

	remote := &Instance{Title: "r", Host: "box1", backend: &failingStartBackend{FakeBackend: NewFakeBackend(), typ: "sandbox"}}
	d = remote.ToInstanceData()
	require.Equal(t, &PlacementData{Kind: PlacementRemote, Host: "box1", Backend: "sandbox"}, d.Placement)
	require.Equal(t, "box1", d.Placement.Short())
	require.Equal(t, "box1 · sandbox", d.Placement.Detail())

	// Archived: the preserved directory is the location.
	remote.archivedWorkspace = &ArchivedWorkspaceData{Host: "box1", Dir: "/srv/af/s1"}
	require.Equal(t, "/srv/af/s1/workspace", remote.ToInstanceData().Placement.Location)

	// Derived, never persisted.
	require.Nil(t, d.ForStorage().Placement)
}

func TestPlacement_ClientRowAdoptsSnapshot(t *testing.T) {
	row := &Instance{Title: "c", backend: &failingStartBackend{FakeBackend: NewFakeBackend(), typ: "sandbox"}}
	p := &PlacementData{Kind: PlacementRemote, Host: "h", Backend: "ssh", Location: "/w/workspace"}
	require.True(t, row.ReconcilePlacementSnapshot(p))
	require.False(t, row.ReconcilePlacementSnapshot(p), "identical placement is not a change")
	require.False(t, row.ReconcilePlacementSnapshot(nil), "an older daemon leaves the row alone")
	require.Equal(t, p, row.Placement())
	require.Equal(t, "h · ssh · /w/workspace", row.Placement().Detail())
}
