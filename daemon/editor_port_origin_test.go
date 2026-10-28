package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

type editorPortFixture struct {
	cs         *controlServer
	m          *Manager
	sidA, sidB string
	portA      int
	portB      int
	served     []string // session ids the gate admitted
}

func newEditorPortFixture(t *testing.T) *editorPortFixture {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	cfg.EditorOriginPorts = "9100-9101"
	m, err := NewManager(cfg)
	require.NoError(t, err)
	m.readyOnce.Do(func() { close(m.ready) })

	f := &editorPortFixture{cs: &controlServer{manager: m}, m: m}
	for _, name := range []string{"editora", "editorb"} {
		inst := startedLocalTabInstance(t, m, repo.ID, repoPath, name, "af_"+name+"_agent")
		_, err := m.CreateTab(CreateTabRequest{Title: name, RepoID: repo.ID, Kind: "vscode"})
		require.NoError(t, err)
		if name == "editora" {
			f.sidA = inst.ID
		} else {
			f.sidB = inst.ID
		}
	}
	// The pool ports are not bound for real: listeners land on free ports and the
	// handler is driven directly.
	m.editorPorts.listen = func(int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	t.Cleanup(func() {
		m.editorPorts.mu.Lock()
		defer m.editorPorts.mu.Unlock()
		for p := range m.editorPorts.servers {
			m.editorPorts.dropServerLocked(p)
		}
	})
	f.portA, err = m.editorPortFor(f.cs, f.sidA)
	require.NoError(t, err)
	f.portB, err = m.editorPortFor(f.cs, f.sidB)
	require.NoError(t, err)
	require.NotEqual(t, f.portA, f.portB)

	prev := editorPortServe
	editorPortServe = func(_ *controlServer, w http.ResponseWriter, _ *http.Request, route webTabRoute) {
		f.served = append(f.served, route.sessionID)
		w.WriteHeader(http.StatusOK)
	}
	t.Cleanup(func() { editorPortServe = prev })
	return f
}

func (f *editorPortFixture) do(port int, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.cs.editorPortHandler(port, rec, req)
	return rec
}

// bootstrap mints and spends a bootstrap token for sid on its port, returning the cookie.
func (f *editorPortFixture) bootstrap(t *testing.T, sid string, port int) *http.Cookie {
	t.Helper()
	tok, err := f.m.editorPorts.mintBootstrap(sid)
	require.NoError(t, err)
	rec := f.do(port, editorPortBootstrapPath+"?t="+url.QueryEscape(tok))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/", rec.Header().Get("Location"), "the key must not live on in a URL")
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	return cookies[0]
}

func TestEditorPort_NoCookieIsRefused(t *testing.T) {
	f := newEditorPortFixture(t)
	assert.Equal(t, http.StatusUnauthorized, f.do(f.portA, "/").Code)
	assert.Equal(t, http.StatusUnauthorized, f.do(f.portA, "/stable-abc/static/x.js").Code)
	assert.Empty(t, f.served)
}

func TestEditorPort_BootstrapSetsAHardenedCookieThenTheSessionIsServed(t *testing.T) {
	f := newEditorPortFixture(t)
	tok, err := f.m.editorPorts.mintBootstrap(f.sidA)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, editorPortBootstrapPath+"?t="+tok, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.cs.editorPortHandler(f.portA, rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	c := rec.Result().Cookies()[0]
	assert.Equal(t, editorPortCookieName(f.portA), c.Name)
	assert.True(t, c.HttpOnly)
	assert.True(t, c.Secure, "HTTPS front door => Secure")
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite)

	require.Equal(t, http.StatusOK, f.do(f.portA, "/", c).Code)
	assert.Equal(t, []string{f.sidA}, f.served)
}

func TestEditorPort_SessionAKeyIsRefusedOnSessionBsOrigin(t *testing.T) {
	f := newEditorPortFixture(t)
	a := f.bootstrap(t, f.sidA, f.portA)
	_ = f.bootstrap(t, f.sidB, f.portB)

	// A's cookie as-is (named for A's port) carries nothing B's port looks for.
	assert.Equal(t, http.StatusUnauthorized, f.do(f.portB, "/", a).Code)
	// And A's key value under B's cookie name is no better.
	forged := &http.Cookie{Name: editorPortCookieName(f.portB), Value: a.Value}
	assert.Equal(t, http.StatusUnauthorized, f.do(f.portB, "/", forged).Code)
	assert.Empty(t, f.served)
}

func TestEditorPort_ReplayedBootstrapIsRefused(t *testing.T) {
	f := newEditorPortFixture(t)
	tok, err := f.m.editorPorts.mintBootstrap(f.sidA)
	require.NoError(t, err)
	path := editorPortBootstrapPath + "?t=" + tok
	require.Equal(t, http.StatusSeeOther, f.do(f.portA, path).Code)
	replay := f.do(f.portA, path)
	assert.Equal(t, http.StatusForbidden, replay.Code)
	assert.Empty(t, replay.Result().Cookies())

	// A token minted for A is also spent, not honoured, when presented on B's port.
	tokA, err := f.m.editorPorts.mintBootstrap(f.sidA)
	require.NoError(t, err)
	wrong := f.do(f.portB, editorPortBootstrapPath+"?t="+tokA)
	assert.Equal(t, http.StatusForbidden, wrong.Code)
	assert.Equal(t, http.StatusForbidden, f.do(f.portA, editorPortBootstrapPath+"?t="+tokA).Code)
}

func TestEditorPort_ExpiredBootstrapIsRefused(t *testing.T) {
	f := newEditorPortFixture(t)
	tok, err := f.m.editorPorts.mintBootstrap(f.sidA)
	require.NoError(t, err)
	base := f.m.editorPorts.now()
	f.m.editorPorts.now = func() time.Time { return base.Add(editorPortBootstrapTTL + 1) }
	assert.Equal(t, http.StatusForbidden, f.do(f.portA, editorPortBootstrapPath+"?t="+tok).Code)
}

func TestEditorPort_AnArchivedOrKilledSessionsKeyIsRefused(t *testing.T) {
	f := newEditorPortFixture(t)
	a := f.bootstrap(t, f.sidA, f.portA)
	require.Equal(t, http.StatusOK, f.do(f.portA, "/", a).Code)

	// stopVSCodeForInstance — what archive and kill both pass through — withdraws it.
	f.m.editorPorts.invalidate(f.sidA)
	assert.Equal(t, http.StatusUnauthorized, f.do(f.portA, "/", a).Code)

	// A bootstrap minted before the withdrawal does not survive it either.
	tok, err := f.m.editorPorts.mintBootstrap(f.sidB)
	require.NoError(t, err)
	f.m.editorPorts.invalidate(f.sidB)
	assert.Equal(t, http.StatusForbidden, f.do(f.portB, editorPortBootstrapPath+"?t="+tok).Code)
}

func TestEditorPort_AllocationIsPersistedAndBounded(t *testing.T) {
	f := newEditorPortFixture(t)
	// A fresh in-memory index over the same AF home reads back the same ports.
	again := newEditorPortOrigins()
	again.listen = f.m.editorPorts.listen
	f.m.editorPorts = again
	pa, err := f.m.editorPortFor(f.cs, f.sidA)
	require.NoError(t, err)
	assert.Equal(t, f.portA, pa, "a session keeps its port across a restart")
	t.Cleanup(func() {
		again.mu.Lock()
		defer again.mu.Unlock()
		for p := range again.servers {
			again.dropServerLocked(p)
		}
	})

	// A third live session has no port left, and the error names the key.
	_, err = f.m.editorPortFor(f.cs, "some-other-live-session")
	// "some-other-live-session" is untracked, so its own allocation is not blocked by
	// reclaim; the pool of two is full of tracked sessions.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network.editor_origin_ports")
}

func TestEditorPort_OffByDefaultNamesTheKey(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	_, err = m.editorPortFor(&controlServer{manager: m}, "any")
	require.ErrorIs(t, err, errEditorPortsOff)
	assert.Contains(t, err.Error(), "network.editor_origin_ports")
}

func TestEditorPort_MintEndpointReturnsABootstrapOnTheFrontDoorHost(t *testing.T) {
	f := newEditorPortFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/editor-origin?session="+url.QueryEscape(f.sidA), nil)
	req.Host = "box.tailnet.ts.net"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.cs.editorOriginHandler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "https://box.tailnet.ts.net:"+strconv.Itoa(f.portA)+editorPortBootstrapPath+"?t=")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	// An unknown session gets no origin.
	bad := httptest.NewRecorder()
	f.cs.editorOriginHandler(bad, httptest.NewRequest(http.MethodPost, "/v1/editor-origin?session=nope", nil))
	assert.Equal(t, http.StatusNotFound, bad.Code)
}

func TestEditorPort_CookieNeverReachesTheEditor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: editorPortCookieName(9100), Value: "secret"})
	req.AddCookie(&http.Cookie{Name: "keep", Value: "1"})
	forwardAppCookies(req)
	assert.Equal(t, "keep=1", req.Header.Get("Cookie"))
}
