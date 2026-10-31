package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
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

// A remote target resolves the selector in the DAEMON's registry and writes
// there; this machine's registry and local daemon are never touched.
func TestProjectsColorFollowsTheRemoteTarget(t *testing.T) {
	// A local registry holding a same-path project must stay unread.
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(t.TempDir(), "af-home"))
	local := setProjectColorViaDaemon
	setProjectColorViaDaemon = routedSetProjectColor
	t.Cleanup(func() { setProjectColorViaDaemon = local })

	var mu sync.Mutex
	hits := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits[r.URL.Path] = body
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/ListProjects":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ListProjectsResponse{Projects: []config.Project{
				{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha"}}}))
		case "/v1/SetProjectColor":
			var req daemon.SetProjectColorRequest
			_ = json.Unmarshal(body, &req)
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SetProjectColorResponse{
				Project: config.Project{ID: req.ID, Color: req.Color}}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })

	color := findSubcommand(t, "color")
	// A path on the daemon's host resolves through the daemon's list.
	_ = captureJSON(t, func() error { return color.RunE(color, []string{"/srv/box/alpha", "#16a34a"}) })
	mu.Lock()
	require.JSONEq(t, `{"id":"prj_0123456789abcdef0123456789abcdef","color":"#16a34a"}`, string(hits["/v1/SetProjectColor"]))
	mu.Unlock()

	// An unknown path and a ~ path are refused without a write.
	for _, sel := range []string{"/srv/box/nope", "~/alpha", "relative/alpha"} {
		mu.Lock()
		delete(hits, "/v1/SetProjectColor")
		mu.Unlock()
		require.Error(t, color.RunE(color, []string{sel, "#16a34a"}), sel)
		mu.Lock()
		require.NotContains(t, hits, "/v1/SetProjectColor", sel)
		mu.Unlock()
	}

	// The local registry was never created or read.
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr), "remote color must not touch the local registry")
}
