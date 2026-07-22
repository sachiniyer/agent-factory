package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// newWebTabProxyFixture builds a manager with one started local instance holding a
// web tab pointing at target, and returns the served mux plus the instance's
// stable id (the web client's key) and the web tab's STABLE id (#1810 — the proxy
// route's key).
func newWebTabProxyFixture(t *testing.T, target string) (mux *http.ServeMux, sessionID, tabID string) {
	t.Helper()
	m, _, sessionID, ids, _ := newWebTabProxyFixtureN(t, target)
	return m, sessionID, ids[0]
}

// newWebTabProxyFixtureWithInstance is newWebTabProxyFixture plus the tracked
// instance, for tests that must drive its lifecycle state (archived, #1809).
func newWebTabProxyFixtureWithInstance(t *testing.T, target string) (
	mux *http.ServeMux, inst *session.Instance, sessionID, tabID string,
) {
	t.Helper()
	m, inst, sessionID, ids, _ := newWebTabProxyFixtureN(t, target)
	return m, inst, sessionID, ids[0]
}

// newWebTabProxyFixtureN is newWebTabProxyFixture for N web tabs, returning the
// tracked instance plus each tab's stable id in creation order (so tabs sit at
// ordinals 1..N after the agent tab) and a closer that closes the Nth web tab. It
// is what the misroute tests need: they must close a LOWER tab and prove a HIGHER
// one still resolves to its own dev server.
func newWebTabProxyFixtureN(t *testing.T, targets ...string) (
	mux *http.ServeMux, inst *session.Instance, sessionID string, tabIDs []string, closeWebTab func(n int),
) {
	t.Helper()
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
	const title = "webproxy"
	inst = startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	for i, target := range targets {
		if _, _, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: target, Name: fmt.Sprintf("web%d", i),
		}); err != nil {
			t.Fatalf("CreateTab(web, %q): %v", target, err)
		}
	}
	tabs := inst.GetTabs()
	if len(tabs) != len(targets)+1 {
		t.Fatalf("tabs = %d, want %d (agent + %d web)", len(tabs), len(targets)+1, len(targets))
	}
	for i := range targets {
		id := tabs[i+1].ID
		if id == "" {
			t.Fatalf("web tab %d has no stable id; the proxy route is id-keyed (#1810)", i)
		}
		tabIDs = append(tabIDs, id)
	}
	closeWebTab = func(n int) {
		t.Helper()
		if _, err := manager.CloseTab(CloseTabRequest{
			Title: title, RepoID: repo.ID, TabName: fmt.Sprintf("web%d", n),
		}); err != nil {
			t.Fatalf("CloseTab(web%d): %v", n, err)
		}
	}
	return newHTTPMux(&controlServer{manager: manager}), inst, inst.ID, tabIDs, closeWebTab
}

// proxyGet issues a proxied GET for the remainder `sub` under the tab's prefix.
func proxyGet(t *testing.T, mux *http.ServeMux, sessionID, tabID, sub string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/%s", sessionID, tabID, sub), nil)
	mux.ServeHTTP(rec, req)
	return rec
}

// followWebTabTokenBootstrap drives the two browser requests made when an iframe
// src carries the daemon's private query credential: the 307 that stores the
// HttpOnly cookie and removes the query parameter, then the clean cookie-backed
// GET that may reach the preview app.
func followWebTabTokenBootstrap(
	t *testing.T, handler http.Handler, req *http.Request,
) (bootstrap, clean *httptest.ResponseRecorder) {
	t.Helper()
	bootstrap = httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, req)
	if bootstrap.Code != http.StatusTemporaryRedirect {
		t.Fatalf("bootstrap status = %d, want 307 (body: %s)", bootstrap.Code, bootstrap.Body.String())
	}
	location := bootstrap.Header().Get("Location")
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse bootstrap Location %q: %v", location, err)
	}
	if redirectURL.Query().Has(webtabTokenQueryParam) {
		t.Fatalf("bootstrap Location %q still exposes %s", location, webtabTokenQueryParam)
	}
	cookie := cookieNamed(bootstrap, webtabTokenCookie)
	if cookie == nil {
		t.Fatal("bootstrap did not set the web-tab token cookie")
	}

	cleanReq := httptest.NewRequest(http.MethodGet, location, nil)
	cleanReq.RemoteAddr = req.RemoteAddr
	cleanReq.AddCookie(cookie)
	clean = httptest.NewRecorder()
	handler.ServeHTTP(clean, cleanReq)
	return bootstrap, clean
}

// TestWebTabProxy_RejectsArchivedSession is the #1809 follow-up gate: archive now
// PRESERVES web tabs, which made an archived session the first one whose web tab
// the proxy could resolve. An archived session is inert — its stored target is a
// bare loopback address whose dev server is long gone and whose port may now host
// something else — so the proxy must refuse until a restore. The tab works again
// the moment liveness flips back.
func TestWebTabProxy_RejectsArchivedSession(t *testing.T) {
	const marker = "AF_WEBTAB_ARCHIVED_GATE"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, marker)
	}))
	defer upstream.Close()

	mux, inst, id, tabID := newWebTabProxyFixtureWithInstance(t, upstream.URL)

	// Live: the target proxies through (the control — proving the refusal below is
	// the archived gate and not a broken fixture).
	if rec := proxyGet(t, mux, id, tabID, ""); rec.Code != http.StatusOK {
		t.Fatalf("live web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Archived: refused, and the upstream is never reached.
	inst.SetStatusForTest(session.Archived)
	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("archived web tab: status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "archived") {
		t.Fatalf("archived web tab: body = %q, want an actionable archived message", body)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("archived web tab: the upstream was proxied; an archived session must be inert")
	}

	// Restored: the preserved tab serves again — the gate is state, not a tombstone.
	inst.SetStatusForTest(session.Running)
	rec = proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restored web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("restored web tab: body = %q, want the upstream content", rec.Body.String())
	}
}

