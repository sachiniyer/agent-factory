package ui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTaskProjectPathRemoteKeepsThePathAsTyped(t *testing.T) {
	prev := isRemoteTaskTarget
	isRemoteTaskTarget = func() bool { return true }
	t.Cleanup(func() { isRemoteTaskTarget = prev })
	t.Setenv("HOME", "/home/laptop")

	got, err := ResolveTaskProjectPath("  ~/src/alpha ")
	require.NoError(t, err)
	require.Equal(t, "~/src/alpha", got, "~ is the daemon host's, never expanded against this machine's home")

	got, err = ResolveTaskProjectPath("/srv/box/alpha")
	require.NoError(t, err)
	require.Equal(t, "/srv/box/alpha", got)

	_, err = ResolveTaskProjectPath("relative/alpha")
	require.ErrorContains(t, err, "must be absolute")

	isRemoteTaskTarget = func() bool { return false }
	got, err = ResolveTaskProjectPath("~/src/alpha")
	require.NoError(t, err)
	require.Equal(t, "/home/laptop/src/alpha", got, "control: the local form still expands ~ here")
}
