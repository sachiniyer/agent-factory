package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// Workspace content never runs with the control origin's authority
// (webtab_isolation.go).

func hasSandboxCSP(h http.Header) bool {
	for _, v := range h.Values("Content-Security-Policy") {
		if strings.TrimSpace(v) == mirrorSandboxCSP {
			return true
		}
	}
	return false
}

// Rule 1: every mirrored web-tab response carries the opaque-origin sandbox, so a
// page opened top-level from the mirror (the pane's "Open") runs exactly as it does
// in the frame. The app's own policy survives beside it.
func TestWorkspaceIsolation_MirroredWebTabIsSandboxed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html>app %s</html>", r.URL.Path)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	for _, sub := range []string{"", "app/viewer.html", "assets/app.js"} {
		rec := proxyGet(t, mux, id, tabID, sub)
		if rec.Code == http.StatusFound || rec.Code == http.StatusTemporaryRedirect {
			continue // the mirror-root redirect carries no document
		}
		require.Equal(t, http.StatusOK, rec.Code, "%q: %s", sub, rec.Body.String())
		assert.True(t, hasSandboxCSP(rec.Header()), "%q: a mirrored document must run sandboxed: %v", sub, rec.Header().Values("Content-Security-Policy"))
		assert.Contains(t, rec.Header().Values("Content-Security-Policy"), "default-src 'self'", "%q: the app's own policy is kept", sub)
	}
}

// A web tab's own per-tab preview origin is separate from everything by design and
// keeps the real origin its app needs.
func TestWorkspaceIsolation_PreviewOriginWebTabKeepsItsOrigin(t *testing.T) {
	m, previewAddr, _, sid, tid := startPreviewDaemon(t)
	resp := previewHostGet(t, previewHTTPClient(), previewAddr, previewHostOf(t, m, previewAddr, sid, tid), "/")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, hasSandboxCSP(resp.Header), "%v", resp.Header.Values("Content-Security-Policy"))
}

// vscodeProxyRecorder is getVSCodeProxy returning the whole response.
func vscodeProxyRecorder(t *testing.T, mux *http.ServeMux, id, tabID, sub string) *httptest.ResponseRecorder {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, sub), nil))
		if rec.Code == http.StatusOK {
			return rec
		}
		if !strings.Contains(rec.Body.String(), "still starting") || time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d, body %s", vscodeProxyPath(id, tabID, sub), rec.Code, rec.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// testCommit is a realistic VS Code product segment: <quality>-<40 hex commit>.
const testCommit = "stable-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// Rule 3 for a LOCAL editor, through the real proxy: only the workbench keeps a real
// origin on the mirror; every other path the editor serves is sandboxed, in any
// spelling a case-insensitive or decoding router would still route.
func TestWorkspaceIsolation_LocalEditorSandboxesAllButTheWorkbench(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	mux := newHTTPMux(&controlServer{manager: manager})

	for _, sub := range []string{"", "static/out/vs/code/browser/workbench/workbench.html",
		testCommit + "/static/out/vs/workbench/contrib/webview/browser/pre/index.html", "_static/out/browser/serviceWorker.js"} {
		rec := vscodeProxyRecorder(t, mux, id, tabID, sub)
		assert.Contains(t, rec.Body.String(), fakeVSCodeMarker)
		assert.False(t, hasSandboxCSP(rec.Header()), "%q: the workbench needs a real origin", sub)
	}
	for _, sub := range []string{"proxy/3000/", "PROXY/3000/", "Proxy/3000/x", "%70roxy/3000/", "absproxy/5173/index.html", "AbsProxy/5173/",
		"vscode-remote-resource?path=/repo/evil.html", testCommit + "/vscode-remote-resource?path=/repo/x.svg",
		"STATIC/out/x.html", "%73tatic/out/x.html", "web-extension-resource/x/y.html", "login", "unknown/route.html"} {
		rec := vscodeProxyRecorder(t, mux, id, tabID, sub)
		assert.True(t, hasSandboxCSP(rec.Header()), "%q: anything but the workbench must run sandboxed", sub)
	}
}