// TestWebTabProxy_RejectsWhileArchiveInFlight covers the window the settled
// LiveArchived gate misses. BeginArchive raises OpArchiving and only THEN tears
// tmux down and moves the worktree; liveness stays live until CommitArchive lands
// at the very end. A gate reading only the settled state would keep proxying the
// preserved loopback URL for the whole teardown, so an iframe left open when the
// user hit archive goes on reaching a port on the daemon's machine while its
// session is being dismantled. Terminal streams fence this window via
// killsInFlight; the proxy route is not serialized with ArchiveSession at all, so
// the fence has to live on the instance.
func TestWebTabProxy_RejectsWhileArchiveInFlight(t *testing.T) {
	const marker = "AF_WEBTAB_ARCHIVING_GATE"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, marker)
	}))
	defer upstream.Close()

	mux, inst, id, tabID := newWebTabProxyFixtureWithInstance(t, upstream.URL)

	// Live: proxies through — the control.
	if rec := proxyGet(t, mux, id, tabID, ""); rec.Code != http.StatusOK {
		t.Fatalf("live web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Mid-archive: liveness is still live, only the op has moved. This is the state
	// ArchiveSession sits in while tmux comes down and the worktree moves.
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("BeginArchive: %v", err)
	}
	if inst.IsArchived() {
		t.Fatal("fixture invalid: a mid-archive session must not read as settled-archived, or this test proves nothing")
	}
	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("mid-archive web tab: status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("mid-archive web tab: the upstream was proxied; archive must be inert from the moment it starts, not only once it commits")
	}

	// Committed: still refused, now on the settled state.
	if err := inst.Transition(session.CommitArchive()); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	rec = proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("archived web tab: status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "archived") {
		t.Fatalf("archived web tab: body = %q, want an actionable archived message", body)
	}
}

// TestWebTabProxy_ServesDuringRestore pins the deliberate other half of the
// serve gate: a restore is NOT inert. BeginRestore moves the session to
// LiveLost + OpRestoring, but both callers (RestoreArchived, undoCommittedArchive)
// move the worktree home BEFORE that transition, so there is no mid-move window to
// protect — and the tab served here is the same one the session serves a moment
// later when the restore completes. Gating it would only blank a pane that is
// about to work, so the inert predicate stops at archive/kill.
func TestWebTabProxy_ServesDuringRestore(t *testing.T) {
	const marker = "AF_WEBTAB_RESTORE_SERVES"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, marker)
	}))
	defer upstream.Close()

	mux, inst, id, tabID := newWebTabProxyFixtureWithInstance(t, upstream.URL)

	inst.SetStatusForTest(session.Archived)
	if rec := proxyGet(t, mux, id, tabID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("archived web tab: status = %d, want 404", rec.Code)
	}

	// Restore in flight: liveness Lost + OpRestoring, worktree already home.
	if err := inst.Transition(session.BeginRestore()); err != nil {
		t.Fatalf("BeginRestore: %v", err)
	}
	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restoring web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("restoring web tab: body = %q, want the upstream content", rec.Body.String())
	}
}

// TestWebTabProxy_ServesLoopbackTarget is the headline proxy test: a web tab
// pointing at a loopback HTTP server is reverse-proxied by the daemon, so a
// same-origin GET /v1/webtab/{id}/{tabId}/ returns the server's content.
func TestWebTabProxy_ServesLoopbackTarget(t *testing.T) {
	const marker = "AF_WEBTAB_PROXY_OK"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><body>%s path=%s</body></html>", marker, r.URL.Path)
	}))
	defer upstream.Close()

	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// The trailing-slash root and a sub-path both proxy through. A root-URL target
	// already mirrors itself, so neither redirects.
	for _, sub := range []string{"", "assets/app.js"} {
		rec := proxyGet(t, mux, id, tabID, sub)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", sub, rec.Code, rec.Body.String())
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if got := string(body); !contains(got, marker) {
			t.Fatalf("GET sub=%q: body %q missing marker %q", sub, got, marker)
		}
		// The proxied path is the remainder under the prefix, forwarded verbatim —
		// proof the prefix was stripped and nothing was re-based onto it.
		if got := string(body); !contains(got, "path=/"+sub) {
			t.Fatalf("GET sub=%q: upstream saw wrong path in %q", sub, got)
		}
	}
}

// newStaticFileUpstream starts an upstream that behaves like a static file server
// (python -m http.server) rooted at a subdirectory: it serves exactly
// /app/viewer.html, /app/x.css and /shared.css, and 404s every other path —
// including any path a prefix-mangling proxy would invent.
func newStaticFileUpstream(t *testing.T, viewerBody, cssBody, sharedBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/viewer.html":
			fmt.Fprint(w, viewerBody)
		case "/app/x.css":
			fmt.Fprint(w, cssBody)
		case "/shared.css":
			fmt.Fprint(w, sharedBody)
		default:
			http.Error(w, "404 File not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWebTabProxy_MirrorsSubdirectoryTarget is the load-bearing test for the
// mirror-path URL model, and the one that retires #1806's documented
// subdirectory-target limits.
//
// The browser URL mirrors the upstream path, so the daemon forwards the remainder
// verbatim and every relative URL on the page resolves at the SAME DEPTH the dev
// server serves it at — including a PARENT-relative one, which lands back inside
// the prefix instead of escaping it.
func TestWebTabProxy_MirrorsSubdirectoryTarget(t *testing.T) {
	const viewerBody = "AF_VIEWER_DOC"
	const cssBody = "AF_VIEWER_CSS"
	const sharedBody = "AF_SHARED_CSS"
	upstream := newStaticFileUpstream(t, viewerBody, cssBody, sharedBody)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/viewer.html")

	cases := []struct {
		name string
		sub  string
		want string
	}{
		// The mirrored path fetches the subdirectory document itself — the case
		// #1806 could not serve.
		{name: "subdirectory target document loads", sub: "app/viewer.html", want: viewerBody},
		// A SIBLING asset: the browser resolves "x.css" on .../app/viewer.html to
		// .../app/x.css, which mirrors upstream /app/x.css.
		{name: "sibling asset resolves beside the document", sub: "app/x.css", want: cssBody},
		// A PARENT-relative asset: the browser resolves "../shared.css" to
		// .../<tabId>/shared.css — still inside the prefix (depth is preserved), and
		// upstream /shared.css. This is what a flat prefix could never express.
		{name: "parent-relative asset resolves inside the prefix", sub: "shared.css", want: sharedBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxyGet(t, mux, id, tabID, tc.sub)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", tc.sub, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !contains(got, tc.want) {
				t.Fatalf("GET sub=%q: body = %q, want it to contain %q", tc.sub, got, tc.want)
			}
		})
	}
}

