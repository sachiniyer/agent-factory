package apiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// The Snapshot sequence a remote daemon serves through the registration race
// (#6586): the credential is bound from the first answer (Caller set
// unconditionally for a sandbox credential) while the row it names appears
// only later — no rows, then a sibling's row, then the sibling and the caller
// together. The changed CallerNoSpawn branch — Caller set, scan Instances for
// it, sentinel when absent — is what each call pins.
func TestCallerNoSpawnRegistrationRaceSequence(t *testing.T) {
	clearTargetEnv(t)
	var mu sync.Mutex
	var tokens []string
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		tokens = append(tokens, agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader)))
		caller := session.InstanceData{ID: "caller-id", Title: "me"}
		sibling := session.InstanceData{ID: "sibling-id", Title: "sib"}
		resp := daemon.SnapshotResponse{Caller: caller.ID}
		switch calls {
		case 1: // bound, but the caller's row is not yet registered
		case 2:
			// A sibling row lands first — the projection still lacks the
			// caller's own row, so the sentinel must still come back.
			resp.Instances = []session.InstanceData{sibling}
		default:
			resp.Instances = []session.InstanceData{sibling, caller}
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(resp))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(envDaemonURL, srv.URL)
	t.Setenv(envDaemonToken, "tok-abc")

	ctx := context.Background()
	_, ok, err := CallerNoSpawn(ctx)
	require.ErrorIs(t, err, ErrCallerRowNotRegistered)
	require.False(t, ok)

	_, ok, err = CallerNoSpawn(ctx)
	require.ErrorIs(t, err, ErrCallerRowNotRegistered,
		"a sibling row in the projection must not satisfy the caller lookup")
	require.False(t, ok)

	row, ok, err := CallerNoSpawn(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "caller-id", row.ID, "the caller's own row is returned once it registers")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"tok-abc", "tok-abc", "tok-abc"}, tokens,
		"the same token goes out on every request")
}

// An operator credential (Caller empty in the response) is the plain ok=false,
// nil-error answer — the signal that sends the caller to the tmux-name path —
// even when the snapshot carries rows.
func TestCallerNoSpawnOperatorCredential(t *testing.T) {
	clearTargetEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, _ *http.Request) {
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{
			Instances: []session.InstanceData{{ID: "s1", Title: "someone"}},
		}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(envDaemonURL, srv.URL)
	t.Setenv(envDaemonToken, "operator-tok")

	_, ok, err := CallerNoSpawn(context.Background())
	require.NoError(t, err)
	require.False(t, ok, "no bound caller: the caller falls back to the tmux-name path")
}

// A bound caller whose session the daemon reports as gone (CallerGone — the
// session is in neither the materialized rows nor the pending creates, so its
// row will never land) is not the registration race: ErrCallerSessionGone
// instead of the retriable sentinel, so the caller fails fast on the first
// answer (#6690).
func TestCallerNoSpawnCallerGoneFailsFast(t *testing.T) {
	clearTargetEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, _ *http.Request) {
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{
			Caller:     "ghost-id",
			CallerGone: true,
			Instances:  []session.InstanceData{{ID: "sibling-id", Title: "sib"}},
		}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(envDaemonURL, srv.URL)
	t.Setenv(envDaemonToken, "tok-abc")

	_, ok, err := CallerNoSpawn(context.Background())
	require.ErrorIs(t, err, ErrCallerSessionGone)
	require.False(t, ok)
}

// A refused credential surfaces the daemon's own answer rather than collapsing
// into ok=false — a caller that fell back to the tmux path on a 401 would
// report a misleading "no session" instead of the auth failure.
func TestCallerNoSpawnUnauthorizedSurfaces(t *testing.T) {
	clearTargetEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = apiproto.WriteEnvelope(w, apiproto.Failure("unauthorized"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(envDaemonURL, srv.URL)
	t.Setenv(envDaemonToken, "bad-tok")

	_, ok, err := CallerNoSpawn(context.Background())
	require.ErrorContains(t, err, "unauthorized")
	require.False(t, ok)
}