// Rule 2: the mirror never serves a RELAYED editor (the VS Code tab of an off-box
// workspace), and refuses before resolving the target, so the editor is not
// started on the workspace's host for nothing. Its own origin is not refused.
func TestWorkspaceIsolation_MirrorRefusesARelayedEditor(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	repoPath := setupControlRepo(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "offbox", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.AddTabWithIDForTest("tab-editor", "VS Code", session.TabKindVSCode)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey("repo-offbox", "offbox")] = inst
	manager.mu.Unlock()
	editor, relayed := manager.trackedEditorTab(inst.ID, "tab-editor")
	require.True(t, editor && relayed)

	cs := &controlServer{manager: manager}
	// By stable id and by title alike: WebTabTarget resolves both, so the
	// refuse-before-start check must too.
	for _, addr := range []string{inst.ID, "offbox"} {
		rec := httptest.NewRecorder()
		newHTTPMux(cs).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(addr, "tab-editor", ""), nil))
		assert.Contains(t, rec.Body.String(), "only on an", "%s: the mirror shows the notice: %s", addr, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "network.preview_listen_addr")
		assert.Contains(t, rec.Body.String(), "network.editor_origin_ports")
		assert.NotContains(t, rec.Body.String(), "runtime cannot run an editor",
			"%s: refused before the target resolved — resolving would have tried to start the editor", addr)
	}

	// The same tab on an editor's own origin goes on to resolve (and here fails,
	// because the fake backend has no editor relay) — it is not the mirror's refusal.
	own := httptest.NewRecorder()
	cs.serveWebTabRoute(own, httptest.NewRequest(http.MethodGet, "/", nil), webTabRoute{
		sessionID: inst.ID, tabID: "tab-editor", previewOrigin: true,
	})
	assert.NotContains(t, own.Body.String(), "only on an")
}

func TestEditorPathKeepsItsOrigin(t *testing.T) {
	for _, rest := range []string{"", "/", "vscode", "vscode/", "static/out/x.js", "static/out/vs/workbench/contrib/webview/browser/pre/index.html",
		testCommit + "/", testCommit + "/static/out/vs/workbench/workbench.web.main.js", "vscode/" + testCommit + "/static/x.css",
		"callback", testCommit + "/callback", "oss-dev/static/x.js", "_static/out/browser/serviceWorker.js"} {
		assert.True(t, editorPathKeepsItsOrigin(rest), "the workbench: %q", rest)
	}
	for _, rest := range []string{
		// code-server's port proxy, in the spellings its case-insensitive router accepts
		"proxy/3000/", "PROXY/3000/", "Proxy/3000", "absproxy/5173/", "ABSPROXY/5173/", "%70roxy/3000/", "proxy%2F3000",
		// worktree files, at the root and under the product segment
		"vscode-remote-resource", testCommit + "/vscode-remote-resource", "a/b/vscode-remote-resource",
		// the allow-listed names, in any other spelling, place or shape
		"STATIC/x.js", "Static/x.js", "%73tatic/x.js", "static", "static%2Fx.js", "_STATIC/x", "_static", "vscode/_static/x",
		"CALLBACK", "callback/x", "x/static/y.js", testCommit, strings.ToUpper(testCommit) + "/static/x.js", "stable-xyz/static/x.js",
		// everything else the editors serve, and anything unknown
		"web-extension-resource/x", "login", "healthz", "update/check", "mint-key", "manifest.json", "version", "anything/else.html"} {
		assert.False(t, editorPathKeepsItsOrigin(rest), "not the workbench: %q", rest)
	}
}

func TestWorkspaceSandboxPolicyMatrix(t *testing.T) {
	mirror := webTabRoute{rest: "x", escapedRest: "x"}
	own := webTabRoute{rest: "x", escapedRest: "x", previewOrigin: true}
	assert.Equal(t, mirrorSandboxCSP, workspaceSandboxPolicy(mirror, session.TabKindWeb))
	assert.Empty(t, workspaceSandboxPolicy(own, session.TabKindWeb), "a per-tab preview origin keeps its origin")
	for _, route := range []webTabRoute{{}, {previewOrigin: true}, {rest: "static/x.js", escapedRest: "static/x.js"}} {
		assert.Empty(t, workspaceSandboxPolicy(route, session.TabKindVSCode), "the workbench keeps its origin: %+v", route)
	}
	for _, route := range []webTabRoute{mirror, own, {rest: "proxy/3000/", escapedRest: "PROXY/3000/", previewOrigin: true}} {
		assert.Equal(t, mirrorSandboxCSP, workspaceSandboxPolicy(route, session.TabKindVSCode),
			"anything but the workbench is sandboxed on every route: %+v", route)
	}
}

