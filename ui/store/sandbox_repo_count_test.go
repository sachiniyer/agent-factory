package store

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// W8.8: inert off-box rows count toward the multi-repo indicator, symmetrically
// on add and remove.
func TestSandboxRowsCountTowardRepos(t *testing.T) {
	mk := func(title, path string) *session.Instance {
		inst, err := session.FromInstanceData(session.InstanceData{
			ID: title, Title: title, Path: path, Program: "claude", BackendType: "docker", Liveness: session.LiveReady,
		})
		require.NoError(t, err)
		return inst
	}
	p := NewProjection()
	a, b := mk("a", "/srv/one"), mk("b", "/srv/two")
	p.AddInstance(a)()
	p.AddInstance(b)()
	require.Equal(t, 2, p.NumRepos())
	require.True(t, p.RemoveInstance(b))
	require.Equal(t, 1, p.NumRepos())
}