// TestWebTabProxy_RootRedirectsToTargetPath verifies that the credential cleanup
// and path-mirror redirects compose: first the private query is removed in place,
// then the clean cookie-backed request is sent to the target's own path.
func TestWebTabProxy_RootRedirectsToTargetPath(t *testing.T) {
	upstream := newStaticFileUpstream(t, "doc", "css", "shared")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/viewer.html")

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/?af_webtab_token=tok", id, tabID), nil)
	bootstrap, clean := followWebTabTokenBootstrap(t, mux, req)

	wantCleanRoot := fmt.Sprintf("/v1/webtab/%s/%s/", id, tabID)
	if got := bootstrap.Header().Get("Location"); got != wantCleanRoot {
		t.Fatalf("bootstrap Location = %q, want %q", got, wantCleanRoot)
	}
	if clean.Code != http.StatusFound {
		t.Fatalf("clean root: status = %d, want 302 (body: %s)", clean.Code, clean.Body.String())
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/app/viewer.html", id, tabID)
	if got := clean.Header().Get("Location"); got != want {
		t.Fatalf("root: Location = %q, want %q", got, want)
	}
}

// TestWebTabProxy_RootTargetDoesNotRedirect guards the redirect loop: a root-URL
// target already mirrors itself, so it must be proxied in place rather than
// redirected to its own path forever.
func TestWebTabProxy_RootTargetDoesNotRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "root-ok")
	}))
	defer upstream.Close()

	for _, target := range []string{upstream.URL, upstream.URL + "/"} {
		mux, id, tabID := newWebTabProxyFixture(t, target)
		rec := proxyGet(t, mux, id, tabID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("target %q: status = %d, want 200 (a root target must not redirect)", target, rec.Code)
		}
	}
}

// TestMirrorRootRedirect pins where a bare tab-root request is sent so the browser
// URL mirrors the target, and — critically — which targets must NOT redirect at all
// (a root target redirecting to its own path is an infinite loop).
func TestMirrorRootRedirect(t *testing.T) {
	const prefix = "/v1/webtab/sess/tab-1"
	cases := []struct {
		target     string
		rawQuery   string
		want       string
		wantRedir  bool
		reasonName string
	}{
		// Path-bearing targets mirror their path.
		{target: "http://localhost:8899/viewer.html", want: prefix + "/viewer.html", wantRedir: true},
		{target: "http://localhost:8899/app/viewer.html", want: prefix + "/app/viewer.html", wantRedir: true},
		// A directory-style target keeps its trailing slash, so the app's relative
		// URLs resolve BENEATH it rather than beside it.
		{target: "http://localhost:8899/app/", want: prefix + "/app/", wantRedir: true},
		// Root targets already mirror themselves — no redirect, no loop.
		{target: "http://localhost:8899", wantRedir: false},
		{target: "http://localhost:8899/", wantRedir: false},
		// Ordinary app query bytes ride along. The daemon token is already removed
		// before this helper can run.
		{
			target: "http://localhost:8899/app/viewer.html", rawQuery: "doc=hello%20world",
			want: prefix + "/app/viewer.html?doc=hello%20world", wantRedir: true,
		},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.target)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.target, err)
		}
		got, ok := mirrorRootRedirect(prefix, u, tc.rawQuery)
		if ok != tc.wantRedir {
			t.Errorf("mirrorRootRedirect(%q): redirect = %v, want %v", tc.target, ok, tc.wantRedir)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("mirrorRootRedirect(%q, %q) = %q, want %q", tc.target, tc.rawQuery, got, tc.want)
		}
	}
}

// TestWebTabProxy_RejectsNonLoopbackTarget verifies the daemon refuses to proxy a
// non-loopback (external) target — no open proxy / SSRF. External URLs are iframed
// directly by the web UI, never routed through the daemon.
func TestWebTabProxy_RejectsNonLoopbackTarget(t *testing.T) {
	mux, id, tabID := newWebTabProxyFixture(t, "https://example.com")

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("external target: status = %d, want 400", rec.Code)
	}
}

// TestWebTabProxy_RejectsNonWebTab verifies proxying a non-web tab (the agent tab)
// is refused, addressed by the agent tab's own stable id.
func TestWebTabProxy_RejectsNonWebTab(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
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
	const title = "webproxy"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: upstream.URL}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	mux := newHTTPMux(&controlServer{manager: manager})
	agentTabID := inst.GetTabs()[0].ID

	rec := proxyGet(t, mux, inst.ID, agentTabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent tab proxy: status = %d, want 404", rec.Code)
	}
}

// TestWebTabProxy_ClosingLowerTabKeepsPreviewOnItsOwnServer is the #1810
// misroute-prevention proof, and the whole reason the route is id-keyed.
//
// With [agent, webA(:A), webB(:B)], an open pane on webB addresses it by id.
// Closing the LOWER tab webA shifts webB from ordinal 2 to 1. An ordinal-keyed
// route would then silently relay a DIFFERENT dev server to a frame that never
// navigated — HTTP 200, no error, wrong app. By id, webB keeps serving webB.
func TestWebTabProxy_ClosingLowerTabKeepsPreviewOnItsOwnServer(t *testing.T) {
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "DEVSERVER_A")
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "DEVSERVER_B")
	}))
	defer serverB.Close()

	mux, _, id, ids, closeWebTab := newWebTabProxyFixtureN(t, serverA.URL, serverB.URL)
	tabA, tabB := ids[0], ids[1]

	// Before: webB's id serves B.
	if rec := proxyGet(t, mux, id, tabB, ""); !contains(rec.Body.String(), "DEVSERVER_B") {
		t.Fatalf("before close: body = %q, want DEVSERVER_B", rec.Body.String())
	}

	// The developer closes an UNRELATED, LOWER tab. Every higher ordinal shifts down.
	closeWebTab(0)

	// After: the SAME url still serves B. This is the assertion an ordinal-keyed
	// route fails — it would return DEVSERVER_A here, with a 200 and no complaint.
	rec := proxyGet(t, mux, id, tabB, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("after close: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "DEVSERVER_B") {
		t.Fatalf("after close: body = %q, want DEVSERVER_B — the pane was repointed at another dev server", got)
	}

	// And the CLOSED tab's id resolves to nothing: a clean 404 naming the id, never
	// a silent bind to whatever now sits at its old ordinal.
	stale := proxyGet(t, mux, id, tabA, "")
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale id: status = %d, want 404 (body: %s)", stale.Code, stale.Body.String())
	}
	if got := stale.Body.String(); !contains(got, tabA) {
		t.Fatalf("stale id: body = %q, want it to name the unknown tab id %q", got, tabA)
	}
}

