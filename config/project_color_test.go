package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeProjectColor(t *testing.T) {
	for in, want := range map[string]string{
		"":         "",
		"  ":       "",
		"#2F5FD8":  "#2f5fd8",
		"2f5fd8":   "#2f5fd8",
		"#abc":     "#aabbcc",
		" #A1b2C3": "#a1b2c3",
	} {
		got, err := NormalizeProjectColor(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"red", "#12345", "#1234567", "#ggg", "rgb(1,2,3)"} {
		_, err := NormalizeProjectColor(in)
		require.Error(t, err, in)
	}
}

func TestSetProjectColorRoundTripsThroughListAndClears(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	root := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	project, err := RegisterProject(root)
	require.NoError(t, err)
	require.Empty(t, project.Color)

	updated, err := SetProjectColor(project.ID, "#E11D48")
	require.NoError(t, err)
	require.Equal(t, "#e11d48", updated.Color)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.Equal(t, "#e11d48", projects[0].Color)

	rebound, err := RegisterProject(root)
	require.NoError(t, err)
	require.Equal(t, "#e11d48", rebound.Color, "re-registering keeps the color")

	cleared, err := SetProjectColor(project.ID, "")
	require.NoError(t, err)
	require.Empty(t, cleared.Color)
	projects, err = ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects[0].Color)
}

func TestSetProjectColorRejectsUnknownProjectAndBadColor(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	root := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	project, err := RegisterProject(root)
	require.NoError(t, err)

	_, err = SetProjectColor("prj_00000000000000000000000000000000", "#ffffff")
	require.ErrorContains(t, err, "not registered")
	_, err = SetProjectColor(project.ID, "blue")
	require.ErrorContains(t, err, "invalid project color")
}
