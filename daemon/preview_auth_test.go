package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The web-tab preview origin's CREDENTIAL (#1856 steps 2-3). The per-tab credential
// is the origin's own unguessable hostname (previewTabHostLabel), vended by
// GET /v1/preview-auth on the control listener. These tests pin the credential
// SEPARATION (the daemon bearer never authorizes the preview origin, and vice
// versa), the vending endpoint's auth + selector + bound-only contract, and the
// secret's ephemerality. The origin model itself — routing, isolation, the proxy —
// is pinned in preview_origin_test.go.

// startPreviewDaemon brings up a daemon with the preview listener enabled on an
// ephemeral port and returns the manager, the preview bound address, and the
// control bound address. Both listeners are loopback.
func startPreviewDaemon(t *testing.T) (m *Manager, previewAddr, controlAddr, sid, tid string) {
	t.Helper()
	return startPreviewDaemonTuned(t, nil)
}

// startPreviewDaemonTuned is startPreviewDaemon with a config hook, for the tests
// that need a STRICT control listener (require_token + require_loopback_token) so
// auth is actually exercised rather than short-circuited by the tokenless-loopback
// default.
//
// It seeds a REAL web tab, because /v1/preview-auth now mints an origin only for a
// live iframe tab: a synthetic (session, tab) pair would be refused, which is the
// point of that guard rather than an inconvenience to work around.
func startPreviewDaemonTuned(t *testing.T, tune func(*config.Config)) (m *Manager, previewAddr, controlAddr, sid, tid string) {
	t.Helper()
	upstream := echoUpstream(t, "auth")
	m, sid, tabIDs := newPreviewDaemonWithTabs(t, tune, upstream.URL)
	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewBound, "the preview listener must bind for these tests")
	return m, listeners.PreviewBoundAddr, listeners.TCPBoundAddr, sid, tabIDs[0]
}

// noRedirectClient inspects a redirect itself rather than following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// previewHostGet issues a GET to the preview listener at previewAddr with the Host
// header set to host — the only way to address a per-tab preview origin, since
// *.localhost has no DNS entry a Go test client would resolve. It returns the whole
// response so a caller can read status, headers, and body.
func previewHostGet(t *testing.T, client *http.Client, previewAddr, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+previewAddr+path, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// previewHostStatus is previewHostGet reduced to a status code.
func previewHostStatus(t *testing.T, client *http.Client, previewAddr, host, path string) int {
	t.Helper()
	resp := previewHostGet(t, client, previewAddr, host, path)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// previewHostOf returns the Host header addressing tab (sid, tid)'s preview origin
// on previewAddr: the tab's minted label plus the listener's real port.
func previewHostOf(t *testing.T, m *Manager, previewAddr, sid, tid string) string {
	t.Helper()
	origin := previewOriginFor(m, sid, tid)
	require.NotEmpty(t, origin, "the preview listener must be bound for a tab to have an origin")
	u, err := url.Parse(origin)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)
	require.Equal(t, port, u.Port(), "the vended origin must name the port the listener actually bound")
	return u.Host
}

// TestPreviewGate_RejectsUnmintedAndForeignHosts pins the gate: the per-tab host
// label IS the credential, so a request whose Host names no minted tab is refused —
// whether the label is well-formed but unknown (a stale origin from a previous
// daemon, or a guess), or not af-shaped at all.
//
// The two refusals are deliberately DIFFERENT surfaces, and the difference is the
// disclosure rule. A host with no af label never reached a tab-addressing surface at
// all, so previewShellHandler answers it unauthenticated with a plain explanatory
// 404. A well-formed but unknown label goes to the gate — and because that route is
// FRAMED, the gate renders the "expired address" notice rather than a JSON auth
// envelope. Neither reveals whether any particular tab exists.
//
// What is asserted is the PROPERTY, not the status: an unminted label must not reach
// a dev server. Pinning a bare code here is what let the earlier version of this test
// keep asserting a 401 the framed route should never have shown a user.
func TestPreviewGate_RejectsUnmintedAndForeignHosts(t *testing.T) {
	m, previewAddr, _, sid, tid := startPreviewDaemon(t)
	client := previewHTTPClient()
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)

	// A well-formed label this daemon never minted (it hashes under another secret):
	// refused at the gate, and refused as a rendered notice because a pane shows it.
	unminted := previewTabHostLabel("some-other-daemons-secret", "s1", "t1") + previewHostSuffix + ":" + port
	unmintedResp := previewHostGet(t, client, previewAddr, unminted, "/")
	unmintedBody := readAllString(t, unmintedResp)
	require.NotContains(t, unmintedBody, "server=auth",
		"a label this daemon never minted must never reach a dev server — the hostname IS the credential")
	require.Contains(t, unmintedBody, "Preview address expired",
		"a framed refusal must render the notice, not a JSON auth envelope")
	require.NotContains(t, unmintedBody, `"error"`)

	// Not an af label at all: the explanatory 404, before the gate.
	for _, host := range []string{previewAddr, "localhost:" + port, "af-not-a-real-label" + previewHostSuffix + ":" + port} {
		resp := previewHostGet(t, client, previewAddr, host, "/")
		require.Equal(t, http.StatusNotFound, resp.StatusCode,
			"a host with no af-shaped label must get the explanatory 404, not a bare 401 about a bearer token: %q", host)
		body := readAllString(t, resp)
		require.Contains(t, body, "web-tab preview origin")
		require.NotContains(t, body, "8443", "the preview origin must never advertise the control-plane address")
		require.NotContains(t, body, "agent-server")
	}

	// Sanity: a MINTED label reaches past the gate and serves its tab, so the refusals
	// above are the credential working, not a dead listener.
	minted := previewHostOf(t, m, previewAddr, sid, tid)
	require.Equal(t, http.StatusOK, previewHostStatus(t, client, previewAddr, minted, "/"),
		"a minted label authenticates and serves its own tab")
}

