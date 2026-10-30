package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// W9.1: the session lifecycle verbs reach the targeted daemon and never the
// local control socket (which would also spawn a local daemon).
func TestRoutedLifecycleVerbsReachRemoteTarget(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		mu.Lock()
		hits[r.URL.Path] = raw
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/ArchiveSession":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ArchiveSessionResponse{ArchivedPath: "/srv/box/arch"}))
		case "/v1/SendPrompt":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SendPromptResponse{OK: true, Status: session.PromptDelivered}))
		case "/v1/ResumeFromLimit":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ResumeFromLimitResponse{OK: true}))
		default:
			_ = apiproto.WriteEnvelope(w, apiproto.Success(map[string]any{}))
		}
	}))
	defer srv.Close()

	oldURL, oldTok := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = srv.URL, "tok"
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = oldURL, oldTok })
	t.Setenv("AF_HOME", t.TempDir())

	_, killErr := routedKillSession(daemon.KillSessionRequest{Title: "a"})
	require.NoError(t, killErr)
	p, err := routedArchiveSession(daemon.ArchiveSessionRequest{Title: "a"})
	require.NoError(t, err)
	require.Equal(t, "/srv/box/arch", p)
	st, err := routedSendPrompt(daemon.SendPromptRequest{Title: "a", Prompt: "hi"})
	require.NoError(t, err)
	require.Equal(t, session.PromptDelivered, st)
	require.NoError(t, routedResumeFromLimit(daemon.ResumeFromLimitRequest{Title: "a"}))

	mu.Lock()
	defer mu.Unlock()
	for _, route := range []string{"KillSession", "ArchiveSession", "SendPrompt", "ResumeFromLimit"} {
		require.Contains(t, hits, "/v1/"+route)
	}
}

// An uncertain outcome is reported as uncertain and is not re-sent.
func TestRoutedMutationUncertainOutcomeIsNamed(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close() // reply lost after the request arrived
	}))
	defer srv.Close()
	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })

	err := routedResumeFromLimit(daemon.ResumeFromLimitRequest{Title: "a"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outcome")
	require.Contains(t, err.Error(), "not re-sent")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, n)
}

func TestBackendsRepoPathRemoteRequiresRepoAsTyped(t *testing.T) {
	oldURL, oldRepo := apiclient.FlagDaemonURL, repoFlag
	apiclient.FlagDaemonURL = "http://box:8443"
	t.Cleanup(func() { apiclient.FlagDaemonURL, repoFlag = oldURL, oldRepo })

	repoFlag = ""
	_, err := backendsRepoPath()
	require.ErrorContains(t, err, "--repo is required")

	repoFlag = "~/src/proj"
	got, err := backendsRepoPath()
	require.NoError(t, err)
	require.Equal(t, "~/src/proj", got)
}

// W9.2: a remote create sends the daemon-host path exactly as typed and runs no
// client-side disk resolution; --repo is required.
func TestRemoteCreateInputsSendsPathAsTyped(t *testing.T) {
	oldURL, oldRepo, oldProg := apiclient.FlagDaemonURL, repoFlag, createProgramFlag
	apiclient.FlagDaemonURL = "http://box:8443"
	t.Cleanup(func() { apiclient.FlagDaemonURL, repoFlag, createProgramFlag = oldURL, oldRepo, oldProg })

	repoFlag = ""
	_, _, err := remoteCreateInputs(false)
	require.ErrorContains(t, err, "--repo is required")

	repoFlag, createProgramFlag = "~/srv/does/not/exist/here", ""
	ws, prog, err := remoteCreateInputs(false)
	require.NoError(t, err)
	require.Equal(t, "~/srv/does/not/exist/here", ws)
	require.Empty(t, prog, "empty program defers to the daemon's default_program")

	createProgramFlag = "not-an-agent"
	_, _, err = remoteCreateInputs(false)
	require.Error(t, err)
}

// #5743: a relative --repo would resolve against the daemon's cwd and could pick
// a different checkout, so it is refused rather than sent.
func TestRemoteCreateInputsRefusesRelativeRepo(t *testing.T) {
	oldURL, oldRepo := apiclient.FlagDaemonURL, repoFlag
	apiclient.FlagDaemonURL = "http://box:8443"
	t.Cleanup(func() { apiclient.FlagDaemonURL, repoFlag = oldURL, oldRepo })

	for _, rel := range []string{"src/project", "./project", "../project", "project"} {
		repoFlag = rel
		_, _, err := remoteCreateInputs(false)
		require.ErrorContains(t, err, "must be absolute", rel)
	}
	repoFlag = "/srv/box/project"
	ws, _, err := remoteCreateInputs(false)
	require.NoError(t, err)
	require.Equal(t, "/srv/box/project", ws)
}