// TestWebTabProxy_UnknownTabID404s: an id that never existed is refused outright,
// so a stale/garbled address can never resolve to some other tab.
func TestWebTabProxy_UnknownTabID404s(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	mux, id, _ := newWebTabProxyFixture(t, upstream.URL)

	// Note "1" — the old ORDINAL address. There is no ordinal fallback: it is just
	// an unknown id now, and it must 404 rather than resolve to the tab at index 1.
	for _, bogus := range []string{"no-such-id", "1", "0"} {
		rec := proxyGet(t, mux, id, bogus, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("tab id %q: status = %d, want 404", bogus, rec.Code)
		}
	}
}

// TestWebTabProxy_SetsScopedTokenCookie verifies that when a bearer token
// authorizes the request (via the daemon-private ?af_webtab_token=), the handler
// sets the path-scoped af_webtab_token cookie so an iframe's sub-resource GETs stay
// authorized.
func TestWebTabProxy_SetsScopedTokenCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "?af_webtab_token=secret-tok")

	found := cookieNamed(rec, webtabTokenCookie)
	if found == nil {
		t.Fatal("expected af_webtab_token cookie to be set for a token-authorized request")
	}
	if found.Value != "secret-tok" || found.Path != webtabPathPrefix || !found.HttpOnly {
		t.Fatalf("cookie = %+v, want value=secret-tok path=%s HttpOnly", found, webtabPathPrefix)
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie SameSite = %v, want Strict", found.SameSite)
	}
}

// TestWebTabProxy_QueryTokenIsOnlyABootstrapCredential pins the browser-facing
// half of the token boundary. The private query parameter may authorize exactly
// one request, but application code must never render at that credential-bearing
// URL: the handler first stores the credential in an HttpOnly cookie and redirects
// to the same path with only its own parameter removed. The clean, cookie-backed
// follow-up is the first request allowed to reach the preview app.
func TestWebTabProxy_QueryTokenIsOnlyABootstrapCredential(t *testing.T) {
	var upstreamCalls atomic.Int32
	seenQuery := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		seenQuery <- r.URL.RawQuery
		fmt.Fprint(w, "app-rendered")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	bootstrapPath := fmt.Sprintf(
		"/v1/webtab/%s/%s/?doc=hello%%20world&af%%5Fwebtab%%5Ftoken=fixture-token&z=1",
		id, tabID,
	)
	bootstrap := httptest.NewRecorder()
	mux.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, bootstrapPath, nil))

	if bootstrap.Code != http.StatusTemporaryRedirect {
		t.Fatalf("bootstrap status = %d, want 307 (body: %s)", bootstrap.Code, bootstrap.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("bootstrap reached the preview app %d time(s), want 0", got)
	}
	wantLocation := fmt.Sprintf("/v1/webtab/%s/%s/?doc=hello%%20world&z=1", id, tabID)
	if got := bootstrap.Header().Get("Location"); got != wantLocation {
		t.Fatalf("bootstrap Location = %q, want %q", got, wantLocation)
	}
	if strings.Contains(bootstrap.Header().Get("Location"), "fixture-token") ||
		strings.Contains(bootstrap.Body.String(), "fixture-token") {
		t.Fatal("bootstrap credential is readable in the redirect URL or body")
	}
	if got := bootstrap.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("bootstrap Cache-Control = %q, want no-store", got)
	}
	if got := bootstrap.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("bootstrap Referrer-Policy = %q, want no-referrer", got)
	}
	cookie := cookieNamed(bootstrap, webtabTokenCookie)
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("bootstrap cookie = %+v, want an HttpOnly token cookie", cookie)
	}

	clean := httptest.NewRecorder()
	cleanReq := httptest.NewRequest(http.MethodGet, wantLocation, nil)
	cleanReq.AddCookie(cookie)
	mux.ServeHTTP(clean, cleanReq)
	if clean.Code != http.StatusOK {
		t.Fatalf("clean follow-up status = %d, want 200 (body: %s)", clean.Code, clean.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("clean follow-up reached the preview app %d time(s), want 1", got)
	}
	if got := <-seenQuery; got != "doc=hello%20world&z=1" {
		t.Fatalf("upstream RawQuery = %q, want exact clean app query", got)
	}
}

// TestWebTabProxy_QueryTokenCleanupKeepsMirroredPath ensures a non-root target
// cannot bypass the bootstrap boundary. The redirect stays on the exact mirrored
// path and preserves the app's raw query bytes and order; it does not bounce via
// the tab root or the upstream target URL.
func TestWebTabProxy_QueryTokenCleanupKeepsMirroredPath(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		fmt.Fprintf(w, "path=%s query=%s", r.URL.EscapedPath(), r.URL.RawQuery)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/viewer.html")

	path := fmt.Sprintf(
		"/v1/webtab/%s/%s/app/viewer.html?z=1&af_webtab_token=fixture-token&a=hello%%20world",
		id, tabID,
	)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("bootstrap status = %d, want 307 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("bootstrap reached the preview app %d time(s), want 0", got)
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/app/viewer.html?z=1&a=hello%%20world", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("bootstrap Location = %q, want %q", got, want)
	}
}