// TestPreviewCredentialSeparation_BothDirections is the security core: the daemon
// bearer cannot authorize the preview origin, and a tab's preview credential cannot
// authorize the control plane. Neither credential is honored on the other surface.
//
// One daemon, made strict on the control side (require_token +
// require_loopback_token) so a wrong credential there actually fails rather than
// riding the tokenless-loopback default. The preview gate is always strict.
func TestPreviewCredentialSeparation_BothDirections(t *testing.T) {
	strict := func(cfg *config.Config) {
		cfg.RequireToken = true
		cfg.RequireLoopbackToken = true
	}
	m, previewAddr, controlAddr, sid, tid := startPreviewDaemonTuned(t, strict)
	client := previewHTTPClient()

	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	tabHost := previewHostOf(t, m, previewAddr, sid, tid)
	label, ok := previewHostLabel(tabHost)
	require.True(t, ok)
	require.NotEqual(t, daemonToken, label, "the two credentials must differ")

	// Sanity: each credential DOES authorize its own surface, so the refusals below
	// are separation, not a broken listener.
	require.Equal(t, http.StatusOK, previewHostStatus(t, client, previewAddr, tabHost, "/asset.js"),
		"the tab's own origin authenticates the preview listener and serves its dev server")
	require.Equal(t, http.StatusOK,
		previewGet(t, client, "http://"+controlAddr+"/v1/health", daemonToken),
		"the daemon token authenticates the control plane")

	// Daemon bearer → preview origin: it is not the preview credential, and the
	// preview listener's extractor does not even look at Authorization, so a request
	// on a non-tab host stays refused no matter what bearer it carries.
	req, err := http.NewRequest(http.MethodGet, "http://"+previewAddr+"/asset.js", nil) //nolint:noctx // test client
	require.NoError(t, err)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+daemonToken)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the daemon bearer must not turn a non-tab host into a preview origin")

	// A tab's preview credential → control plane: the control gate wants the daemon
	// token, so it 401s whichever transport carries the label.
	asWebtab := "http://" + controlAddr + webtabPathPrefix + sid + "/" + tid + "/?af_webtab_token=" + label
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, asWebtab, ""),
		"a tab's preview credential must NOT authenticate the control plane")
	require.Equal(t, http.StatusUnauthorized,
		previewGet(t, client, "http://"+controlAddr+"/v1/health", label),
		"a tab's preview credential must NOT authenticate the control plane as a bearer either")
}

