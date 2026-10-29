package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// RegisterProject by URL clones through the seam under projects_root, registers
// the checkout, and refuses a hostile URL before anything is cloned.
func TestControlServer_RegisterProject_ByGitURL(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	checkout := setupControlRepo(t)

	var gotRoot, gotURL string
	prev := cloneProject
	cloneProject = func(_ context.Context, root, url string) (string, error) {
		gotRoot, gotURL = root, url
		return checkout, nil
	}
	t.Cleanup(func() { cloneProject = prev })

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{URL: "https://github.com/owner/repo.git"}, &resp))
	require.Equal(t, filepath.Join(home, "projects"), gotRoot, "default projects_root is <af home>/projects")
	require.Equal(t, "https://github.com/owner/repo.git", gotURL)
	require.True(t, resp.OK)
	require.NotEmpty(t, resp.Project.ID)

	cloneProject = func(context.Context, string, string) (string, error) {
		t.Fatal("cloned a refused request")
		return "", nil
	}
	for _, req := range []RegisterProjectRequest{
		{URL: "file:///srv/secret/repo"},
		{URL: "/etc"},
		{URL: "ext::sh -c id"},
		{URL: "--upload-pack=id"},
		{URL: "https://u:pw@example.com/o/r.git"},
		{URL: "https://github.com/o/r.git", Path: "/srv/x"},
	} {
		var r RegisterProjectResponse
		require.Error(t, cs.RegisterProject(req, &r), "%+v", req)
		require.False(t, r.OK)
	}
}

// A config that exists but cannot be loaded refuses the registration instead of
// cloning into the default root the operator did not choose (#6095).
func TestProjectsRoot_BrokenConfigRefusesRatherThanUsingTheDefault(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("projects_root = [not toml"), 0o600))

	prev := cloneProject
	cloneProject = func(context.Context, string, string) (string, error) {
		t.Fatal("cloned although the config could not be read")
		return "", nil
	}
	t.Cleanup(func() { cloneProject = prev })

	_, err := cloneProjectForURL("https://github.com/owner/repo.git")
	require.Error(t, err)
	require.Contains(t, err.Error(), "config")
	require.NotContains(t, err.Error(), filepath.Join(home, "projects"))
}

// An absent config is not an error: the default root applies.
func TestProjectsRoot_AbsentConfigUsesTheDefault(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	root, err := projectsRoot()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "projects"), root)
}
