package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// The client half of the W4.3 stable-id tab surface (#5571): every wire method
// the daemon's remoteAgentServer will drive in W4.4, pinned against the
// {data,error} envelope the headless server emits — verbatim ids on create,
// ErrTabGone reconstructed from the tab_gone code (NEVER inferred from a bare
// 404, which an older server also gives an unknown route), and the health
// advertisement a feature check reads.

// tabWireServer records each call's method, path, auth header, and decoded
// body, and answers from a per-route canned payload. Route keys are the wire
// paths; a nil routes map entry answers a generic success.
type tabWireServer struct {
	t *testing.T

	mu      sync.Mutex
	methods map[string]string
	auths   map[string]string
	bodies  map[string]map[string]any
	queries map[string]string
	// route responses: path -> {data,error} payload to encode. An absent key
	// fails the test — a call to an unexpected route is a bug, not a 404.
	data  map[string]any
	fail  map[string]any // when set, encoded as the envelope's error member
	mux   *http.ServeMux
	token string
}

func newTabWireServer(t *testing.T) *tabWireServer {
	s := &tabWireServer{
		t:       t,
		methods: map[string]string{},
		auths:   map[string]string{},
		bodies:  map[string]map[string]any{},
		queries: map[string]string{},
		data:    map[string]any{},
		token:   "test-token",
	}
	s.mux = http.NewServeMux()
	return s
}

// serve routes path with the supplied data payload (or fail payload).
func (s *tabWireServer) route(path string, data any) *tabWireServer {
	s.data[path] = data
	s.mux.HandleFunc(path, s.handler(path))
	return s
}

// routeErr routes path answering a failure envelope carrying code ("" for a
// code-less failure — the pre-code server shape).
func (s *tabWireServer) routeErr(path, message, code string) *tabWireServer {
	errObj := map[string]any{"message": message}
	if code != "" {
		errObj["code"] = code
	}
	s.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		s.record(r, path)
		s.encode(w, map[string]any{"data": nil, "error": errObj})
	})
	return s
}

func (s *tabWireServer) handler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.record(r, path)
		s.encode(w, map[string]any{"data": s.data[path], "error": nil})
	}
}

func (s *tabWireServer) record(r *http.Request, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[path] = r.Method
	s.auths[path] = r.Header.Get("Authorization")
	s.queries[path] = r.URL.RawQuery
	if r.Body != nil {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.bodies[path] = body
		}
	}
}

func (s *tabWireServer) encode(w http.ResponseWriter, env map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		s.t.Errorf("encode %s response: %v", env, err)
	}
}

func (s *tabWireServer) client(t *testing.T, url string) *remoteAgentClient {
	t.Helper()
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: url, Token: s.token}, "probe")
	require.NoError(t, err)
	return rc
}

func (s *tabWireServer) assertAuth(t *testing.T, path string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, "Bearer "+s.token, s.auths[path],
		"%s must carry the bearer token", path)
}

// TestRemoteAgentClient_TabWireSurface drives every W4.3 control route the
// client speaks: stable-id create for both kinds, id-addressed close and
// preview, the roster with exit evidence, and the health capability probe —
// with the request bodies and the bearer token asserted on the wire.
func TestRemoteAgentClient_TabWireSurface(t *testing.T) {
	died := time.Unix(1726000000, 0)
	srv := newTabWireServer(t).
		route("/v1/agent/tab-create", map[string]any{
			"tab": map[string]any{
				"id": "daemon-tab-1", "name": "shell", "kind": int(TabKindShell),
				"tmux_name": "af_x__shell",
			},
		}).
		route("/v1/agent/tab-close", map[string]any{"ok": true}).
		route("/v1/agent/tabs", map[string]any{
			"tabs": []any{
				map[string]any{"id": "agent-id", "kind": int(TabKindAgent), "name": "agent"},
				map[string]any{
					"id": "deploy-id", "kind": int(TabKindProcess),
					"name": "deploy", "command": "./deploy.sh",
					"exit": map[string]any{"status": 7, "status_known": true, "at": died.Format(time.RFC3339Nano), "stopped_by": "daemon"},
				},
			},
		}).
		route("/v1/agent/preview", map[string]any{"content": "deploy: done", "has_terminal_modes": false}).
		route("/v1/health", map[string]any{
			"ok": true, "version": "1.2.3-test", "features": []string{"tab-api", "tab-id-addressing"},
		})
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()
	rc := srv.client(t, httpSrv.URL)

	// --- create: the daemon's id travels verbatim as both assignment and key ---
	created, err := rc.createTab("daemon-tab-1", TabKindName(TabKindShell), "", "")
	require.NoError(t, err)
	assert.Equal(t, "daemon-tab-1", created.ID, "the roster row reports the supplied id verbatim")
	assert.Equal(t, TabKindShell, created.Kind)
	body := srv.bodies["/v1/agent/tab-create"]
	assert.Equal(t, "daemon-tab-1", body["id"])
	assert.Equal(t, TabKindName(TabKindShell), body["kind"])
	assert.Equal(t, http.MethodPost, srv.methods["/v1/agent/tab-create"])
	srv.assertAuth(t, "/v1/agent/tab-create")

	// --- close: id-only, never an ordinal ------------------------------------
	require.NoError(t, rc.closeTab("daemon-tab-1", ""))
	assert.Equal(t, "daemon-tab-1", srv.bodies["/v1/agent/tab-close"]["id"])
	srv.assertAuth(t, "/v1/agent/tab-close")

	// --- roster: stable ids and the refreshed exit evidence ------------------
	tabs, err := rc.tabs()
	require.NoError(t, err)
	require.Len(t, tabs, 2)
	assert.Equal(t, TabKindAgent, tabs[0].Kind, "the agent row rides at index 0")
	proc := tabs[1]
	assert.Equal(t, "deploy-id", proc.ID)
	assert.Equal(t, "./deploy.sh", proc.Command)
	require.NotNil(t, proc.Exit, "the process row's exit evidence must survive the wire")
	assert.Equal(t, 7, proc.Exit.Status)
	assert.True(t, proc.Exit.StatusKnown)
	assert.Equal(t, "daemon", proc.Exit.StoppedBy)
	assert.True(t, proc.Exit.At.Equal(died), "at = %v, want %v", proc.Exit.At, died)
	srv.assertAuth(t, "/v1/agent/tabs")

	// --- preview by stable id: tab_id rides, the ordinal does not ------------
	snap, err := rc.previewByID("deploy-id", true)
	require.NoError(t, err)
	assert.Equal(t, "deploy: done", snap.Content)
	prev := srv.bodies["/v1/agent/preview"]
	assert.Equal(t, "deploy-id", prev["tab_id"])
	assert.Equal(t, float64(0), prev["tab"], "an id-addressed preview must not send a positional tab")
	assert.Equal(t, true, prev["full"])
	srv.assertAuth(t, "/v1/agent/preview")

	// --- health: the capability probe a feature check reads -------------------
	health, err := rc.health()
	require.NoError(t, err)
	assert.True(t, health.OK)
	assert.Equal(t, "1.2.3-test", health.Version)
	assert.Equal(t, []string{"tab-api", "tab-id-addressing"}, health.Features)
	assert.Equal(t, http.MethodGet, srv.methods["/v1/health"],
		"/v1/health is the wire's one GET — doJSON must not force POST")
	srv.assertAuth(t, "/v1/health")
}

