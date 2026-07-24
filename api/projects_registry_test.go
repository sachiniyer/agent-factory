package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// TestProjectsRegistryCommandsRegistered is the production-wiring regression
// for #2216's registry slice, updated for #2456's rename of `register` to `add`.
// A registry package exercised only by its own unit tests would not make stable
// project identity reachable by users; these are the actual Cobra commands wired
// into `af projects` at startup.
func TestProjectsRegistryCommandsRegistered(t *testing.T) {
	want := map[string]bool{
		"list":   false,
		"add":    false,
		"rebind": false,
	}
	for _, cmd := range ProjectsCmd.Commands() {
		if _, ok := want[cmd.Name()]; ok {
			want[cmd.Name()] = true
		}
	}
	for name, registered := range want {
		require.Truef(t, registered, "af projects %s is not wired into the production command", name)
	}
	require.Contains(t, findSubcommand(t, "add").Aliases, "register",
		"`register` must remain a working alias for `add` so preview-build users are not broken (#2456)")
}

// TestProjectsAddRoutesThroughDaemon proves `af projects add` performs the
// registration through the daemon RegisterProject RPC — the single-writer path
// (#2456), not an in-process config write — forwarding the raw path (the daemon
// resolves it on its own filesystem) and printing the returned project.
func TestProjectsAddRoutesThroughDaemon(t *testing.T) {
	var gotReq daemon.RegisterProjectRequest
	restore := registerProjectViaDaemon
	registerProjectViaDaemon = func(req daemon.RegisterProjectRequest) (config.Project, error) {
		gotReq = req
		return config.Project{ID: "prj_00000000000000000000000000000000", Root: "/resolved/root"}, nil
	}
	t.Cleanup(func() { registerProjectViaDaemon = restore })

	add := findSubcommand(t, "add")
	out := captureJSON(t, func() error { return add.RunE(add, []string{"~/some/path"}) })
	require.Equal(t, "~/some/path", gotReq.Path,
		"the raw path is forwarded to the daemon, which resolves it on its own filesystem")

	var project config.Project
	require.NoError(t, json.Unmarshal(out, &project))
	require.Equal(t, "/resolved/root", project.Root, "the resolved project is printed back")
}

// TestProjectsAddSurfacesDaemonError: a daemon-side rejection (not a git repo,
// daemon warming) is surfaced to the CLI as an error, not swallowed.
func TestProjectsAddSurfacesDaemonError(t *testing.T) {
	restore := registerProjectViaDaemon
	registerProjectViaDaemon = func(daemon.RegisterProjectRequest) (config.Project, error) {
		return config.Project{}, errors.New("path is not inside a git checkout")
	}
	t.Cleanup(func() { registerProjectViaDaemon = restore })

	add := findSubcommand(t, "add")
	require.Error(t, add.RunE(add, []string{"/tmp/not-a-repo"}),
		"a daemon rejection must surface as a CLI error")
}

func findSubcommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, cmd := range ProjectsCmd.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	t.Fatalf("af projects %s not found", name)
	return nil
}

func TestProjectsListMissingHomeIsReadOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	out := captureJSON(t, func() error { return projectsListCmd.RunE(projectsListCmd, nil) })
	var projects []map[string]any
	require.NoError(t, json.Unmarshal(out, &projects))
	require.Empty(t, projects)
	_, err := os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist, "the production list command must not create config or log files")
}