// TestPreviewOriginStripsStrandedAfCredential pins #2400's clean-before-render rule
// on the preview origin. Nothing af mints ever puts a credential in a preview-origin
// URL — the hostname carries it — so this only fires for a hand-pasted or bookmarked
// leftover from an earlier release. It must still fire, and BEFORE any target is
// resolved: arbitrary preview code must never render under a document whose own
// window.location holds an af bearer.
//
// Unlike the app-origin bootstrap, it sets NO cookie: there is no query-borne
// credential here to persist, so a param found in the URL is a leftover to remove,
// never one to adopt.
func TestPreviewOriginStripsStrandedAfCredential(t *testing.T) {
	m, previewAddr, _, sid, tid := startPreviewDaemon(t)
	client := noRedirectClient()
	tabHost := previewHostOf(t, m, previewAddr, sid, tid)

	req, err := http.NewRequest(http.MethodGet,
		"http://"+previewAddr+"/app?doc=1&af_webtab_token=daemon-bearer&af_preview_token=stale", nil)
	require.NoError(t, err)
	req.Host = tabHost
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode,
		"a preview-origin URL carrying an af credential must redirect to a clean one, not render")
	require.Equal(t, "/app?doc=1", resp.Header.Get("Location"),
		"EVERY af credential param must be scrubbed; the app's own query survives")
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	require.Empty(t, resp.Cookies(), "the preview origin has no query-borne credential to persist — it sets no cookie")
}

// TestPreviewAuthEndpoint_RequiresAuthThenReturnsOrigin pins the vending endpoint
// against a STRICT control listener (require_token + require_loopback_token), so
// auth is actually exercised rather than short-circuited by the tokenless-loopback
// default: no credential is 401, and the daemon token is 200 with that tab's
// origin. The response is no-store — the origin embeds a capability.
func TestPreviewAuthEndpoint_RequiresAuthThenReturnsOrigin(t *testing.T) {
	strict := func(cfg *config.Config) {
		cfg.RequireToken = true
		cfg.RequireLoopbackToken = true
	}
	m, previewAddr, controlAddr, sid, tid := startPreviewDaemonTuned(t, strict)
	client := previewHTTPClient()
	endpoint := "http://" + controlAddr + "/v1/preview-auth?session=" + url.QueryEscape(sid) +
		"&tab=" + url.QueryEscape(tid)

	// No credential → 401.
	require.Equal(t, http.StatusUnauthorized, previewGet(t, client, endpoint, ""),
		"the origin-vending endpoint must require control-plane auth")

	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+daemonToken)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"),
		"a response carrying a capability must not be cacheable")

	var env struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	origin, err := url.Parse(env.Data.Origin)
	require.NoError(t, err)
	require.Equal(t, "http", origin.Scheme)
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)
	require.Equal(t, port, origin.Port(), "the vended origin must name the preview listener's real port")
	require.True(t, strings.HasSuffix(origin.Hostname(), previewHostSuffix),
		"a per-tab origin must sit under %s so the browser resolves it to loopback with no DNS", previewHostSuffix)
	label, ok := previewHostLabel(origin.Host)
	require.True(t, ok, "the vended host must carry a well-formed af label")
	require.Equal(t, previewTabHostLabel(m.previewSecret, sid, tid), label,
		"the vended origin must be THIS tab's derivation")

	// A pair that names no live tab mints NOTHING, even for a fully authorized
	// caller. The control listener is tokenless for every peer by default, so a
	// cross-origin page can drive this GET without reading the answer; registering
	// unvalidated pairs would let it evict real labels once the registry cap was
	// reached, and every open preview would fail until its pane remounted.
	bogus, err := http.NewRequest(http.MethodGet,
		"http://"+controlAddr+"/v1/preview-auth?session=no-such-session&tab=no-such-tab", nil)
	require.NoError(t, err)
	bogus.Header.Set("Authorization", "Bearer "+daemonToken)
	bogusResp, err := client.Do(bogus)
	require.NoError(t, err)
	defer func() { _ = bogusResp.Body.Close() }()
	require.Equal(t, http.StatusOK, bogusResp.StatusCode)
	var bogusEnv struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(bogusResp.Body).Decode(&bogusEnv))
	require.Empty(t, bogusEnv.Data.Origin,
		"a (session, tab) pair that names no live iframe tab must mint no origin")
	_, registered := m.previewOrigins.lookup(previewTabHostLabel(m.previewSecret, "no-such-session", "no-such-tab"))
	require.False(t, registered, "and it must leave nothing behind in the registry")

	// A missing selector is a 400, never an address that resolves to more than one tab.
	require.Equal(t, http.StatusBadRequest,
		previewGet(t, client, "http://"+controlAddr+"/v1/preview-auth", daemonToken),
		"the endpoint must require a session/tab selector")
	require.Equal(t, http.StatusBadRequest,
		previewGet(t, client, "http://"+controlAddr+"/v1/preview-auth?session=s1", daemonToken))
}

