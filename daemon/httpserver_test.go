package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon HTTP/JSON server (#1029 PR 4) is a 1:1 mirror of the client-facing
// RPCs: each route decodes the SAME request struct and calls the SAME
// controlServer method the net/rpc handler calls, then encodes the response in
// the shared {data,error} envelope. These tests drive the mux directly (no
// socket bound) since the mux is the whole surface under test.

// doHTTP dispatches one request through the daemon HTTP mux.
func doHTTP(cs *controlServer, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	newHTTPMux(cs).ServeHTTP(rec, req)
	return rec
}

// doHTTPAsClient is doHTTP for a request that identifies itself as a machine-
// generated af client, the way every apiclient call does. The header is the
// daemon's discriminator for unknown-field handling, so it is the only difference
// between this and a hand-authored curl.
func doHTTPAsClient(cs *controlServer, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(agentproto.ClientVersionHeader, "9.9.9")
	rec := httptest.NewRecorder()
	newHTTPMux(cs).ServeHTTP(rec, req)
	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) apiproto.Envelope {
	t.Helper()
	var env apiproto.Envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// dataInto re-marshals the envelope's Data member into a typed response struct.
func dataInto(t *testing.T, env apiproto.Envelope, dst any) {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, dst))
}

// TestHTTP_ListTasks_ReadRoute covers a read route: POST /v1/ListTasks returns
// 200 with the task list read from disk, wrapped in the success envelope.
func TestHTTP_ListTasks_ReadRoute(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1001", "")))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1002", "")))

	rec := doHTTP(&controlServer{}, http.MethodPost, "/v1/ListTasks", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp ListTasksResponse
	dataInto(t, env, &resp)
	require.Len(t, resp.Tasks, 2)
	assert.ElementsMatch(t, []string{"aaaa1001", "aaaa1002"},
		[]string{resp.Tasks[0].ID, resp.Tasks[1].ID})
}

// TestHTTP_Snapshot_ReadRoute covers the sessions read route against a ready
// manager: an all-repo Snapshot with no sessions returns an empty list.
func TestHTTP_Snapshot_ReadRoute(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := doHTTP(&controlServer{manager: m}, http.MethodPost, "/v1/Snapshot", `{"repo_id":""}`)
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp SnapshotResponse
	dataInto(t, env, &resp)
	require.Empty(t, resp.Instances)
}

// TestHTTP_AddTask_MutationRoute covers a create/mutation route: POST /v1/AddTask
// persists the task through the shared core and re-arms the scheduler.
func TestHTTP_AddTask_MutationRoute(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cs := &controlServer{scheduler: newTaskScheduler()}

	body, err := json.Marshal(AddTaskRequest{Task: enabledCronTask("bbbb1001", "")})
	require.NoError(t, err)
	rec := doHTTP(cs, http.MethodPost, "/v1/AddTask", string(body))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp AddTaskResponse
	dataInto(t, env, &resp)
	assert.True(t, resp.OK)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "AddTask route must persist through the shared daemon core")
	assert.Equal(t, "bbbb1001", tasks[0].ID)
	assert.Contains(t, cs.scheduler.scheduledTaskIDs(), "bbbb1001",
		"AddTask route must re-arm the scheduler like the RPC handler")
}

// TestHTTP_TriggerTask_HandlerError covers the TriggerTask mutation route and the
// handler-error → 500 mapping: RunTask refuses a disabled task, and the envelope
// still carries the error message.
func TestHTTP_TriggerTask_HandlerError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	disabled := enabledCronTask("cccc1001", "")
	disabled.Enabled = false
	require.NoError(t, task.AddTask(disabled))

	rec := doHTTP(&controlServer{}, http.MethodPost, "/v1/TriggerTask", `{"id":"cccc1001"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Data)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, "disabled")
}

// TestHTTP_MalformedJSON_400 covers the malformed-body → 400 mapping.
func TestHTTP_MalformedJSON_400(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rec := doHTTP(&controlServer{scheduler: newTaskScheduler()}, http.MethodPost, "/v1/AddTask", `{not json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Data)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, "malformed JSON")
}

