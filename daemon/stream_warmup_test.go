package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The warm-up contract for the WebSocket stream plane (#2109), the same contract
// the web-tab proxy was brought under in #1878.
//
// RunDaemon binds the HTTP listener long BEFORE the restore finishes (#829,
// deliberately: the restore shells out per remote-hook session and can take
// minutes). Both stream routes are HTTP rather than net/rpc, so — exactly like the
// proxy did — they slipped the requireManagerReady gate that every state-reading
// RPC goes through. A stream request landing in that window runs
// agentServerForStream → resolveStreamSession → refreshLocked, which builds
// instances off disk and puts them in m.instances. RestoreInstances then rebuilds
// that map from scratch, so the instance already handed to the client is no longer
// the tracked one: an orphan the client keeps streaming against. requireManagerReady's
// own doc comment names this failure mode — "construct throwaway instances from
// disk that the restore then orphans".

// seedWarmingStreamDaemon sets up the state RunDaemon is in between the listener
// bind and the end of the restore: a session sitting ON DISK, and a manager over
// that same home holding no instances and not ready.
//
// The row has to be one a restore would really MATERIALIZE, and getting there
// takes some care. seedDiskInstance's minimal InstanceData carries no worktree, so
// FromInstanceData rejects it outright; a row for a LIVE local session carries one
// but FromInstanceData then calls instance.Start, which reconnects to a REAL tmux
// session that a mock-tmux fixture never created — so it is skipped too. Against
// either seed the side-effect assertion below passes on UNFIXED code and the whole
// test is vacuous.
//
// An ARCHIVED row is the shape that loads without a live tmux: FromInstanceData
// rebuilds its worktree and returns INERT before Start (#1028). It is a real
// restorable session — the restore materializes it, findSession registers it — so
// it reproduces the orphaning faithfully, and restorableRowGuard proves it does
// rather than leaving that to be assumed.
func seedWarmingStreamDaemon(t *testing.T) (mux *http.ServeMux, warming *Manager, repoID, title string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	title = "streamer"

	ready, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	inst := startedLocalTabInstance(t, ready, repo.ID, repoPath, title, "af_"+title+"_agent")
	if err := inst.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("archive transition: %v", err)
	}
	ready.persistInstance(repo.ID, inst)
	restorableRowGuard(t)

	warming = warmingManager(t)
	return newHTTPMux(&controlServer{manager: warming}), warming, repo.ID, title
}

// restorableRowGuard proves the seeded home holds a session a restore actually
// loads. Without it, "the warm-up request loaded no instances" could mean the gate
// works OR that there was never anything loadable there — and the second reading
// is a green test that checks nothing.
func restorableRowGuard(t *testing.T) {
	t.Helper()
	probe, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager (restorable-row probe): %v", err)
	}
	probe.mu.Lock()
	loaded := len(probe.instances)
	probe.mu.Unlock()
	if loaded == 0 {
		t.Fatal("the seeded home holds no RESTORABLE session: a warm-up request would find nothing to load, " +
			"so the side-effect assertion would pass even on ungated code")
	}
}

// streamGet issues a GET against a stream route with a plain recorder. A recorder
// is not a Hijacker, which is exactly the point for the /stream case: if the gate
// works, the refusal is written as an ordinary HTTP response and the upgrade is
// never attempted.
func streamGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// coder/websocket only attempts an upgrade for a request that ASKS for one, so
	// the /stream case has to ask — otherwise the handler would fall out somewhere
	// unrelated to the gate and the test would prove nothing about it.
	if !contains(path, "stream-info") {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	mux.ServeHTTP(rec, req)
	return rec
}

// assertNoRestoreDriven is the side-effect assertion, and it is the one that
// matters: the point is not that the request is refused, it is that it does not do
// the RESTORE'S JOB on its way to being refused.
func assertNoRestoreDriven(t *testing.T, m *Manager, status int) {
	t.Helper()
	m.mu.Lock()
	loaded := len(m.instances)
	m.mu.Unlock()
	if loaded != 0 {
		t.Fatalf("a warm-up stream request loaded %d instance(s) off disk (status %d): it drove its own restore, "+
			"and RestoreInstances will orphan what it just handed the client — exactly the case "+
			"requireManagerReady documents as \"throwaway instances from disk that the restore then orphans\"",
			loaded, status)
	}
	if m.Ready() {
		t.Fatal("a stream request must never be what marks the manager ready")
	}
}

// TestStreamInfoHandler_WarmUpDoesNotDriveItsOwnRestore is the #2109 regression
// test for GET /v1/sessions/{id}/stream-info.
func TestStreamInfoHandler_WarmUpDoesNotDriveItsOwnRestore(t *testing.T) {
	mux, warming, repoID, title := seedWarmingStreamDaemon(t)

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream-info?repo_id=%s", title, repoID))

	assertNoRestoreDriven(t, warming, rec.Code)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 during warm-up (body: %s)", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), daemonStartingErrText) {
		t.Fatalf("body = %s, want the warm-up error text so a client knows to retry", rec.Body.String())
	}
}

// TestStreamHandler_WarmUpRefusedBeforeUpgrade is the #2109 regression test for GET
// /v1/sessions/{id}/stream, and it pins WHERE the refusal happens as well as that
// it happens. The gate has to fire BEFORE websocket.Accept: after the upgrade the
// only way to say no is a WS close frame, which a client handles on a completely
// different path from an HTTP status — it reads as a stream that dropped, not as
// "retry in a moment". A recorder cannot be hijacked, so an attempted upgrade would
// show up here as a failure that is not the gate's 503.
func TestStreamHandler_WarmUpRefusedBeforeUpgrade(t *testing.T) {
	mux, warming, repoID, title := seedWarmingStreamDaemon(t)

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream?repo_id=%s", title, repoID))

	assertNoRestoreDriven(t, warming, rec.Code)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 during warm-up (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusSwitchingProtocols || rec.Header().Get("Sec-WebSocket-Accept") != "" {
		t.Fatal("the warm-up refusal upgraded the connection first; it must be a plain HTTP 503")
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want the JSON failure envelope every other stream error uses", ct)
	}
	if !contains(rec.Body.String(), daemonStartingErrText) {
		t.Fatalf("body = %s, want the warm-up error text so a client knows to retry", rec.Body.String())
	}
}

// TestStreamInfoHandler_ServesOnceReady is the other half of the gate: it must be a
// WARM-UP gate, not a new refusal. A ready manager — every daemon's within seconds
// of boot — answers exactly as before.
func TestStreamInfoHandler_ServesOnceReady(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !manager.Ready() {
		t.Fatal("NewManager restores synchronously; its manager must be ready")
	}
	const title = "streamer"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream-info?repo_id=%s", title, repo.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the manager is ready (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "/stream") {
		t.Fatalf("stream-info body = %s, want the session's stream URL", got)
	}
}

// TestStreamHandler_ReadyManagerReachesResolution proves the gate is scoped to
// warm-up on the /stream route too: with a ready manager the request runs on into
// session resolution, and an unknown session is refused as the 404 it has always
// been — never the warm-up 503.
func TestStreamHandler_ReadyManagerReachesResolution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, "/v1/sessions/no-such-session/stream")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown session on a ready manager (body: %s)",
			rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), daemonStartingErrText) {
		t.Fatalf("a ready manager answered with the warm-up error: %s", rec.Body.String())
	}
}
