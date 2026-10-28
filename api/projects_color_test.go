package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

func TestProjectsColorSendsAnIDVerbatimPastABrokenRegistry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(t.TempDir(), "af-home"))
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	broken := filepath.Join(dir, "prj_11111111111111111111111111111111")
	require.NoError(t, os.MkdirAll(broken, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(broken, "project.json"), []byte("{"), 0o644))
	_, err = config.ListProjects()
	require.Error(t, err, "the strict registry read fails on the broken record")

	var got daemon.SetProjectColorRequest
	restore := setProjectColorViaDaemon
	setProjectColorViaDaemon = func(req daemon.SetProjectColorRequest) (config.Project, error) {
		got = req
		return config.Project{ID: req.ID, Color: req.Color}, nil
	}
	t.Cleanup(func() { setProjectColorViaDaemon = restore })

	color := findSubcommand(t, "color")
	_ = captureJSON(t, func() error {
		return color.RunE(color, []string{"prj_0123456789abcdef0123456789abcdef", "#16a34a"})
	})
	require.Equal(t, daemon.SetProjectColorRequest{ID: "prj_0123456789abcdef0123456789abcdef", Color: "#16a34a"}, got)
}
