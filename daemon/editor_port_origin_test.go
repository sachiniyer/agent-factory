package daemon

import (
	"encoding/json"
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
	"github.com/sachiniyer/agent-factory/session"
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
	insts := map[string]*session.Instance{}
	for _, name := range []string{"editora", "editorb"} {
		insts[name] = startedLocalTabInstance(t, m, repo.ID, repoPath, name, "af_"+name+"_agent")
	}
	// seedDiskInstance overwrites the repo's rows, so the second helper call leaves
	// only one session on disk and a refresh would drop the other (and re-mint ids).
	// Persist both rows with their ids before anything refreshes.
	seeded, err := json.Marshal([]session.InstanceData{
		{ID: insts["editora"].ID, Title: "editora", Path: repoPath, Status: session.Running},
		{ID: insts["editorb"].ID, Title: "editorb", Path: repoPath, Status: session.Running},
	})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repo.ID, seeded))
	for _, name := range []string{"editora", "editorb"} {
		_, err := m.CreateTab(CreateTabRequest{Title: name, RepoID: repo.ID, Kind: "vscode"})
		require.NoError(t, err)
	}
	f.sidA, f.sidB = insts["editora"].ID, insts["editorb"].ID
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

// bootstrapRaw mints and spends a bootstrap token for sid on port, presenting any
// cookies the "browser" already holds, and returns the response.
func (f *editorPortFixture) bootstrapRaw(t *testing.T, sid string, port int, held ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := f.m.editorPorts.mintBootstrap(sid)
	require.NoError(t, err)
	return f.do(port, editorPortBootstrapPath+"?t="+url.QueryEscape(tok), held...)
}

func editorPortCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// bootstrap runs a first-visit bootstrap (a browser holding nothing) and returns the
// session's key cookie. A first visit clears origin storage and then enters the editor.
func (f *editorPortFixture) bootstrap(t *testing.T, sid string, port int) *http.Cookie {
	t.Helper()
	rec := f.bootstrapRaw(t, sid, port)
	require.Equal(t, http.StatusOK, rec.Code)
	c := editorPortCookie(rec, editorPortCookieName(port))
	require.NotNil(t, c)
	return c
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
	c := editorPortCookie(rec, editorPortCookieName(f.portA))
	require.NotNil(t, c)
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
	require.Equal(t, http.StatusOK, f.do(f.portA, path).Code)
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

// The property of the reassignment finding: after a port changes owner, no browser
// may expose the previous session's origin-scoped storage to the new session's editor.
func TestEditorPort_FirstBootstrapClearsOriginStorage(t *testing.T) {
	f := newEditorPortFixture(t)
	rec := f.bootstrapRaw(t, f.sidA, f.portA)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Clear-Site-Data"), `"storage"`)
	assert.NotContains(t, rec.Header().Get("Clear-Site-Data"), "cookies", "the key cookie set on this response must survive")
	assert.Contains(t, rec.Body.String(), "indexedDB", "the script covers origins where Clear-Site-Data is ignored (plain HTTP)")
	assert.NotNil(t, editorPortCookie(rec, editorPortEpochCookieName(f.portA)))

	// The same browser re-bootstrapping (restart, expired key) keeps its storage.
	epoch := editorPortCookie(rec, editorPortEpochCookieName(f.portA))
	again := f.bootstrapRaw(t, f.sidA, f.portA, epoch)
	assert.Equal(t, http.StatusSeeOther, again.Code)
	assert.Empty(t, again.Header().Get("Clear-Site-Data"))
}

func TestEditorPort_ReclaimedPortsFirstBootstrapClearsThePreviousOwnersStorage(t *testing.T) {
	f := newEditorPortFixture(t)
	// B's session is gone: replace it by an untracked owner of its port.
	e := f.m.editorPorts
	e.mu.Lock()
	delete(e.alloc, f.sidB)
	e.alloc["ghost"] = f.portB
	e.mu.Unlock()
	old := f.bootstrapRaw(t, "ghost", f.portB)
	oldEpoch := editorPortCookie(old, editorPortEpochCookieName(f.portB))
	require.NotNil(t, oldEpoch)

	// The pool is full (A tracked, ghost gone), so a new session takes ghost's port.
	port, err := f.m.editorPortFor(f.cs, "newcomer")
	require.NoError(t, err)
	require.Equal(t, f.portB, port)
	require.NotEqual(t, oldEpoch.Value, e.epochFor(port), "a reassigned port gets a fresh epoch")

	// A browser (maybe on another device) still holding the OLD owner's epoch is told
	// to clear on its first bootstrap for the new owner...
	first := f.bootstrapRaw(t, "newcomer", port, oldEpoch)
	require.Equal(t, http.StatusOK, first.Code)
	assert.Contains(t, first.Header().Get("Clear-Site-Data"), `"storage"`)
	// ...and not again once it carries the new epoch.
	newEpoch := editorPortCookie(first, editorPortEpochCookieName(port))
	require.NotNil(t, newEpoch)
	second := f.bootstrapRaw(t, "newcomer", port, newEpoch)
	assert.Equal(t, http.StatusSeeOther, second.Code)
	assert.Empty(t, second.Header().Get("Clear-Site-Data"))

	// The old owner's key no longer works on the reassigned port.
	oldKey := &http.Cookie{Name: editorPortCookieName(port), Value: editorPortCookie(old, editorPortCookieName(port)).Value}
	assert.Equal(t, http.StatusUnauthorized, f.do(port, "/", oldKey).Code)
}

func TestEditorPort_ReassignmentPrefersTheLongestIdlePort(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.EditorOriginPorts = "9100-9102"
	m, err := NewManager(cfg)
	require.NoError(t, err)
	e := m.editorPorts
	e.listen = func(int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for p := range e.servers {
			e.dropServerLocked(p)
		}
	})
	e.mu.Lock()
	e.loaded = true
	e.freed[9100] = 1000 // freed most recently
	e.freed[9101] = 500  // freed longest ago
	e.mu.Unlock()
	cs := &controlServer{manager: m}
	var got []int
	for _, id := range []string{"one", "two", "three"} {
		p, err := m.editorPortFor(cs, id)
		require.NoError(t, err)
		got = append(got, p)
	}
	assert.Equal(t, []int{9102, 9101, 9100}, got, "never-used first, then least recently freed")
}

func TestEditorPort_ProbePageIsServedWithoutACredentialAndCarriesNothing(t *testing.T) {
	f := newEditorPortFixture(t)
	rec := f.do(f.portA, editorPortProbePath)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	assert.Contains(t, body, editorPortProbeMessage)
	assert.NotContains(t, body, f.sidA, "the probe names no session")
	assert.Empty(t, f.served, "the probe never reaches the editor proxy")
	assert.Empty(t, rec.Result().Cookies(), "the probe sets no cookie")
}