// TestWebTabProxy_QueryTokenCleansBeforeTargetResolution pins the ordering that
// matters for VS Code tabs: resolving their target can start an editor. Even a
// server with no manager must perform the credential cleanup first, so no target
// lookup or application process can precede the clean browser URL.
func TestWebTabProxy_QueryTokenCleansBeforeTargetResolution(t *testing.T) {
	mux := newHTTPMux(&controlServer{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/webtab/session/tab/?af_webtab_token=fixture-token", nil))

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 before the missing-manager check (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/v1/webtab/session/tab/" {
		t.Fatalf("Location = %q, want the clean request path", got)
	}
}

// TestWebTabProxy_TokenCookieSecureTracksScheme is the #1808 regression test.
//
// The daemon serves PLAIN HTTP, and a browser silently DROPS a Secure cookie
// delivered over http:// to a non-localhost origin — so flagging it
// unconditionally killed the cookie in the one deployment it exists for (a network
// peer with require_token=true), 401ing every iframe sub-resource. Secure must
// therefore track the scheme the request actually arrived over: omitted on the
// daemon's own plain-HTTP listener, set when a front proxy terminated TLS.
func TestWebTabProxy_TokenCookieSecureTracksScheme(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	cases := []struct {
		name       string
		fwdProto   string
		wantSecure bool
	}{
		// The real remote case: plain HTTP straight to the daemon. A Secure cookie
		// here is a dropped cookie.
		{name: "plain HTTP listener omits Secure", fwdProto: "", wantSecure: false},
		// The recommended deployment: TLS terminated at a front proxy, plain HTTP to
		// the daemon. The browser IS on https, so the cookie can and should be Secure.
		{name: "TLS-terminating front proxy sets Secure", fwdProto: "https", wantSecure: true},
		{name: "X-Forwarded-Proto is case-insensitive", fwdProto: "HTTPS", wantSecure: true},
		// A proxy chain appends; the FIRST entry is the original client's scheme.
		{name: "proxy chain uses the first entry", fwdProto: "https, http", wantSecure: true},
		{name: "explicit http omits Secure", fwdProto: "http", wantSecure: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/v1/webtab/%s/%s/?af_webtab_token=tok", id, tabID), nil)
			// A NETWORK peer (not loopback) — the only case the cookie exists for.
			req.RemoteAddr = "172.17.0.4:54321"
			if tc.fwdProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			mux.ServeHTTP(rec, req)

			c := cookieNamed(rec, webtabTokenCookie)
			if c == nil {
				t.Fatal("af_webtab_token cookie was not set")
			}
			if c.Secure != tc.wantSecure {
				t.Fatalf("Secure = %v, want %v (X-Forwarded-Proto: %q)", c.Secure, tc.wantSecure, tc.fwdProto)
			}
			// The rest of the cookie's protection is unchanged either way.
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != webtabPathPrefix {
				t.Fatalf("cookie lost its other protections: %+v", c)
			}
		})
	}
}

// TestWebTabProxy_RemotePeerSubResourcesAuthorize is the end-to-end #1808 proof at
// the gate: a NETWORK peer over plain HTTP with require_token=true authorizes the
// top-level navigation with ?af_webtab_token. The handler cleans that URL before
// application code renders, and the cookie it gets back then authorizes both the
// clean navigation and iframe sub-resource GETs — which carry neither header nor query.
// Before the fix the cookie was Secure, the browser dropped it, and every one of
// those 401'd.
func TestWebTabProxy_RemotePeerSubResourcesAuthorize(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "UPSTREAM path=%s", r.URL.Path)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// The daemon's web listener posture: token required, loopback exempt. Our peer
	// is NOT loopback, so it must present the token.
	gate := &authGate{
		expectedToken:  func() (string, error) { return "secret-tok", nil },
		loopbackExempt: true,
	}
	authed := withAuth(mux, gate, nil)

	// 1. The top-level navigation authorizes via ?af_webtab_token (an iframe src
	//    cannot set a header), gets the cookie, and follows the clean URL.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/?af_webtab_token=secret-tok", id, tabID), nil)
	req.RemoteAddr = "172.17.0.4:54321"
	bootstrap, clean := followWebTabTokenBootstrap(t, authed, req)
	if clean.Code != http.StatusOK {
		t.Fatalf("clean top-level nav: status = %d, want 200 (body: %s)", clean.Code, clean.Body.String())
	}
	cookie := cookieNamed(bootstrap, webtabTokenCookie)
	if cookie == nil {
		t.Fatal("no af_webtab_token cookie for the token-authorized navigation")
	}
	if cookie.Secure {
		t.Fatal("cookie is Secure over plain HTTP: a real browser drops it and every sub-resource 401s (#1808)")
	}

	// 2. The framed app's sub-resource GET: no Authorization header, no
	//    ?af_webtab_token — only the cookie the browser stored. It must be authorized.
	sub := httptest.NewRecorder()
	subReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/rel.css", id, tabID), nil)
	subReq.RemoteAddr = "172.17.0.4:54321"
	subReq.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	authed.ServeHTTP(sub, subReq)
	if sub.Code != http.StatusOK {
		t.Fatalf("sub-resource: status = %d, want 200 — the iframe's assets 401 (body: %s)", sub.Code, sub.Body.String())
	}
	if got := sub.Body.String(); !contains(got, "path=/rel.css") {
		t.Fatalf("sub-resource: upstream saw %q, want path=/rel.css", got)
	}

	// 3. The same sub-resource WITHOUT the cookie is still refused — the cookie is a
	//    real credential, not a hole.
	no := httptest.NewRecorder()
	noReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/rel.css", id, tabID), nil)
	noReq.RemoteAddr = "172.17.0.4:54321"
	authed.ServeHTTP(no, noReq)
	if no.Code != http.StatusUnauthorized {
		t.Fatalf("cookieless sub-resource: status = %d, want 401", no.Code)
	}
}

