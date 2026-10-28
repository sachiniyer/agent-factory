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

	require.NoError(t, routedKillSession(daemon.KillSessionRequest{Title: "a"}))
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
