package daemon

import (
	"net/http"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// The warm-up contract (#1878): the web-tab proxy must not serve — or touch
// session state — until the daemon's restore has finished.
//
// RunDaemon binds the HTTP listener long BEFORE the restore (#829, deliberately:
// the restore shells out per remote-hook session and can take minutes). So a stale
// iframe left open across a daemon restart starts re-requesting the instant the
// port answers. Every one of those requests resolves through resolveStreamSession,
// which calls refreshLocked and REPLACES the instance map from disk — the proxy
// driving its own restore. RestoreInstances documents the invariant it broke:
// "every RPC that mutates it is gated on Ready". This route is HTTP, not net/rpc,
// so it slipped the gate entirely.

// warmingManager returns a manager in the state RunDaemon has one in between the
// listener bind and the end of the restore: constructed over the CURRENT
// AGENT_FACTORY_HOME, holding no instances, not ready. (NewManager is the
// synchronous-restore constructor, so a fixture built on it is already ready and
// cannot model this.)
func warmingManager(t *testing.T) *Manager {
	t.Helper()
	m, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	if m.Ready() {
		t.Fatal("a shell manager must report !Ready — it models daemon warm-up")
	}
	if n := len(m.instances); n != 0 {
		t.Fatalf("a shell manager holds %d instances, want 0 — the restore is what fills it", n)
	}
	return m
}

// TestWebTabProxy_WarmUpRequestDoesNotDriveItsOwnRestore is the #1878 regression
// test, and it asserts the SIDE-EFFECT rather than the status code: the point is
// not that the request is refused, it is that the request does not do the
// RESTORE'S JOB on its way to being refused.
//
// Shape: the fixture persists a session and its web tab to this home, then a
// SECOND, still-warming manager is built over that same home — a daemon whose
// listener is up while its restore has not run. Its instance map is empty, and the
// session it would serve is sitting on disk.
//
// resolveStreamSession opens with refreshLocked, which loads that session off disk
// and into the map (constructing instances and flipping AutoYes on, exactly as the
// restore does). So the map growing IS the bug, observed directly: the request
// restored a session that nothing asked it to restore.
func TestWebTabProxy_WarmUpRequestDoesNotDriveItsOwnRestore(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	// Sets AGENT_FACTORY_HOME and persists a session + web tab under it.
	_, _, sessionID, tabIDs, _ := newWebTabProxyFixtureN(t, upstream.URL)

	warming := warmingManager(t)
	mux := newHTTPMux(&controlServer{manager: warming})

	rec := proxyGet(t, mux, sessionID, tabIDs[0], "")

	warming.mu.Lock()
	loaded := len(warming.instances)
	warming.mu.Unlock()
	if loaded != 0 {
		t.Fatalf("a warm-up request loaded %d instance(s) off disk (status %d): the proxy drove its own restore, "+
			"which RestoreInstances documents as the restore's job — \"every RPC that mutates it is gated on Ready\"",
			loaded, rec.Code)
	}
	if warming.Ready() {
		t.Fatal("a proxy request must never be what marks the manager ready")
	}
}

// TestWebTabProxy_WarmUpRendersARetryingNotice pins WHAT the pane shows while the
// daemon restores. This route is framed, so its body is rendered at the user — a
// raw JSON error envelope in the iframe is the failure the editor's notice pages
// already exist to avoid, and a daemon restart points every open pane here at
// once, making this the likeliest error body in the whole surface to be seen.
func TestWebTabProxy_WarmUpRendersARetryingNotice(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m := warmingManager(t)
	mux := newHTTPMux(&controlServer{manager: m})

	rec := proxyGet(t, mux, "some-session", "some-tab", "")

	// 503 is the honest status: the tab genuinely is not being served, and a
	// browser still renders an error body inside an iframe.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 during warm-up (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html — the pane frames this body", ct)
	}
	body := rec.Body.String()
	// It resolves ON ITS OWN once the restore lands; the user reloads nothing.
	if !contains(body, `http-equiv="refresh"`) {
		t.Fatalf("the warm-up notice must retry itself, or a pane caught mid-restore stays blank forever: %s", body)
	}
	if !contains(body, "Starting up") {
		t.Fatalf("the warm-up notice must say what is happening: %s", body)
	}
	// A web tab frames the SAME route as an editor, and the kind is not even known
	// this early — so the notice must not tell someone previewing their dev server
	// that VS Code is starting.
	if contains(body, "VS Code") {
		t.Fatalf("the warm-up notice is kind-agnostic; it must not announce VS Code: %s", body)
	}
	// Never the raw envelope: writeHTTPError's JSON is what this replaces.
	if contains(body, `"error"`) {
		t.Fatalf("the warm-up reply rendered a JSON error envelope into the pane: %s", body)
	}
	// A notice must never be cached — the next request may be the live tab.
	if cc := rec.Header().Get("Cache-Control"); !contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store: a cached notice outlives the warm-up it describes", cc)
	}
}

// TestWebTabProxy_ServesOnceReady is the other half of the gate: it must be a
// WARM-UP gate, not a new refusal. A ready manager — every fixture's, and every
// daemon's within seconds of boot — proxies exactly as before.
func TestWebTabProxy_ServesOnceReady(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the manager is ready (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "PATH=/app.js") {
		t.Fatalf("upstream saw %q, want PATH=/app.js", got)
	}
}