// Rule 5: a workspace response never acts on a whole origin it shares.
func TestConfineOriginHeaders(t *testing.T) {
	const prefix = "/v1/webtab/s1/t1"
	mirror := webTabRoute{prefix: prefix}
	own := webTabRoute{previewOrigin: true}
	run := func(route webTabRoute, kind session.TabKind, set map[string]string) http.Header {
		h := http.Header{}
		for k, v := range set {
			h.Set(k, v)
		}
		confineOriginHeaders(h, route, kind, prefix+"/assets/sw.js")
		return h
	}
	wipe := map[string]string{"Clear-Site-Data": `"storage", "cookies"`}
	assert.Empty(t, run(mirror, session.TabKindWeb, wipe).Get("Clear-Site-Data"), "the web UI's origin")
	assert.Empty(t, run(mirror, session.TabKindVSCode, wipe).Get("Clear-Site-Data"), "the web UI's origin")
	assert.Empty(t, run(own, session.TabKindVSCode, wipe).Get("Clear-Site-Data"), "an editor's origin belongs to the editor")
	assert.NotEmpty(t, run(own, session.TabKindWeb, wipe).Get("Clear-Site-Data"), "a web tab's own preview origin is the app's")

	for value, want := range map[string]string{
		"/":                         prefix + "/",
		"/app/":                     prefix + "/app/",
		"/app":                      prefix + "/app",
		"./":                        prefix + "/assets/",
		"../":                       prefix + "/",
		"../../../../":              prefix + "/",
		"%2e%2e/%2e%2e/%2e%2e/":     prefix + "/",
		"http://127.0.0.1:8443/":    prefix + "/",
		"//127.0.0.1:8443/":         prefix + "/",
		"/../../":                   prefix + "/",
		"/v1/webtab/s1/t1-sibling/": prefix + "/v1/webtab/s1/t1-sibling/",
	} {
		got := run(mirror, session.TabKindWeb, map[string]string{"Service-Worker-Allowed": value}).Get("Service-Worker-Allowed")
		assert.Equal(t, want, got, "Service-Worker-Allowed %q", value)
		assert.True(t, strings.HasPrefix(got, prefix+"/"), "%q stays inside the tab", value)
	}
	assert.Empty(t, run(mirror, session.TabKindWeb, nil).Values("Service-Worker-Allowed"), "nothing is added")
	assert.Equal(t, "/", run(own, session.TabKindWeb, map[string]string{"Service-Worker-Allowed": "/"}).Get("Service-Worker-Allowed"),
		"a tab's own origin is the tab's to scope")
}

// Rule 5 through the real proxy, on the mirror.
func TestWorkspaceIsolation_MirrorConfinesOriginWideHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Clear-Site-Data", `"*"`)
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, "self.onfetch = () => {}")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)
	rec := proxyGet(t, mux, id, tabID, "sw.js")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Values("Clear-Site-Data"))
	assert.Equal(t, webtabPathPrefix+id+"/"+tabID+"/", rec.Header().Get("Service-Worker-Allowed"))
}

// The header's flags are the web client's mirror-frame sandbox; web/src/tabaddr.test.ts
// pins webSandbox(false, false) to the same literal.
func TestMirrorSandboxMatchesTheClientsFrameSandbox(t *testing.T) {
	assert.Equal(t, "sandbox allow-scripts allow-forms allow-popups allow-modals", mirrorSandboxCSP)
}

