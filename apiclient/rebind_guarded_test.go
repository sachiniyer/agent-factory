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

// Tests for #4822's mixed-version rule on the HTTP transport: a guarded rebind
// is a compare-and-set, and a daemon built before the precondition existed
// would ignore expected_root/expected_checkout_id and apply the move
// unconditionally — last writer wins wearing the CAS's spelling. The guarded
// ROUTE is the capability check (#4888 review): a daemon that predates
// /v1/RebindProjectGuarded answers its marked catch-all 404, which is
// definitive proof the precondition would be silently dropped — the client
// surfaces ErrGuardedRebindUnsupported instead of believing the guard ran. No
// Ping probe decides, because a probe is never affine with the mutation it
// predicts. An intermediary's unmarked 404 is NOT proof and stays
// UnconfirmedHTTPResponseError.

// rebindServer mounts a handler on each of the two rebind routes and records
// which one (if either) was reached.
func rebindServer(t *testing.T, guarded, unguarded http.HandlerFunc) (*httptest.Server, *string) {
	t.Helper()
	var reached string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/RebindProject", func(w http.ResponseWriter, r *http.Request) {
		reached = "RebindProject"
		unguarded(w, r)
	})
	mux.HandleFunc("/v1/RebindProjectGuarded", func(w http.ResponseWriter, r *http.Request) {
		reached = "RebindProjectGuarded"
		guarded(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &reached
}

func writeRebindOK(t *testing.T, w http.ResponseWriter, r *http.Request, got *daemon.RebindProjectRequest) {
	t.Helper()
	require.NoError(t, json.NewDecoder(r.Body).Decode(got))
	w.Header().Set("Content-Type", "application/json")
	_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.RebindProjectResponse{
		Project: config.Project{ID: got.ID, Root: got.Path},
	}))
}

func TestRebindProjectGuardedRoutesToTheGuardedRoute(t *testing.T) {
	var got daemon.RebindProjectRequest
	srv, reached := rebindServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRebindOK(t, w, r, &got)
	}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unguarded route must not see a guarded request")
	})
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.NoError(t, err)
	assert.Equal(t, "RebindProjectGuarded", *reached)
	assert.Equal(t, "/old", got.ExpectedRoot)
	assert.Equal(t, "chk-observed", got.ExpectedCheckoutID,
		"both halves of the observed pair ride the wire")
}

func TestRebindProjectGuardedRefusesOnMarkedRouteNotServed(t *testing.T) {
	// A pre-guard daemon's catch-all answers 404 with the daemon_rejected
	// marker — definitive proof the route, and so the precondition, is absent.
	srv, _ := rebindServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = apiproto.WriteEnvelope(w, apiproto.Envelope{Error: &apiproto.EnvelopeError{
			Message:        `unknown route "/v1/RebindProjectGuarded"`,
			DaemonRejected: true,
		}})
	}, nil)
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.Error(t, err)
	require.True(t, errors.Is(err, daemon.ErrGuardedRebindUnsupported),
		"a marked route-absent answer must refuse with the upgrade sentinel, got %v", err)
	assert.Contains(t, err.Error(), "daemon host")
}

func TestRebindProjectGuardedUnmarked404StaysUnconfirmed(t *testing.T) {
	// A proxy's plain 404 cannot prove the route is absent — the intermediary
	// may have answered without forwarding — so it must NOT collapse into the
	// unsupported sentinel; outcome unknown is the honest report.
	srv, _ := rebindServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>nginx 404</html>", http.StatusNotFound)
	}, nil)
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "/old", "chk-observed", "/new")
	require.Error(t, err)
	assert.False(t, errors.Is(err, daemon.ErrGuardedRebindUnsupported),
		"an unmarked intermediary 404 is not proof of route absence, got %v", err)
	var unconfirmed *UnconfirmedHTTPResponseError
	assert.True(t, errors.As(err, &unconfirmed), "outcome unknown, got %v", err)
}

func TestRebindProjectUnguardedUsesThePlainRoute(t *testing.T) {
	var got daemon.RebindProjectRequest
	srv, reached := rebindServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guarded route must not see an unguarded request")
	}, func(w http.ResponseWriter, r *http.Request) {
		writeRebindOK(t, w, r, &got)
	})
	c, err := NewRemote(srv.URL, "tok")
	require.NoError(t, err)

	_, err = c.RebindProject("prj", "", "", "/new")
	require.NoError(t, err)
	assert.Equal(t, "RebindProject", *reached,
		"an unguarded rebind keeps last-writer-wins on every daemon vintage")
}