// TestWebTabProxy_ForwardsCookiesBothDirections verifies cookie-backed dev apps
// work in the iframe: the client's app cookies are forwarded upstream (with the
// daemon's own token cookie stripped), and the upstream's Set-Cookie is relayed
// back re-scoped under the tab's proxy path (Domain dropped).
func TestWebTabProxy_ForwardsCookiesBothDirections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the Cookie the upstream received so the test can assert what was forwarded.
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "appsess", Value: "abc", Path: "/", Domain: "localhost"})
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/", id, tabID), nil)
	// The browser sends both the daemon token cookie and the app's own cookie.
	req.Header.Set("Cookie", webtabTokenCookie+"=daemon-tok; appsess=xyz")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Upstream must have seen the app cookie but NOT the daemon token cookie.
	echoed := rec.Header().Get("X-Echo-Cookie")
	if !contains(echoed, "appsess=xyz") {
		t.Fatalf("upstream Cookie %q missing app cookie appsess=xyz", echoed)
	}
	if contains(echoed, webtabTokenCookie) {
		t.Fatalf("upstream Cookie %q must not carry the daemon token cookie", echoed)
	}

	// The upstream Set-Cookie must be relayed back, re-scoped under this tab's proxy
	// path, Domain dropped.
	wantPath := fmt.Sprintf("/v1/webtab/%s/%s/", id, tabID)
	app := cookieNamed(rec, "appsess")
	if app == nil {
		t.Fatal("upstream Set-Cookie (appsess) was not relayed to the client")
	}
	if app.Path != wantPath {
		t.Fatalf("relayed cookie Path = %q, want %q", app.Path, wantPath)
	}
	if app.Domain != "" {
		t.Fatalf("relayed cookie Domain = %q, want empty (defaults to proxy host)", app.Domain)
	}
}

// TestWebTabProxy_SubdirCookieRidesTheMirroredPath is the cookie half of the
// mirror model: a dev app that scopes a cookie to its OWN subdirectory (Path=/app —
// login/session/CSRF cookies routinely do) must have it re-scoped by a pure
// PREFIX-PREPEND, and the result must be a path the browser will actually send the
// cookie back on.
//
// Because the browser path mirrors upstream, Path=/app becomes
// /v1/webtab/<sid>/<tab>/app — exactly the prefix of the requests the app scoped it
// to, so the cookie rides along on /app/* and nothing else. A model that did not
// mirror depth could not express this.
func TestWebTabProxy_SubdirCookieRidesTheMirroredPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/login" {
			http.SetCookie(w, &http.Cookie{Name: "appsess", Value: "abc", Path: "/app"})
		}
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/index.html")

	// The app sets a cookie scoped to its own subdirectory.
	rec := proxyGet(t, mux, id, tabID, "app/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", rec.Code)
	}
	c := cookieNamed(rec, "appsess")
	if c == nil {
		t.Fatal("the app's Path=/app cookie was not relayed")
	}
	wantPath := fmt.Sprintf("/v1/webtab/%s/%s/app", id, tabID)
	if c.Path != wantPath {
		t.Fatalf("cookie Path = %q, want %q (a pure prefix-prepend of /app)", c.Path, wantPath)
	}

	// A real browser sends a Path=/v1/webtab/<sid>/<tab>/app cookie on any request
	// under that path — which, mirrored, is exactly the app's own /app/* requests.
	// Prove the round trip reaches upstream.
	follow := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/app/data.json", id, tabID), nil)
	req.AddCookie(&http.Cookie{Name: "appsess", Value: "abc"})
	mux.ServeHTTP(follow, req)
	if follow.Code != http.StatusOK {
		t.Fatalf("follow-up: status = %d, want 200", follow.Code)
	}
	if got := follow.Header().Get("X-Echo-Cookie"); !contains(got, "appsess=abc") {
		t.Fatalf("upstream Cookie = %q, want it to carry appsess=abc on the /app request", got)
	}
	if got := follow.Body.String(); !contains(got, "path=/app/data.json") {
		t.Fatalf("upstream saw %q, want path=/app/data.json", got)
	}
}

// TestWebTabProxy_StripsFramingHeaders verifies the proxy removes a dev server's
// X-Frame-Options and the frame-ancestors CSP directive, so a same-origin preview
// always frames, while leaving other CSP directives intact.
func TestWebTabProxy_StripsFramingHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "" {
		t.Fatalf("X-Frame-Options = %q, want stripped", xfo)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if contains(csp, "frame-ancestors") {
		t.Fatalf("CSP still carries frame-ancestors: %q", csp)
	}
	if !contains(csp, "default-src 'self'") {
		t.Fatalf("CSP lost its other directives: %q", csp)
	}
}

// redirectingUpstream serves a redirect to loc with the given status on every
// request, for the Location-rewrite tests (#1843).
func redirectingUpstream(t *testing.T, status int, loc string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(status)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// TestWebTabProxy_RedirectLocationStaysInPrefix is the #1843 repro: a dev app that
// 302s to an absolute path (the shape every login flow uses) must land back inside
// the tab prefix. Un-rewritten, the browser follows "/login" to the daemon's own
// origin and 404s.
func TestWebTabProxy_RedirectLocationStaysInPrefix(t *testing.T) {
	upstream := redirectingUpstream(t, http.StatusFound, "/app/")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "some/path")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/app/", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (the redirect escaped the tab prefix)", got, want)
	}
}

// TestWebTabProxy_RedirectLocationKeepsEncodedLeadingSlash is the end-to-end proof
// of the leading-%2F fix, on the surface it actually reaches the user: an upstream
// 302 whose Location's FIRST segment is an encoded slash must arrive at the browser
// still encoded.
//
// Flattening it is not cosmetic — /%2Ffoo and //foo are different upstream routes,
// so the browser would follow the redirect to a resource the app never named, and
// the proxy would forward that wrong path back upstream. This is the same bug class
// the request direction already guards (TestWebTabProxy_PreservesEncodedSlash);
// this closes it on the response direction.
func TestWebTabProxy_RedirectLocationKeepsEncodedLeadingSlash(t *testing.T) {
	upstream := redirectingUpstream(t, http.StatusFound, "/%2Ffoo")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "some/path")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/%%2Ffoo", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q — the encoded leading slash was flattened, "+
			"pointing the browser at a route the app never named", got, want)
	}
}

