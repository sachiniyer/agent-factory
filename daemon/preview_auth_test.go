package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The web-tab preview origin's auth (#1856 steps 2-3a). The listener opens the auth
// handshake — a PER-TAB credential (previewTabToken) plus the #2400 clean-before-render
// bootstrap — but serves no content yet. These tests pin the credential SEPARATION
// (the daemon bearer never authorizes the preview origin, and vice-versa), the PER-TAB
// scoping (tab A's token never authenticates tab B's route, the cookie is scoped to the
// tab's own path), the bootstrap's clean-before-render behavior, and the retrieval
// endpoint.

// startPreviewDaemon brings up a daemon with the preview listener enabled on an
// ephemeral port and returns the manager, the preview bound address, and the
// control bound address. Both listeners are loopback.
func startPreviewDaemon(t *testing.T) (m *Manager, previewAddr, controlAddr string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeHTTP()) })

	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewBound)
	return m, listeners.PreviewBoundAddr, listeners.TCPBoundAddr
}

// noRedirectClient inspects the bootstrap's 307 itself rather than following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// TestPreviewBootstrap_CleanBeforeRender pins the #2400 discipline on the preview
// origin: a top-level navigation carrying the preview token on the private query
// param is turned into an HttpOnly cookie and redirected to the same URL with the
// token removed, so no framed app JS can read the credential from its own
// window.location. The app's own query survives.
func TestPreviewBootstrap_CleanBeforeRender(t *testing.T) {
	m, previewAddr, _ := startPreviewDaemon(t)
	client := noRedirectClient()

	tok := previewTabToken(m.previewSecret, "s1", "t1")
	url := "http://" + previewAddr + "/v1/webtab/s1/t1/?doc=1&af_preview_token=" + tok
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode,
		"a preview navigation carrying the token must redirect to a clean URL, not render")
	require.Equal(t, "/v1/webtab/s1/t1/?doc=1", resp.Header.Get("Location"),
		"the redirect must scrub the preview token and preserve the app's own query")
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == previewTokenCookie {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "the bootstrap must set the preview-token cookie")
	require.Equal(t, tok, cookie.Value)
	require.True(t, cookie.HttpOnly, "the credential cookie must be HttpOnly so app JS cannot read it")
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Equal(t, "/v1/webtab/s1/t1/", cookie.Path,
		"the cookie is scoped to THIS tab's path, so it is never sent on another tab's route (#1856 step 3a)")
}

// TestPreviewBootstrap_ScrubsEveryAfCredential pins P2-a: the clean-before-render
// sanitizer must strip the WHOLE set of af credential query params from the URL it
// redirects to, not just the one this flow authorized on. A navigation that carries
// both the preview token (which authorizes it here) AND the daemon bearer (which
// rides along because the two origins share a query surface and cookie jar,
// RFC 6265 §8.5) must land on a URL with NEITHER — otherwise the higher-value
// daemon bearer is stranded in the document window.location the preview page renders
// under. The cookie set is still only the preview one (this flow's own credential).
func TestPreviewBootstrap_ScrubsEveryAfCredential(t *testing.T) {
	m, previewAddr, _ := startPreviewDaemon(t)
	client := noRedirectClient()

	tok := previewTabToken(m.previewSecret, "s1", "t1")
	url := "http://" + previewAddr + "/v1/webtab/s1/t1/?doc=1&af_preview_token=" + tok + "&af_webtab_token=daemon-bearer-value"
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Equal(t, "/v1/webtab/s1/t1/?doc=1", resp.Header.Get("Location"),
		"BOTH af credential params must be scrubbed; the daemon bearer must not survive in the redirect URL")

	var previewCookie, webtabCookie *http.Cookie
	for _, c := range resp.Cookies() {
		switch c.Name {
		case previewTokenCookie:
			previewCookie = c
		case webtabTokenCookie:
			webtabCookie = c
		}
	}
	require.NotNil(t, previewCookie, "the preview flow sets its own credential cookie")
	require.Equal(t, tok, previewCookie.Value)
	require.Nil(t, webtabCookie,
		"the preview flow must NOT mint the daemon-bearer cookie — it strips that param, it does not adopt it")
}

