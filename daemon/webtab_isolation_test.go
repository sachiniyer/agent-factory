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

// Rules 2-3 for a LOCAL editor: its workbench keeps a real origin on the mirror,
// while the routes where it relays content it did not write — code-server's port
// proxy and VS Code's worktree-file route — are sandboxed.
func TestWorkspaceIsolation_LocalEditorSandboxesOnlyWorkspaceContent(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	mux := newHTTPMux(&controlServer{manager: manager})

	for _, sub := range []string{"", "static/out/vs/code/browser/workbench/workbench.html", "stable-abc/static/out/vs/workbench/contrib/webview/browser/pre/index.html"} {
		rec := vscodeProxyRecorder(t, mux, id, tabID, sub)
		assert.Contains(t, rec.Body.String(), fakeVSCodeMarker)
		assert.False(t, hasSandboxCSP(rec.Header()), "%q: the workbench needs a real origin", sub)
	}
	for _, sub := range []string{"proxy/3000/", "absproxy/5173/index.html", "vscode-remote-resource?path=/repo/evil.html", "stable-abc/vscode-remote-resource?path=/repo/x.svg"} {
		rec := vscodeProxyRecorder(t, mux, id, tabID, sub)
		assert.True(t, hasSandboxCSP(rec.Header()), "%q: content the editor relays must run sandboxed", sub)
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
	rec := httptest.NewRecorder()
	newHTTPMux(cs).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(inst.ID, "tab-editor", ""), nil))
	assert.Contains(t, rec.Body.String(), "only on an", "the mirror shows the notice: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "network.preview_listen_addr")
	assert.Contains(t, rec.Body.String(), "network.editor_origin_ports")
	assert.NotContains(t, rec.Body.String(), "runtime cannot run an editor",
		"refused before the target resolved — resolving would have tried to start the editor")

	// The same tab on an editor's own origin goes on to resolve (and here fails,
	// because the fake backend has no editor relay) — it is not the mirror's refusal.
	own := httptest.NewRecorder()
	cs.serveWebTabRoute(own, httptest.NewRequest(http.MethodGet, "/", nil), webTabRoute{
		sessionID: inst.ID, tabID: "tab-editor", previewOrigin: true,
	})
	assert.NotContains(t, own.Body.String(), "only on an")
}

func TestEditorServesWorkspaceContent(t *testing.T) {
	for _, rest := range []string{"proxy/3000/", "proxy/3000/x", "absproxy/5173/", "vscode-remote-resource", "stable-1a2b/vscode-remote-resource", "a/b/vscode-remote-resource"} {
		assert.True(t, editorServesWorkspaceContent(rest), rest)
	}
	for _, rest := range []string{"", "static/out/x.js", "stable-1a2b/static/out/vs/workbench/workbench.web.main.js", "proxyfoo/1", "my-proxy/1", "vscode-remote-resource-x"} {
		assert.False(t, editorServesWorkspaceContent(rest), rest)
	}
}

func TestWorkspaceSandboxPolicyMatrix(t *testing.T) {
	mirror := webTabRoute{rest: "x"}
	own := webTabRoute{rest: "x", previewOrigin: true}
	assert.Equal(t, mirrorSandboxCSP, workspaceSandboxPolicy(mirror, session.TabKindWeb))
	assert.Empty(t, workspaceSandboxPolicy(own, session.TabKindWeb), "a per-tab preview origin keeps its origin")
	assert.Empty(t, workspaceSandboxPolicy(mirror, session.TabKindVSCode), "the workbench keeps its origin")
	assert.Empty(t, workspaceSandboxPolicy(own, session.TabKindVSCode))
	for _, route := range []webTabRoute{{rest: "proxy/3000/"}, {rest: "proxy/3000/", previewOrigin: true}} {
		assert.Equal(t, mirrorSandboxCSP, workspaceSandboxPolicy(route, session.TabKindVSCode),
			"an editor's relayed content is sandboxed on every route: %+v", route)
	}
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
	_, statErr := os.Stat(argsFile)
	assert.True(t, os.IsNotExist(statErr), "a refused handshake must not start the editor (argv file: %v)", statErr)

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
	c, resp, err := dial("http://127.0.0.1:3000")
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