// TestWebTabProxy_AbsoluteSelfRedirectRewritten covers the app redirecting to its
// OWN absolute URL — equally common, and equally unreachable for a remote viewer,
// since the upstream origin is loopback on the daemon's machine. The path is kept
// verbatim (mirror-path model) and the query rides along.
func TestWebTabProxy_AbsoluteSelfRedirectRewritten(t *testing.T) {
	// selfURL is read only from inside the handler, which cannot run until the
	// server is up and the assignment below has happened.
	var selfURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", selfURL+"/dash/?tab=1")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer upstream.Close()
	selfURL = upstream.URL
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "x")

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/dash/?tab=1", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (absolute self-redirect must be re-pathed into the prefix)", got, want)
	}
}

// TestWebTabProxy_ForeignRedirectPassesThrough is the other half of the rule: an
// off-site redirect (an OAuth provider, say) is a REAL navigation away from the
// frame. Rewriting it would point the browser at a prefix the daemon then refuses
// to proxy — only loopback targets are proxied — and rehost a foreign origin under
// ours.
func TestWebTabProxy_ForeignRedirectPassesThrough(t *testing.T) {
	const foreign = "https://accounts.example.com/oauth?client_id=abc"
	upstream := redirectingUpstream(t, http.StatusFound, foreign)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "login")

	if got := rec.Header().Get("Location"); got != foreign {
		t.Fatalf("Location = %q, want %q untouched", got, foreign)
	}
}

// TestWebTabProxy_NetworkPathRedirectPassesThrough is the OAuth case in the spelling
// net/url does not read as a host. "///accounts.example.com/oauth" is a navigation to
// accounts.example.com in every browser, but Go parses it with an empty Host and the
// whole reference as a path — so the absolute-path rule captured it into the tab and
// sent the user to the dev server's /accounts.example.com/oauth instead of to the
// identity provider.
func TestWebTabProxy_NetworkPathRedirectPassesThrough(t *testing.T) {
	const foreign = "///accounts.example.com/oauth"
	upstream := redirectingUpstream(t, http.StatusFound, foreign)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "login")

	if got := rec.Header().Get("Location"); got != foreign {
		t.Fatalf("Location = %q, want %q untouched (a network-path redirect is a foreign host)", got, foreign)
	}
}

// TestWebTabProxy_RedirectDotSegmentsNormalized pins the dot-segment rule end to end.
// The browser resolves "/../login" AFTER reading the header, so prefixing it verbatim
// yields ".../<tab>/../login" — which normalizes to /v1/webtab/<sid>/login, a path
// naming no tab at all. The upstream meant /login; that is what must ride the prefix.
func TestWebTabProxy_RedirectDotSegmentsNormalized(t *testing.T) {
	upstream := redirectingUpstream(t, http.StatusFound, "/../login")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "deep/page")

	want := fmt.Sprintf("/v1/webtab/%s/%s/login", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (a dot segment ate the tab prefix)", got, want)
	}
}

// TestWebTabProxy_RedirectKeepsLeadingEncodedSlash is the encoding half of the same
// header: %2F is a literal path character, not a separator, and an app that redirects
// to /%2Ffoo names a different resource than /foo. Prefixing Path and RawPath
// independently desynced them, and url.String() answers a desync by dropping RawPath.
func TestWebTabProxy_RedirectKeepsLeadingEncodedSlash(t *testing.T) {
	upstream := redirectingUpstream(t, http.StatusFound, "/%2Ffoo")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "x")

	want := fmt.Sprintf("/v1/webtab/%s/%s/%%2Ffoo", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (the escaped slash was decoded away)", got, want)
	}
}

// TestRewriteUpstreamRefNeverEmitsNetworkPath guards the one way this rewrite could
// hand out an OPEN REDIRECT. A real tab prefix is always "/v1/webtab/<sid>/<tab>", so
// the prefixed path can never begin "//" — but normalization can produce a leading
// "//" from an upstream "/..//evil.com/x", and with no prefix in front of it that is
// a Location the browser reads as a foreign host. The rewrite must refuse instead.
func TestRewriteUpstreamRefNeverEmitsNetworkPath(t *testing.T) {
	target, err := url.Parse("http://localhost:3000")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	for _, prefix := range []string{"", "/"} {
		got, ok := rewriteUpstreamRef("/..//evil.com/x", prefix, target)
		if ok {
			t.Fatalf("rewriteUpstreamRef(prefix=%q) = %q, true; want pass-through, not a network-path Location",
				prefix, got)
		}
	}
	// With a real prefix in front the same reference is an ordinary local path.
	want := "/v1/webtab/sess/tab-1//evil.com/x"
	if got, ok := rewriteUpstreamRef("/..//evil.com/x", "/v1/webtab/sess/tab-1", target); !ok || got != want {
		t.Fatalf("rewriteUpstreamRef = %q, %v; want %q, true", got, ok, want)
	}
}

// TestWebTabProxy_RefreshHeaderRewritten covers the delayed-redirect spelling of the
// same escape.
func TestWebTabProxy_RefreshHeaderRewritten(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Refresh", "5; url=/app/done")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "start")

	want := fmt.Sprintf("5; url=/v1/webtab/%s/%s/app/done", id, tabID)
	if got := rec.Header().Get("Refresh"); got != want {
		t.Fatalf("Refresh = %q, want %q", got, want)
	}
}

// TestWebTabProxy_NonRedirectLocationUntouched pins the status gate: a 2xx Location
// IDENTIFIES a resource rather than naming somewhere to navigate, so prefixing it
// would corrupt an id the app's own JS may compare against.
func TestWebTabProxy_NonRedirectLocationUntouched(t *testing.T) {
	upstream := redirectingUpstream(t, http.StatusCreated, "/api/items/5")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "api/items")

	if got := rec.Header().Get("Location"); got != "/api/items/5" {
		t.Fatalf("Location = %q, want %q untouched on a 201", got, "/api/items/5")
	}
}