// TestPreviewAuthEndpoint_WithholdsOriginUnlessBound pins that an origin is vended
// only when the preview listener actually BOUND, not merely configured — so a
// bind conflict (non-fatal by design: PreviewConfigured=true, PreviewBound=false)
// does not hand the client an address that will never answer. The client reads an
// empty origin as "keep using the same-origin sandboxed mirror".
func TestPreviewAuthEndpoint_WithholdsOriginUnlessBound(t *testing.T) {
	// Both cases seed a REAL web tab, so the empty answer can only be about the
	// LISTENER. With a synthetic pair the assertion would hold even if the
	// bound-check were deleted — a test that cannot fail.
	upstream := echoUpstream(t, "withhold")

	// Disabled: no preview listener at all.
	t.Run("disabled", func(t *testing.T) {
		m, sid, tabIDs := newPreviewDaemonWithTabs(t, func(cfg *config.Config) {
			cfg.PreviewListenAddr = ""
		}, upstream.URL)
		require.False(t, m.lifecycle.snapshot().listeners.PreviewConfigured)
		requireIframeTab(t, m, sid, tabIDs[0], "the tab is live — only the listener is missing")
		require.Empty(t, fetchPreviewAuthOrigin(t, m, sid, tabIDs[0]),
			"a disabled preview listener must vend no origin")
	})

	// Configured but bind FAILED: the port is already taken.
	t.Run("configured but bind failed", func(t *testing.T) {
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = blocker.Close() }()

		m, sid, tabIDs := newPreviewDaemonWithTabs(t, func(cfg *config.Config) {
			cfg.PreviewListenAddr = blocker.Addr().String() // guaranteed EADDRINUSE
		}, upstream.URL)

		listeners := m.lifecycle.snapshot().listeners
		require.True(t, listeners.PreviewConfigured, "the address was configured")
		require.False(t, listeners.PreviewBound, "the bind failed")
		requireIframeTab(t, m, sid, tabIDs[0], "the tab is live — only the bind failed")
		require.Empty(t, fetchPreviewAuthOrigin(t, m, sid, tabIDs[0]),
			"a configured-but-unbound preview listener must not vend an origin for a dead port")
	})
}

// requireIframeTab asserts the (session, tab) pair really is a live iframe tab, so a
// test that expects an EMPTY origin proves the reason it means to prove rather than
// passing because the tab was never there.
// requireIframeTabOK asserts whether (sid, tid) resolves as a live iframe tab, so a
// test that expects a NEGATIVE proves the reason it means to prove.
func requireIframeTabOK(t *testing.T, m *Manager, sid, tid string, want bool, msgAndArgs ...any) {
	t.Helper()
	_, ok := m.iframeTabKind(sid, tid)
	require.Equal(t, want, ok, msgAndArgs...)
}

func requireIframeTab(t *testing.T, m *Manager, sid, tid, msg string) {
	t.Helper()
	_, ok := m.iframeTabKind(sid, tid)
	require.True(t, ok, msg)
}

// fetchPreviewAuthOrigin calls GET /v1/preview-auth?session=&tab= on m's control
// listener (over its tokenless-loopback default) and returns the vended origin
// (empty when the preview listener is not bound).
func fetchPreviewAuthOrigin(t *testing.T, m *Manager, sid, tid string) string {
	t.Helper()
	controlAddr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	resp, err := previewHTTPClient().Get("http://" + controlAddr + "/v1/preview-auth?session=" +
		url.QueryEscape(sid) + "&tab=" + url.QueryEscape(tid))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var env struct {
		Data previewAuthResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	return env.Data.Origin
}

// TestPreviewSecret_EphemeralAndNeverOnDisk pins the two properties of the per-tab
// HMAC secret: it differs between daemon lifetimes (rotates on restart) and it is
// never written to the AF home. The secret is the root from which every tab's origin
// derives, so it matters more than any single origin that it never lands on disk.
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