// TestRemoteAgentClient_TabGoneCodeReconstructsSentinel pins the 404
// disambiguation the slice exists for: only the `tab_gone` code — emitted by a
// server that routed the call and found the id dead — reconstructs ErrTabGone.
// A bare 404 (or a 404-shaped failure with no code) is what an OLDER server
// gives an unknown route, and it must stay sentinel-free so the caller never
// reports "route predates server" as "the tab died".
func TestRemoteAgentClient_TabGoneCodeReconstructsSentinel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		wantIdent bool
	}{
		{name: "tab_gone reconstructs ErrTabGone", code: apiproto.ErrorCodeTabGone, wantIdent: true},
		{name: "code-less failure stays sentinel-free", code: "", wantIdent: false},
		{name: "unrelated code stays sentinel-free", code: apiproto.ErrorCodeResourceExhausted, wantIdent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTabWireServer(t).routeErr("/v1/agent/tab-close", "tab id \"x\" names no live tab", tc.code)
			httpSrv := httptest.NewServer(srv.mux)
			defer httpSrv.Close()
			rc := srv.client(t, httpSrv.URL)

			err := rc.closeTab("x", "")
			require.Error(t, err)
			assert.Equal(t, tc.wantIdent, errors.Is(err, ErrTabGone),
				"errors.Is(err, ErrTabGone) for code %q (err: %v)", tc.code, err)
		})
	}
}

// TestRemoteAgentClient_DialStreamByIDAddressing pins the stream's addressing
// contract at the URL: ?tab_id= selects the id-native bind on a server that
// advertises it, and the legacy ?tab= ordinal is sent only in its absence.
// The handshake itself is exercised by the ws_timeout tests and the
// out-of-process integration; what matters here is the query the client emits
// — that is the whole wire contract.
func TestRemoteAgentClient_DialStreamByIDAddressing(t *testing.T) {
	var (
		mu    sync.Mutex
		query string
	)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.RawQuery
		mu.Unlock()
		// Refuse the upgrade: the dial fails, but the request line was captured.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer httpSrv.Close()

	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: httpSrv.URL, Token: "test-token"}, "probe")
	require.NoError(t, err)

	_, err = rc.dialStreamByID(context.Background(), "daemon-tab-1")
	require.Error(t, err, "the refused upgrade must error — the assertion is on the emitted URL")
	mu.Lock()
	got := query
	mu.Unlock()
	assert.Contains(t, got, "tab_id=daemon-tab-1", "an id-addressed dial must send ?tab_id=")
	assert.NotContains(t, got, "tab=", "an id-addressed dial must not send a positional tab")
	// The token still rides the query exactly as the ordinal arm does.
	assert.Contains(t, got, "access_token=test-token")

	// The ordinal arm keeps ?tab= and no tab_id.
	mu.Lock()
	query = ""
	mu.Unlock()
	_, err = rc.dialStream(context.Background(), 2, "", 0)
	require.Error(t, err)
	mu.Lock()
	got = query
	mu.Unlock()
	assert.Contains(t, got, "tab=2")
	assert.NotContains(t, got, "tab_id")
}