// TestRewriteUpstreamRef pins the rewrite rule itself — the cases the proxy tests
// above can only reach one at a time.
func TestRewriteUpstreamRef(t *testing.T) {
	const prefix = "/v1/webtab/sess/tab-1"
	const dflt = "http://localhost:3000"
	cases := []struct {
		name   string
		target string // upstream the tab proxies; defaults to dflt
		ref    string
		want   string // "" means: passed through untouched
	}{
		{name: "absolute path", ref: "/login", want: prefix + "/login"},
		{name: "absolute path with query and fragment", ref: "/a?b=c#d", want: prefix + "/a?b=c#d"},
		{name: "root", ref: "/", want: prefix + "/"},
		{name: "same origin", ref: "http://localhost:3000/app/", want: prefix + "/app/"},
		{name: "same origin, origin only", ref: "http://localhost:3000?x=1", want: prefix + "/?x=1"},
		{name: "same origin, host case-insensitive", ref: "http://LOCALHOST:3000/x", want: prefix + "/x"},
		{name: "protocol relative, same host", ref: "//localhost:3000/x", want: prefix + "/x"},
		{name: "encoded path kept verbatim", ref: "/a%2Fb", want: prefix + "/a%2Fb"},
		// The LEADING-position encoded slash is its own case, and the one the
		// side-by-side join got wrong: url.Parse gives "/%2Ffoo" the decoded Path
		// "//foo", whose TrimLeft eats BOTH slashes while the escaped form loses only
		// its one real slash. Path and RawPath then disagreed, net/url dropped the raw
		// form, and the redirect flattened %2Ffoo to foo — silently sending the browser
		// to a different upstream route than the app named.
		{name: "encoded leading slash kept verbatim", ref: "/%2Ffoo", want: prefix + "/%2Ffoo"},
		{name: "encoded leading slash on a deeper path", ref: "/%2Fa/b.css", want: prefix + "/%2Fa/b.css"},
		{name: "encoded leading slash on an absolute self-redirect",
			ref: "http://localhost:3000/%2Ffoo", want: prefix + "/%2Ffoo"},
		{name: "encoded leading slash with a query", ref: "/%2Ffoo?a=1", want: prefix + "/%2Ffoo?a=1"},
		// The default port is implicit on one side and explicit on the other; they
		// name the same server either way.
		{name: "default port explicit matches implicit target",
			target: "http://localhost", ref: "http://localhost:80/x", want: prefix + "/x"},
		{name: "implicit ref port matches explicit default target",
			target: "http://localhost:80", ref: "http://localhost/x", want: prefix + "/x"},
		{name: "ipv6 loopback same origin",
			target: "http://[::1]:3000", ref: "http://[::1]:3000/x", want: prefix + "/x"},
		{name: "relative stays relative", ref: "next/page"},
		{name: "dot relative stays relative", ref: "../up"},
		{name: "foreign host", ref: "https://example.com/x"},
		// The spellings whose BROWSER reading diverges from net/url's parse. Each
		// one reached master rewriting to something the browser would not follow
		// where the upstream pointed.
		//
		// A LEADING %2F decodes to a second slash in Path but not in RawPath, so
		// prefixing the two independently desynced them and url.String() dropped the
		// encoding — silently renaming the resource the app named.
		{name: "leading encoded slash keeps its encoding", ref: "/%2Ffoo", want: prefix + "/%2Ffoo"},
		{name: "leading encoded slash on a same-origin absolute",
			ref: "http://localhost:3000/%2Ffoo", want: prefix + "/%2Ffoo"},
		{name: "real double slash in a same-origin path survives",
			ref: "http://localhost:3000//foo", want: prefix + "//foo"},
		// DOT SEGMENTS are resolved by the browser AFTER it reads the header, so an
		// un-normalized one eats the prefix it was just given and escapes the tab.
		{name: "dot segments resolved before prefixing", ref: "/../login", want: prefix + "/login"},
		{name: "encoded dot segments resolved too", ref: "/%2e%2e/login", want: prefix + "/login"},
		{name: "encoded dot segments are case insensitive", ref: "/%2E%2E/login", want: prefix + "/login"},
		{name: "dot segments cannot climb past root", ref: "/../../x", want: prefix + "/x"},
		{name: "interior dot segments resolved", ref: "/a/b/../c", want: prefix + "/a/c"},
		{name: "single dot segment resolved", ref: "/a/./b", want: prefix + "/a/b"},
		{name: "trailing single dot keeps the directory slash", ref: "/a/.", want: prefix + "/a/"},
		{name: "trailing double dot keeps the directory slash", ref: "/a/..", want: prefix + "/"},
		{name: "dot segments in the query are left alone", ref: "/x?q=/../y", want: prefix + "/x?q=/../y"},
		// A NETWORK-PATH reference names a host to the browser, but net/url reports
		// the longer slash runs as a plain local path. Foreign is foreign: pass it
		// through, or an OAuth handoff gets stranded in the frame.
		{name: "triple-slash network path is foreign", ref: "///accounts.example.com/oauth"},
		{name: "backslash network path is foreign", ref: "/\\accounts.example.com/oauth"},
		// Even when it names the upstream: the browser would leave the proxy for the
		// loopback origin, which a remote viewer cannot reach. sameUpstreamHost's rule
		// applies — an ambiguous spelling degrades to an honest un-rewritten redirect
		// rather than a frame bound to a server it never named.
		{name: "triple-slash naming the upstream is still not ours to claim",
			ref: "///localhost:3000/x"},
		{name: "loopback alias is not the same host", ref: "http://127.0.0.1:3000/x"},
		{name: "different port", ref: "http://localhost:3001/x"},
		{name: "scheme upgrade is not ours", ref: "https://localhost:3000/x"},
		{name: "mailto", ref: "mailto:dev@example.com"},
		{name: "empty", ref: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.target
			if raw == "" {
				raw = dflt
			}
			target, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse target %q: %v", raw, err)
			}
			got, ok := rewriteUpstreamRef(tc.ref, prefix, target)
			if tc.want == "" {
				if ok {
					t.Fatalf("rewriteUpstreamRef(%q) = %q, true; want pass-through", tc.ref, got)
				}
				return
			}
			if !ok {
				t.Fatalf("rewriteUpstreamRef(%q) = _, false; want %q", tc.ref, tc.want)
			}
			if got != tc.want {
				t.Fatalf("rewriteUpstreamRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// cookieNamed returns the named cookie from a recorded response, or nil.
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
