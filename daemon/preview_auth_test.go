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

// The web-tab preview origin's auth (#1856 step 2). The listener opens the auth
// handshake — its own ephemeral credential plus the #2400 clean-before-render
// bootstrap — but serves no content yet. These tests pin the credential SEPARATION
// (the daemon bearer never authorizes the preview origin, and vice-versa), the
// bootstrap's clean-before-render behavior, and the retrieval endpoint.

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

	url := "http://" + previewAddr + "/v1/webtab/s1/t1/?doc=1&af_preview_token=" + m.previewToken
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
	require.Equal(t, m.previewToken, cookie.Value)
	require.True(t, cookie.HttpOnly, "the credential cookie must be HttpOnly so app JS cannot read it")
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Equal(t, webtabPathPrefix, cookie.Path,
		"the cookie is scoped to the preview content path, not the whole origin's root")
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

	url := "http://" + previewAddr + "/v1/webtab/s1/t1/?doc=1&af_preview_token=" + m.previewToken + "&af_webtab_token=daemon-bearer-value"
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
	require.Equal(t, m.previewToken, previewCookie.Value)
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

	// A sub-resource GET carrying the preview cookie is authenticated — and only
	// then finds no content (404), never a 401.
	withCookie, err := http.NewRequest(http.MethodGet, assetURL, nil)
	require.NoError(t, err)
	withCookie.AddCookie(&http.Cookie{Name: previewTokenCookie, Value: m.previewToken})
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
	require.NotEqual(t, daemonToken, m.previewToken, "the two credentials must differ")

	// Sanity: each credential DOES authorize its own surface, so a 401 below is
	// separation, not a broken listener.
	require.Equal(t, http.StatusNotFound,
		previewGet(t, client, "http://"+previewAddr+"/v1/webtab/s1/t1/asset.js", m.previewToken),
		"the preview token authenticates the preview origin (no content ⇒ 404)")
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
	previewAsWebtab := "http://" + controlAddr + "/v1/webtab/s1/t1/?af_webtab_token=" + m.previewToken
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
	url := "http://" + controlAddr + "/v1/preview-auth"

	// No credential → 401 (the negative test the endpoint lacked).
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, url, ""),
		"the credential-vending endpoint must require control-plane auth")

	// Daemon token → 200 with the preview token, and no-store.
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
	require.Equal(t, m.previewToken, env.Data.Token,
		"an authenticated control-plane client must be able to fetch the preview token")
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

// fetchPreviewAuthToken calls GET /v1/preview-auth on m's control listener (over
// its own unix-socket-trust loopback default) and returns the vended token.
func fetchPreviewAuthToken(t *testing.T, m *Manager) string {
	t.Helper()
	controlAddr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	resp, err := previewHTTPClient().Get("http://" + controlAddr + "/v1/preview-auth")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var env struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	return env.Data.Token
}

// TestPreviewToken_EphemeralAndNeverOnDisk pins the two properties of the
// credential: it differs between daemon lifetimes (rotates on restart) and it is
// never written to the AF home.
func TestPreviewToken_EphemeralAndNeverOnDisk(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()

	m1, err := NewManager(cfg)
	require.NoError(t, err)
	m2, err := NewManager(cfg)
	require.NoError(t, err)

	require.NotEmpty(t, m1.previewToken)
	require.NotEqual(t, m1.previewToken, m2.previewToken,
		"the preview token must be freshly minted per daemon, not persisted and reused")

	// The token value must appear in no file under the AF home.
	require.NoError(t, filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // unreadable/special file — not where a secret would be
		}
		require.NotContains(t, string(data), m1.previewToken,
			"the preview token must never be written to disk (found in %s)", path)
		return nil
	}))
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