// #5744: the remote create asks the daemon to validate before reserving.
func TestRoutedCreateSetsValidateFirst(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.CreateSessionResponse{}))
	}))
	defer srv.Close()
	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })

	_, err := routedCreateSession(daemon.CreateSessionRequest{Title: "a", RepoPath: "/srv/x"})
	require.NoError(t, err)
	require.Equal(t, true, got["validate_first"])
}

// W9.3: tab and project verbs reach the targeted daemon, send daemon-host paths
// as typed, and read the daemon's registry rather than this machine's.
func TestRoutedTabAndProjectVerbsReachRemoteTarget(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		mu.Lock()
		hits[r.URL.Path] = raw
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/CreateTab":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.CreateTabResponse{Name: "shell-1"}))
		case "/v1/ListProjects":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.ListProjectsResponse{
				Projects: []config.Project{{ID: "prj_remote", Root: "/srv/box/alpha"}}}))
		case "/v1/RegisterProject":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RegisterProjectResponse{
				OK: true, Project: config.Project{ID: "prj_remote", Root: "/srv/box/alpha"}}))
		default:
			_ = apiproto.WriteEnvelope(w, apiproto.Success(map[string]any{}))
		}
	}))
	defer srv.Close()

	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })
	// A seeded local registry must never be read for a remote list.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	created, err := routedCreateTab(daemon.CreateTabRequest{Title: "a", Shell: true})
	require.NoError(t, err)
	require.Equal(t, "shell-1", created.Name)

	projects, err := routedListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.Equal(t, "/srv/box/alpha", projects[0].Root, "the list is the daemon's registry")

	path, err := projectArgPath("~/src/alpha")
	require.NoError(t, err)
	require.Equal(t, "~/src/alpha", path, "sent as typed, never expanded against the caller's home")
	_, err = routedRegisterProject(daemon.RegisterProjectRequest{Path: path})
	require.NoError(t, err)

	_, err = projectArgPath("relative/alpha")
	require.ErrorContains(t, err, "must be absolute")

	_, _, err = resolveProjectDeleteTarget(nil)
	require.ErrorContains(t, err, "required against the daemon")
	req, name, err := resolveProjectDeleteTarget([]string{"/srv/box/alpha"})
	require.NoError(t, err)
	require.Equal(t, "/srv/box/alpha", req.RepoPath)
	require.Equal(t, "alpha", name)

	mu.Lock()
	defer mu.Unlock()
	for _, route := range []string{"CreateTab", "ListProjects", "RegisterProject"} {
		require.Contains(t, hits, "/v1/"+route)
	}
	require.JSONEq(t, `{"path":"~/src/alpha"}`, string(hits["/v1/RegisterProject"]))
}

// A remote rebind must put each value in its own field: the destination path in
// `path`, the guard in expected_root/expected_checkout_id (the apiclient method
// takes them in a different order than the request struct, and all are strings).
func TestRoutedRebindProjectKeepsPathAndGuardApart(t *testing.T) {
	var mu sync.Mutex
	got := map[string]map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got[r.URL.Path] = body
		mu.Unlock()
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RebindProjectResponse{OK: true,
			Project: config.Project{ID: "prj_x", Root: body["path"]}}))
	}))
	defer srv.Close()
	oldURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = srv.URL
	t.Cleanup(func() { apiclient.FlagDaemonURL = oldURL })

	p, err := routedRebindProject(daemon.RebindProjectRequest{ID: "prj_x", Path: "/srv/new"})
	require.NoError(t, err)
	require.Equal(t, "/srv/new", p.Root)
	mu.Lock()
	require.Equal(t, "/srv/new", got["/v1/RebindProject"]["path"])
	require.Equal(t, "prj_x", got["/v1/RebindProject"]["id"])
	mu.Unlock()

	_, err = routedRebindProject(daemon.RebindProjectRequest{
		ID: "prj_x", Path: "/srv/new", ExpectedRoot: "/srv/old", ExpectedCheckoutID: "chk1"})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	g := got["/v1/RebindProjectGuarded"]
	require.Equal(t, "/srv/new", g["path"])
	require.Equal(t, "/srv/old", g["expected_root"])
	require.Equal(t, "chk1", g["expected_checkout_id"])
}