// Rule 4: an editor's WebSocket is accepted only from the editor's own origin. The
// fake stands in for openvscode-server, which checks no Origin itself, so whatever
// is refused here af refused. A refused handshake is answered before the target
// resolves, so it never starts the editor either.
func TestWorkspaceIsolation_EditorWebSocketOnlyFromItsOwnOrigin(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "argv")
	binary := writeFakeVSCodeBinary(t, "openvscode-server", map[string]string{fakeVSCodeArgsEnv: argsFile})
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	srv := httptest.NewServer(newHTTPMux(&controlServer{manager: manager}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + vscodeProxyPath(id, tabID, "ws")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func(origin string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		return websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
	}
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	foreign := []string{
		"http://127.0.0.1:3000", // a dev server on another port of this host
		"null",                  // a sandboxed page: rule 3's, or a mirrored web tab's
		"http://localhost:" + port,
		"http://attacker.example",
		"http://127.0.0.1:" + port + ".attacker.example",
	}
	for _, origin := range foreign {
		c, resp, err := dial(origin)
		if c != nil {
			_ = c.CloseNow()
		}
		require.Error(t, err, "Origin %q must not reach the editor's WebSocket", origin)
		require.NotNil(t, resp, "Origin %q: %v", origin, err)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "Origin %q", origin)
	}
	// The same tab addressed by its session's TITLE is refused before it starts too.
	byTitle := "ws" + strings.TrimPrefix(srv.URL, "http") + vscodeProxyPath("vscodeproxy", tabID, "ws")
	c, resp, err := websocket.Dial(ctx, byTitle, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"null"}}})
	if c != nil {
		_ = c.CloseNow()
	}
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "by title")
	_, statErr := os.Stat(argsFile)
	assert.True(t, os.IsNotExist(statErr), "a refused handshake, by id or title, must not start the editor (argv file: %v)", statErr)

	// The editor's own page (and a non-browser client, which sends no Origin) still
	// gets a real, relaying connection.
	waitForVSCodeReady(t, srv.Client(), srv.URL+vscodeProxyPath(id, tabID, ""))
	for _, origin := range []string{srv.URL, ""} {
		c, _, err := dial(origin)
		require.NoError(t, err, "Origin %q is the editor's own", origin)
		require.NoError(t, c.Write(ctx, websocket.MessageText, []byte("af-own-origin")))
		_, got, err := c.Read(ctx)
		require.NoError(t, err)
		assert.Equal(t, "af-own-origin", string(got))
		_ = c.CloseNow()
	}
	// A running editor is refused the same way.
	c, resp, err = dial("http://127.0.0.1:3000")
	if c != nil {
		_ = c.CloseNow()
	}
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// Rule 4 holds on an editor's OWN origin too (the preview listener's editor origin
// and the editor-origin ports reach the same core with previewOrigin set): there it
// protects the editor from a page on any other origin, the browser's other tabs
// included.
func TestWorkspaceIsolation_EditorWebSocketRuleOnItsOwnOrigin(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "openvscode-server", nil)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	cs := &controlServer{manager: manager}
	const host = "afabc123.localhost:8444"
	serve := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.Host = host
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		cs.serveWebTabRoute(rec, req, webTabRoute{sessionID: id, tabID: tabID, escapedRest: "ws", rest: "ws", previewOrigin: true})
		return rec
	}
	const refusal = "editor accepts connections only from its own page"
	for _, origin := range []string{
		"http://127.0.0.1:8443",         // the web UI's origin is another origin here
		"http://afother.localhost:8444", // another session's editor
		"null",
	} {
		rec := serve(origin)
		assert.Equal(t, http.StatusForbidden, rec.Code, "Origin %q on %s: %s", origin, host, rec.Body.String())
		assert.Contains(t, rec.Body.String(), refusal)
	}
	// Its own origin passes the rule (and goes on to the editor, which a recorder
	// cannot hand a connection to — what matters here is that af did not refuse it).
	assert.NotContains(t, serve("http://"+host).Body.String(), refusal)
}

func TestEditorUpgradeRefusal(t *testing.T) {
	upgrade := func(host, origin string, mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/ws", nil)
		r.Host = host
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if mutate != nil {
			mutate(r)
		}
		return r
	}
	refused := func(r *http.Request, allowed ...string) bool { return editorUpgradeRefusal(r, allowed) != nil }

	// Its own origin, and no Origin at all (not a browser), are admitted.
	assert.False(t, refused(upgrade("127.0.0.1:8443", "http://127.0.0.1:8443", nil)))
	assert.False(t, refused(upgrade("127.0.0.1:8443", "", nil)))
	assert.False(t, refused(upgrade("box.ts.net", "https://box.ts.net", func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https") // tailscale serve keeps Host
	})))
	// Behind a same-host proxy that rewrote Host: the browser's name, admitted by
	// network.allowed_hosts, from a loopback peer — exactly as the web UI's streams.
	behindProxy := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-Host", "box.example.com")
	}
	assert.False(t, refused(upgrade("127.0.0.1:8443", "http://box.example.com", behindProxy), "box.example.com"))
	assert.True(t, refused(upgrade("127.0.0.1:8443", "http://box.example.com", behindProxy)), "a forwarded name the daemon does not answer to vouches for nothing")

	// Any other origin is refused, whatever the upgrade's spelling.
	for _, origin := range []string{"null", "http://127.0.0.1:3000", "http://localhost:8443", "https://127.0.0.1:8443/x", "http://attacker.example"} {
		assert.True(t, refused(upgrade("127.0.0.1:8443", origin, nil)), origin)
	}
	assert.True(t, refused(upgrade("127.0.0.1:8443", "null", func(r *http.Request) { r.Header.Set("Connection", "upgrade") })))
	assert.True(t, refused(upgrade("127.0.0.1:8443", "null", func(r *http.Request) { r.Header.Del("Upgrade") })), "a Connection: upgrade alone counts")
	assert.True(t, refused(upgrade("127.0.0.1:8443", "null", func(r *http.Request) { r.Header.Set("Upgrade", "h2c") })), "every upgrade, not only websocket")

	// A request that asks for no protocol switch is not this rule's.
	plain := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/", nil)
	plain.Host = "127.0.0.1:8443"
	plain.Header.Set("Origin", "null")
	assert.False(t, refused(plain))
}
