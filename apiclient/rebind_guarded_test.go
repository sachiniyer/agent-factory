package apiclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// Tests for #4822's mixed-version rule on the HTTP transport: a guarded
// rebind is a compare-and-set, and a daemon built before the precondition
// existed would ignore expected_root/expected_checkout_id and apply the move
// unconditionally — last writer wins wearing the CAS's spelling. The client
// must refuse BEFORE the mutation goes out, pointing at the upgrade /
// rebind-from-the-daemon-host remedy, and must never fall back to an
// unguarded send.

// guardedRebindServer serves the client's Ping probe with the given response
// and counts whether the mutation route was ever touched.
func guardedRebindServer(t *testing.T, health http.HandlerFunc) (*httptest.Server, *bool) {
	t.Helper()
	sent := false
	mux := http.NewServeMux()
	if health != nil {
		mux.HandleFunc("/v1/health", health)
	}
	mux.HandleFunc("/v1/RebindProject", func(w http.ResponseWriter, r *http.Request) {
		sent = true
		var req daemon.RebindProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RebindProjectResponse{
			Project: config.Project{ID: req.ID, Root: req.Path},
		}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestRebindProjectRefusesGuardedSendWhenDaemonPredatesTheCapability(t *testing.T) {
	srv, sent := guardedRebindServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// A pre-upgrade daemon answers Ping but carries no guarded_rebind bit.
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{Version: "0.9.0"}))
	})
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.Error(t, err)
	require.True(t, errors.Is(err, daemon.ErrGuardedRebindUnsupported),
		"an unadvertised capability must refuse the guarded send, got %v", err)
	assert.False(t, *sent, "the refusal happens BEFORE the mutation goes out")
	assert.Contains(t, err.Error(), "daemon host")
}

func TestRebindProjectRefusesGuardedSendWhenTheProbeFails(t *testing.T) {
	// No /v1/health route at all — the probe cannot establish the capability,
	// so the guarded send must not go out either.
	srv, sent := guardedRebindServer(t, nil)
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.Error(t, err)
	require.True(t, errors.Is(err, daemon.ErrGuardedRebindUnsupported),
		"a capability check that cannot answer is still fail-closed, got %v", err)
	assert.False(t, *sent, "the refusal happens BEFORE the mutation goes out")
}

func TestRebindProjectCarriesTheGuardedPairOnTheWire(t *testing.T) {
	var gotReq daemon.RebindProjectRequest
	sent := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{Version: "9.9.9", GuardedRebind: true}))
	})
	mux.HandleFunc("/v1/RebindProject", func(w http.ResponseWriter, r *http.Request) {
		sent = true
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RebindProjectResponse{
			Project: config.Project{ID: gotReq.ID, Root: gotReq.Path},
		}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.NoError(t, err)
	require.True(t, sent, "a daemon that advertises the capability receives the send")
	assert.Equal(t, "/old", gotReq.ExpectedRoot)
	assert.Equal(t, "chk-observed", gotReq.ExpectedCheckoutID,
		"both halves of the observed pair ride the wire")
}

func TestRebindProjectUnguardedSendDoesNotPing(t *testing.T) {
	pinged := false
	sent := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		pinged = true
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{Version: "9.9.9"}))
	})
	mux.HandleFunc("/v1/RebindProject", func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RebindProjectResponse{
			Project: config.Project{ID: "prj", Root: "/new"},
		}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "", "", "/new")
	require.NoError(t, err)
	assert.True(t, sent, "the unguarded send went out")
	assert.False(t, pinged,
		"an unguarded rebind pays no capability probe — master's CLI keeps working on any vintage")
}