// TestPreviewGate_CookieAuthenticatesFollowUpAndRejectsOthers pins the gate on the
// preview listener: the cookie the bootstrap set authenticates a sub-resource
// request (which carries neither header nor query), a wrong credential is 401, and
// no credential is 401.
func TestPreviewGate_CookieAuthenticatesFollowUpAndRejectsOthers(t *testing.T) {
	m, previewAddr, _ := startPreviewDaemon(t)
	client := previewHTTPClient()
	assetURL := "http://" + previewAddr + "/v1/webtab/s1/t1/asset.js"
	tok := previewTabToken(m.previewSecret, "s1", "t1")

	// A sub-resource GET carrying this tab's preview cookie is authenticated — and
	// only then finds no content (404), never a 401.
	withCookie, err := http.NewRequest(http.MethodGet, assetURL, nil)
	require.NoError(t, err)
	withCookie.AddCookie(&http.Cookie{Name: previewTokenCookie, Value: tok})
	resp, err := client.Do(withCookie)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the preview cookie must authenticate a sub-resource request (no content yet ⇒ 404, not 401)")

	// A wrong cookie value is rejected.
	require.Equal(t, http.StatusUnauthorized, previewGetWithCookie(t, client, assetURL, "not-the-token"),
		"a wrong preview cookie must be rejected")

	// No credential at all is rejected — the preview origin is not loopback-exempt.
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, assetURL, ""),
		"the preview origin requires its credential from every peer, loopback included")
}

// TestPreviewGate_RejectsAnotherTabsToken is the per-tab isolation core (#1856
// step 3a): a request carrying tab A's credential to tab B's ROUTE must be refused.
// The gate derives the expected token from the sid/tid in the path, so B's route
// only ever accepts B's token — a leaked or observed token for one tab authorizes
// exactly that tab, never a sibling. Presented on the header, the query, and the
// cookie, since all three are extractor inputs.
func TestPreviewGate_RejectsAnotherTabsToken(t *testing.T) {
	m, previewAddr, _ := startPreviewDaemon(t)
	client := previewHTTPClient()

	tabAToken := previewTabToken(m.previewSecret, "sA", "tA")
	tabBToken := previewTabToken(m.previewSecret, "sB", "tB")
	require.NotEqual(t, tabAToken, tabBToken, "per-tab tokens must differ, or there is no isolation to test")

	bURL := "http://" + previewAddr + "/v1/webtab/sB/tB/asset.js"

	// Tab A's token on tab B's route: refused on every transport.
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, bURL, tabAToken),
		"tab A's token (Authorization) must NOT authenticate tab B's route")
	require.Equal(t, http.StatusUnauthorized, previewGetWithCookie(t, client, bURL, tabAToken),
		"tab A's token (cookie) must NOT authenticate tab B's route")
	aTokOnBQuery := "http://" + previewAddr + "/v1/webtab/sB/tB/asset.js?af_preview_token=" + tabAToken
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, aTokOnBQuery, ""),
		"tab A's token (query) must NOT authenticate tab B's route")

	// Sanity: tab B's OWN token authenticates tab B's route (so the 401s above are
	// isolation, not a dead route).
	require.Equal(t, http.StatusNotFound, previewGet(t, client, bURL, tabBToken),
		"tab B's own token authenticates tab B's route (no content yet ⇒ 404)")
}

// TestPreviewCredentialSeparation_BothDirections is the security core: the daemon
// bearer cannot authorize the preview origin, and the preview token cannot
// authorize the control plane. Neither credential is honored on the other surface.
//
// One daemon, made strict on the control side (require_token +
// require_loopback_token) so a wrong credential there actually fails rather than
// riding the tokenless-loopback default. The preview gate is always strict.
func TestPreviewCredentialSeparation_BothDirections(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	m, err := NewManager(cfg)
	require.NoError(t, err)
	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeHTTP()) })

	listeners := m.lifecycle.snapshot().listeners
	previewAddr := listeners.PreviewBoundAddr
	controlAddr := listeners.TCPBoundAddr
	client := previewHTTPClient()

	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	tok := previewTabToken(m.previewSecret, "s1", "t1")
	require.NotEqual(t, daemonToken, tok, "the two credentials must differ")

	// Sanity: each credential DOES authorize its own surface, so a 401 below is
	// separation, not a broken listener.
	require.Equal(t, http.StatusNotFound,
		previewGet(t, client, "http://"+previewAddr+"/v1/webtab/s1/t1/asset.js", tok),
		"the tab's preview token authenticates the preview origin (no content ⇒ 404)")
	require.Equal(t, http.StatusOK,
		previewGet(t, client, "http://"+controlAddr+"/v1/health", daemonToken),
		"the daemon token authenticates the control plane")

	// Daemon token → preview origin: rejected (it is not the preview credential).
	require.Equal(t, http.StatusUnauthorized,
		previewGet(t, client, "http://"+previewAddr+"/v1/webtab/s1/t1/asset.js", daemonToken),
		"the daemon bearer must NOT authenticate the preview origin")

	// Preview token → control plane, presented on the daemon's own webtab query
	// transport on the webtab path: the control gate wants the daemon token, so it
	// 401s.
	previewAsWebtab := "http://" + controlAddr + "/v1/webtab/s1/t1/?af_webtab_token=" + tok
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, previewAsWebtab, ""),
		"the preview token must NOT authenticate the control plane")
}

