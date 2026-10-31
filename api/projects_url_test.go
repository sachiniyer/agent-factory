package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

func TestProjectsAddSendsAGitURLVerbatimAndAPathResolved(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(t.TempDir(), "af-home"))
	var got daemon.RegisterProjectRequest
	restore := registerProjectViaDaemon
	registerProjectViaDaemon = func(req daemon.RegisterProjectRequest) (config.Project, error) {
		got = req
		return config.Project{ID: "prj_x"}, nil
	}
	t.Cleanup(func() { registerProjectViaDaemon = restore })

	add := findSubcommand(t, "add")
	for _, url := range []string{"https://github.com/owner/repo.git", "git@github.com:owner/repo.git", "ssh://git@h/o/r"} {
		_ = captureJSON(t, func() error { return add.RunE(add, []string{url}) })
		require.Equal(t, daemon.RegisterProjectRequest{URL: url}, got, "a URL is never resolved as a path on this machine")
	}

	dir := t.TempDir()
	_ = captureJSON(t, func() error { return add.RunE(add, []string{dir}) })
	require.Equal(t, daemon.RegisterProjectRequest{Path: dir}, got, "control: a path still goes as a path")
}

// Against a remote target the URL crosses the wire for the DAEMON to clone; the
// client neither clones nor validates against its own filesystem.
func TestRoutedRegisterProjectSendsTheURLToTheRemoteDaemon(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		var req daemon.RegisterProjectRequest
		_ = json.Unmarshal(b, &req)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RegisterProjectResponse{OK: true,
			Project: config.Project{ID: "prj_x", Root: "/srv/box/projects/repo"}}))
	}))
	defer srv.Close()
	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(t.TempDir(), "af-home"))

	p, err := routedRegisterProject(daemon.RegisterProjectRequest{URL: "https://github.com/owner/repo.git"})
	require.NoError(t, err)
	require.Equal(t, "/srv/box/projects/repo", p.Root)
	mu.Lock()
	require.JSONEq(t, `{"path":"","url":"https://github.com/owner/repo.git"}`, string(body))
	mu.Unlock()
}