// TestHTTP_UnknownJSONField_400 covers strict request decoding: a typo like
// repo_idd must fail as a bad request instead of silently becoming the
// zero-value RepoID and dispatching an all-repo Snapshot.
//
// This request is hand-authored (no client-version header), which is the
// population that keeps strict decoding — see decodeHTTPRequest. Do not add the
// header here: that would delete the #1264/#1273 guard this test exists to hold.
func TestHTTP_UnknownJSONField_400(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := doHTTP(&controlServer{manager: m}, http.MethodPost, "/v1/Snapshot", `{"repo_idd":"typo"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Data)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, `unknown field "repo_idd"`)
}

// TestHTTP_AfClientUnknownAdditiveField_Tolerated is the forward-compat lock.
//
// The daemon is upgraded independently of its clients (#960), so a client newer
// than the daemon legitimately sends fields the daemon has never heard of. That
// is not hypothetical: #1779 added tab_id to PreviewRequest, and every newer TUI
// then 400'd its 100ms preview poll against any older daemon with
// `unknown field "tab_id"`. Per the #1029 additive-envelope contract such a field
// must be IGNORED, not fatal.
//
// The unknown field here stands in for a field some FUTURE client will add, which
// is the only honest way to test forward compatibility from inside the current
// version.
func TestHTTP_AfClientUnknownAdditiveField_Tolerated(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := doHTTPAsClient(&controlServer{manager: m}, http.MethodPost, "/v1/Snapshot",
		`{"repo_id":"","field_from_a_newer_client":"additive"}`)

	require.Equal(t, http.StatusOK, rec.Code,
		"an af client's unknown additive field must degrade, not hard-fail: rejecting it makes every daemon/client version skew a hard error")
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
}

// TestHTTP_AfClientSkewedField_DoesNotRejectByName reproduces the reported
// incident's exact shape on the exact route: a Preview body carrying a field this
// daemon does not know, sent by an af client. Whatever else Preview does with an
// unknown session, it must never fail the request for the FIELD.
func TestHTTP_AfClientSkewedField_DoesNotRejectByName(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := doHTTPAsClient(&controlServer{manager: m}, http.MethodPost, "/v1/Preview",
		`{"title":"alpha","repo_id":"","tab":0,"tab_id":"t-abc","full":false,"field_from_a_newer_client":"additive"}`)

	env := decodeEnvelope(t, rec)
	if env.Error != nil {
		assert.NotContains(t, env.Error.Message, "unknown field",
			"the request must never be rejected for carrying a field a newer client added")
		assert.NotEqual(t, http.StatusBadRequest, rec.Code,
			"a skewed additive field must not be a bad request")
	}
}

// TestHTTP_OversizeBody_413_NotDispatched covers the body-over-limit → 413
// mapping AND proves the oversize request is REJECTED, never
// truncated-then-dispatched: with a well-formed AddTask body that exceeds the
// (shrunk) cap, nothing is persisted and the scheduler is untouched — the
// manager was never reached.
func TestHTTP_OversizeBody_413_NotDispatched(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	orig := maxHTTPBodyBytes
	maxHTTPBodyBytes = 64
	t.Cleanup(func() { maxHTTPBodyBytes = orig })

	cs := &controlServer{scheduler: newTaskScheduler()}
	body, err := json.Marshal(AddTaskRequest{Task: enabledCronTask("ffff1001", "")})
	require.NoError(t, err)
	require.Greater(t, int64(len(body)), maxHTTPBodyBytes,
		"body must exceed the cap for this test to exercise 413")

	rec := doHTTP(cs, http.MethodPost, "/v1/AddTask", string(body))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Data)
	require.NotNil(t, env.Error)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Empty(t, tasks, "oversize request must be rejected before the manager write")
	require.Empty(t, cs.scheduler.scheduledTaskIDs(),
		"oversize request must not re-arm the scheduler")
}

// TestHTTP_BodyUnderLimit_Succeeds covers the boundary from the other side: a
// body that fits under the cap still decodes and dispatches normally.
func TestHTTP_BodyUnderLimit_Succeeds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cs := &controlServer{scheduler: newTaskScheduler()}
	body, err := json.Marshal(AddTaskRequest{Task: enabledCronTask("ffff2001", "")})
	require.NoError(t, err)

	orig := maxHTTPBodyBytes
	maxHTTPBodyBytes = int64(len(body)) + 16 // comfortably above the body
	t.Cleanup(func() { maxHTTPBodyBytes = orig })

	rec := doHTTP(cs, http.MethodPost, "/v1/AddTask", string(body))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, decodeEnvelope(t, rec).Error)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "ffff2001", tasks[0].ID)
}

// TestHTTP_UnknownRoute_404 covers the unknown-route → 404 mapping via the mux
// catch-all, still returning the envelope body.
func TestHTTP_UnknownRoute_404(t *testing.T) {
	rec := doHTTP(&controlServer{}, http.MethodPost, "/v1/NoSuchMethod", `{}`)
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Data)
	require.NotNil(t, env.Error)
}

// TestHTTP_WrongVerb_405 covers the wrong-verb → 405 mapping: RPC routes are
// POST-only, health is GET-only.
func TestHTTP_WrongVerb_405(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	rec := doHTTP(&controlServer{}, http.MethodGet, "/v1/ListTasks", "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.NotNil(t, decodeEnvelope(t, rec).Error)

	rec = doHTTP(&controlServer{}, http.MethodPost, "/v1/health", `{}`)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.NotNil(t, decodeEnvelope(t, rec).Error)
}

// TestHTTP_Health covers GET /v1/health mapping to Ping.
func TestHTTP_Health(t *testing.T) {
	rec := doHTTP(&controlServer{}, http.MethodGet, "/v1/health", "")
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp PingResponse
	dataInto(t, env, &resp)
	assert.True(t, resp.OK)
}

// TestHTTP_SharedCore_MatchesRPCTwin is the shared-core proof: the HTTP route and
// the net/rpc handler (the controlServer method called directly) return
// equivalent results because they ARE the same method.
func TestHTTP_SharedCore_MatchesRPCTwin(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("dddd1001", "")))
	require.NoError(t, task.AddTask(enabledCronTask("dddd1002", "")))
	cs := &controlServer{}

	// net/rpc twin: invoke the method the way rpc.ServeConn would.
	var rpcResp ListTasksResponse
	require.NoError(t, cs.ListTasks(ListTasksRequest{}, &rpcResp))

	// HTTP path: same method, wrapped in the envelope.
	rec := doHTTP(cs, http.MethodPost, "/v1/ListTasks", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var httpResp ListTasksResponse
	dataInto(t, decodeEnvelope(t, rec), &httpResp)

	require.Equal(t, rpcResp, httpResp,
		"HTTP route and net/rpc twin must produce equivalent results (one shared core)")
}

// TestHTTP_UnixSocket_EndToEnd drives the real transport: startHTTPServer binds
// the 0600 Unix socket, and a net/http client dialing that socket reaches the
// health and ListTasks routes — the `curl --unix-socket` path from the PR. It
// also asserts the socket is torn down on close.
func TestHTTP_UnixSocket_EndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, task.AddTask(enabledCronTask("eeee1001", "")))

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)

	sockPath, err := DaemonHTTPSocketPath()
	require.NoError(t, err)

	// 0600 is the authentication: no group/other access to the socket.
	info, err := os.Stat(sockPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}

	getEnvelope := func(resp *http.Response) apiproto.Envelope {
		t.Helper()
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		var env apiproto.Envelope
		require.NoError(t, json.Unmarshal(body, &env))
		return env
	}

	// GET /v1/health over the socket.
	resp, err := client.Get("http://af/v1/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Nil(t, getEnvelope(resp).Error)

	// POST /v1/ListTasks over the socket returns the persisted task.
	resp, err = client.Post("http://af/v1/ListTasks", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listResp ListTasksResponse
	dataInto(t, getEnvelope(resp), &listResp)
	require.Len(t, listResp.Tasks, 1)
	assert.Equal(t, "eeee1001", listResp.Tasks[0].ID)

	// Close tears the listener down and unlinks the socket.
	require.NoError(t, closeHTTP())
	_, statErr := os.Stat(sockPath)
	assert.True(t, os.IsNotExist(statErr), "socket file must be unlinked on close")
}

// TestHTTPResponseWriteAbandoned covers the expected disconnect errors emitted
// when a client tears down its connection before the response is written.
func TestHTTPResponseWriteAbandoned(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		net.ErrClosed,
		syscall.EPIPE,
		syscall.ECONNRESET,
		fmt.Errorf("wrapped: %w", syscall.EPIPE),
	} {
		assert.True(t, httpResponseWriteAbandoned(err), "expected abandoned response write: %v", err)
	}
	assert.False(t, httpResponseWriteAbandoned(errors.New("disk full")),
		"unexpected response write errors must remain visible")
}

// TestHTTP_SuccessBodyUsesSharedEnvelopeWriter pins that the HTTP success body is
// produced by the SAME apiproto.WriteEnvelope the CLI's --json path uses, so the
// two surfaces are byte-for-byte identical and can never drift.
func TestHTTP_SuccessBodyUsesSharedEnvelopeWriter(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: m}

	rec := doHTTP(cs, http.MethodPost, "/v1/Snapshot", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{}, &resp))
	var want bytes.Buffer
	require.NoError(t, apiproto.WriteEnvelope(&want, apiproto.Success(resp)))

	require.Equal(t, want.String(), rec.Body.String(),
		"HTTP body must be the shared envelope writer's bytes (identical to CLI --json)")
}