// TestPreviewAuthEndpoint_RequiresAuthThenReturnsToken pins the retrieval endpoint
// against a STRICT control listener (require_token + require_loopback_token), so
// auth is actually exercised rather than short-circuited by the tokenless-loopback
// default: no credential is 401, and the daemon token is 200 with the preview
// token. The response is no-store — it carries a raw bearer.
func TestPreviewAuthEndpoint_RequiresAuthThenReturnsToken(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	m, err := NewManager(cfg)
	require.NoError(t, err)
	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeHTTP()) })

	controlAddr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	client := previewHTTPClient()
	url := "http://" + controlAddr + "/v1/preview-auth?session=s1&tab=t1"

	// No credential → 401 (the negative test the endpoint lacked).
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, url, ""),
		"the credential-vending endpoint must require control-plane auth")

	// Daemon token → 200 with THIS tab's preview token, and no-store.
	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+daemonToken)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"),
		"a raw-bearer response must not be cacheable")

	var env struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, previewTabToken(m.previewSecret, "s1", "t1"), env.Data.Token,
		"an authenticated control-plane client must be able to fetch the requested tab's preview token")

	// A missing selector is a 400, never a token that authenticates more than one tab.
	require.Equal(t, http.StatusBadRequest,
		previewGet(t, client, "http://"+controlAddr+"/v1/preview-auth", daemonToken),
		"the endpoint must require a session/tab selector")
}

// TestPreviewAuthEndpoint_WithholdsTokenUnlessBound pins that the token is vended
// only when the preview listener actually BOUND, not merely configured — so a
// bind conflict (non-fatal by design: PreviewConfigured=true, PreviewBound=false)
// does not hand out a live-looking credential for a dead port.
func TestPreviewAuthEndpoint_WithholdsTokenUnlessBound(t *testing.T) {
	// Disabled: no preview listener at all.
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		cfg := config.DefaultConfig()
		cfg.ListenAddr = "127.0.0.1:0"
		cfg.PreviewListenAddr = ""
		m, err := NewManager(cfg)
		require.NoError(t, err)
		closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, closeHTTP()) })
		require.Empty(t, fetchPreviewAuthToken(t, m), "a disabled preview listener must vend no token")
	})

	// Configured but bind FAILED: the port is already taken.
	t.Run("configured but bind failed", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = blocker.Close() }()

		cfg := config.DefaultConfig()
		cfg.ListenAddr = "127.0.0.1:0"
		cfg.PreviewListenAddr = blocker.Addr().String() // guaranteed EADDRINUSE
		m, err := NewManager(cfg)
		require.NoError(t, err)
		closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, closeHTTP()) })

		listeners := m.lifecycle.snapshot().listeners
		require.True(t, listeners.PreviewConfigured, "the address was configured")
		require.False(t, listeners.PreviewBound, "the bind failed")
		require.Empty(t, fetchPreviewAuthToken(t, m),
			"a configured-but-unbound preview listener must not vend a token for a dead port")
	})
}

// fetchPreviewAuthToken calls GET /v1/preview-auth?session=&tab= on m's control
// listener (over its own unix-socket-trust loopback default) and returns the vended
// per-tab token (empty when the preview listener is not bound).
func fetchPreviewAuthToken(t *testing.T, m *Manager) string {
	t.Helper()
	controlAddr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	resp, err := previewHTTPClient().Get("http://" + controlAddr + "/v1/preview-auth?session=s1&tab=t1")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var env struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	return env.Data.Token
}

// TestPreviewSecret_EphemeralAndNeverOnDisk pins the two properties of the per-tab
// HMAC secret: it differs between daemon lifetimes (rotates on restart) and it is
// never written to the AF home. The secret is the root from which every tab token
// derives, so it matters more than any single token that it never lands on disk.
func TestPreviewSecret_EphemeralAndNeverOnDisk(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()

	m1, err := NewManager(cfg)
	require.NoError(t, err)
	m2, err := NewManager(cfg)
	require.NoError(t, err)

	require.NotEmpty(t, m1.previewSecret)
	require.NotEqual(t, m1.previewSecret, m2.previewSecret,
		"the preview secret must be freshly minted per daemon, not persisted and reused")

	// The secret value must appear in no file under the AF home.
	require.NoError(t, filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // unreadable/special file — not where a secret would be
		}
		require.NotContains(t, string(data), m1.previewSecret,
			"the preview secret must never be written to disk (found in %s)", path)
		return nil
	}))
}

// TestPreviewTabToken_Derivation pins the per-tab derivation: deterministic, distinct
// per tab and per secret, and — critically — the NUL separator makes the (sid, tid)
// pair unambiguous so no two different pairs that concatenate to the same string can
// share a token. Without the separator, ("ab","c") and ("a","bc") would collide and
// one tab's token would authenticate another.
func TestPreviewTabToken_Derivation(t *testing.T) {
	const secret = "secret-key-material"

	require.Equal(t, previewTabToken(secret, "s1", "t1"), previewTabToken(secret, "s1", "t1"),
		"derivation must be deterministic")
	require.NotEqual(t, previewTabToken(secret, "s1", "t1"), previewTabToken(secret, "s1", "t2"),
		"different tabs in the same session must derive different tokens")
	require.NotEqual(t, previewTabToken(secret, "s1", "t1"), previewTabToken(secret, "s2", "t1"),
		"the same tab id in different sessions must derive different tokens")
	require.NotEqual(t, previewTabToken(secret, "ab", "c"), previewTabToken(secret, "a", "bc"),
		"the separator must prevent a concatenation collision between distinct (sid, tid) pairs")
	require.NotEqual(t, previewTabToken(secret, "s1", "t1"), previewTabToken("other-secret", "s1", "t1"),
		"a different secret must derive a different token (rotation on restart)")
	require.NotEmpty(t, previewTabToken(secret, "s1", "t1"))
}

// TestParsePreviewTabPath pins the gate's path parse: a two-segment tab route yields
// its ids, a percent-encoded segment is unescaped exactly once (matching PathValue),
// and anything that is not a tab route is a fail-closed miss.
func TestParsePreviewTabPath(t *testing.T) {
	sid, tid, ok := parsePreviewTabPath("/v1/webtab/s1/t1/asset.js")
	require.True(t, ok)
	require.Equal(t, "s1", sid)
	require.Equal(t, "t1", tid)

	// A percent-encoded segment decodes to ONE id, not two segments.
	sid, tid, ok = parsePreviewTabPath("/v1/webtab/a%2Fb/t1/")
	require.True(t, ok)
	require.Equal(t, "a/b", sid, "an encoded slash is part of the id, not a segment boundary")
	require.Equal(t, "t1", tid)

	for _, miss := range []string{
		"/v1/webtab/",     // bare prefix, no tab
		"/v1/webtab/s1/",  // session only, no tab
		"/v1/webtab/s1",   // no trailing structure
		"/v1/Snapshot",    // not a webtab path at all
		"/",               // root
		"/v1/webtab//t1/", // empty session segment
		"/v1/webtab/s1//", // empty tab segment
	} {
		_, _, ok := parsePreviewTabPath(miss)
		require.False(t, ok, "%q is not a tab route and must be a fail-closed miss", miss)
	}
}

// previewGetWithCookie issues a GET carrying the preview cookie and returns the
// status code.
func previewGetWithCookie(t *testing.T, client *http.Client, url, cookieValue string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: previewTokenCookie, Value: cookieValue})
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestPreviewPresentedToken_IsolatedFromDaemonTransports pins the extractor
// separation at the unit level: previewPresentedToken reads ONLY the preview
// transports (Authorization, af_preview_token query/cookie), never the daemon's
// af_webtab_token or ?access_token=; and webTabAwareToken never reads the preview
// param. So the two credentials cannot be read for one another even if a request
// carried both.
func TestPreviewPresentedToken_IsolatedFromDaemonTransports(t *testing.T) {
	// A request carrying the daemon's transports but NOT the preview one: the
	// preview extractor sees nothing.
	r := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/?af_webtab_token=DAEMON&access_token=DAEMON2", nil)
	require.Empty(t, previewPresentedToken(r),
		"previewPresentedToken must ignore the daemon's af_webtab_token / access_token transports")

	// The preview param is read.
	r2 := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/?af_preview_token=PREVIEW", nil)
	require.Equal(t, "PREVIEW", previewPresentedToken(r2))

	// And the daemon's webtab extractor never reads the preview param.
	r3 := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/?af_preview_token=PREVIEW", nil)
	require.Empty(t, webTabAwareToken(r3),
		"webTabAwareToken must ignore the preview transport, so the preview credential can't authorize the daemon surface")

	// The Authorization header is honored by both (a direct client), but that is the
	// credential the caller chose to send, not a cross-transport leak.
	r4 := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/", nil)
	r4.Header.Set("Authorization", "Bearer HDR")
	require.Equal(t, "HDR", previewPresentedToken(r4))

	// Sanity: an unrelated cookie is ignored; only af_preview_token is read.
	r5 := httptest.NewRequest(http.MethodGet, "/v1/webtab/s/t/", nil)
	r5.AddCookie(&http.Cookie{Name: webtabTokenCookie, Value: "DAEMONCOOKIE"})
	require.Empty(t, previewPresentedToken(r5), "the preview extractor must not read the daemon's webtab cookie")
	require.False(t, strings.Contains(previewPresentedToken(r5), "DAEMONCOOKIE"))
}
