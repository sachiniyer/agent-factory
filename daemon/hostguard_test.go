package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The Host guard (hostguard.go). The property: the control listener answers a
// request only when its Host names this daemon, and refuses every other Host
// before routing — static shell, /v1 reads, /v1 mutations and WebSocket upgrades.

// hostDo sends method path to the listener at addr carrying Host host and returns
// the status, the Content-Type and the body.
func hostDo(t *testing.T, addr, method, host, path, body string) (int, string, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+loopbackDialAddr(addr)+path, rdr)
	require.NoError(t, err)
	req.Host = host
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := loopbackHTTPClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

// rawStatus writes request verbatim to addr and returns the response status — the
// way to send a Host Go's client would normalize or refuse to send at all.
func rawStatus(t *testing.T, addr, request string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", loopbackDialAddr(addr), 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = io.WriteString(conn, request)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func hostGuardListener(t *testing.T) (*Manager, string, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, _, addr := boundWebListeners(t, cfg)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return m, addr, port
}

// A rebound name is refused on every kind of route, with the refusal naming the
// Host and the exact line that would allow it — and the SAME request addressed to
// this daemon is served (the differential).
func TestHostGuard_RefusesForeignHostBeforeRouting(t *testing.T) {
	_, addr, port := hostGuardListener(t)
	foreign := "attacker.example:" + port
	routes := []struct {
		name, method, path, body string
		allowedStatus            int // 0 = any status but 421, reached the route
	}{
		{"static shell", http.MethodGet, "/", "", http.StatusOK},
		{"static deep link", http.MethodGet, "/sessions/abc", "", http.StatusOK},
		{"v1 read", http.MethodPost, "/v1/Snapshot", "{}", http.StatusOK},
		{"v1 health", http.MethodGet, "/v1/health", "", http.StatusOK},
		{"v1 auth probe", http.MethodGet, authInfoPath, "", http.StatusOK},
		{"v1 mutating", http.MethodPost, "/v1/KillSession", `{"title":"no-such-session"}`, 0},
		{"CORS preflight", http.MethodOptions, "/v1/Snapshot", "", http.StatusNoContent},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			status, ctype, body := hostDo(t, addr, rt.method, foreign, rt.path, rt.body)
			require.Equal(t, http.StatusMisdirectedRequest, status, "a foreign Host must be refused before routing: %s", body)
			assert.Equal(t, "text/plain; charset=utf-8", ctype)
			assert.Contains(t, body, `"attacker.example:`+port+`"`, "the refusal names the Host it refused")
			assert.Contains(t, body, "af config set network.allowed_hosts attacker.example")
			assert.Contains(t, body, `allowed_hosts = ["attacker.example"]`)
			assert.NotContains(t, body, `"error"`, "the guard answered, not the mux's JSON envelope")

			// Differential: the same request addressed to this daemon reaches the route.
			okStatus, okType, okBody := hostDo(t, addr, rt.method, addr, rt.path, rt.body)
			require.NotEqual(t, http.StatusMisdirectedRequest, okStatus, okBody)
			if rt.allowedStatus != 0 {
				require.Equal(t, rt.allowedStatus, okStatus, okBody)
			} else {
				assert.Contains(t, okType, "application/json", "the mutating route ran and answered its own envelope")
			}
		})
	}
}

// The WebSocket upgrade rides the same guard: a foreign Host never gets a 101.
func TestHostGuard_RefusesForeignHostOnWebSocketUpgrade(t *testing.T) {
	_, addr, port := hostGuardListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := "ws://" + loopbackDialAddr(addr) + "/v1/events"

	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: loopbackHTTPClient, Host: "attacker.example:" + port})
	require.Error(t, err, "a foreign Host must not upgrade")
	require.NotNil(t, resp)
	require.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode)

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: loopbackHTTPClient, Host: "localhost:" + port})
	require.NoError(t, err, "the same upgrade addressed to this daemon succeeds")
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// Loopback names work on any port, in any case, with one trailing root dot.
func TestHostGuard_AdmitsLoopbackNamesOnAnyPort(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	for _, host := range []string{
		"localhost", "localhost:1", "LOCALHOST:65535", "localhost.:8443", "Localhost.",
		"127.0.0.1", "127.0.0.1:9", "127.8.9.10:8443",
		"[::1]", "[::1]:8443", "[::ffff:127.0.0.1]:80",
	} {
		status, _, body := hostDo(t, addr, http.MethodGet, host, "/v1/health", "")
		assert.Equal(t, http.StatusOK, status, "Host %q must be admitted: %s", host, body)
	}
}

// A *.localhost SUBDOMAIN is refused on the control listener — including one in
// the exact shape af mints for its preview and editor origins. Safari before
// macOS 26 resolves *.localhost through the network's DNS (WebKit bug 160504), so
// a hostile resolver can serve a page under such a name and rebind it to
// 127.0.0.1; the af label shape is public, and this listener does not check the
// per-label credential, so admitting the shape would admit an attacker's label.
// af's own *.localhost origins live on the preview listener (pinned below). An
// operator who wants one here can still list it.
func TestHostGuard_RefusesLocalhostSubdomains(t *testing.T) {
	m, addr, port := hostGuardListener(t)
	forged := "af" + strings.Repeat("a", previewLabelEncoding.EncodedLen(previewLabelBytes)) + previewHostSuffix
	require.True(t, isPreviewLabel(strings.TrimSuffix(forged, previewHostSuffix)), "the forged label must have af's exact shape")
	for _, host := range []string{
		"x.localhost:" + port,
		"Editor.LOCALHOST",
		"evil.localhost.",
		forged + ":" + port,
		previewTabHostLabel("secret", "sid", "tid") + previewHostSuffix + ":8444",
		editorOriginLabel("secret", "sid") + previewHostSuffix,
		previewProbeLabel + previewHostSuffix,
	} {
		status, _, body := hostDo(t, addr, http.MethodPost, host, "/v1/Snapshot", "{}")
		assert.Equal(t, http.StatusMisdirectedRequest, status, "Host %q must be refused: %s", host, body)
	}

	next := *m.Config()
	next.AllowedHosts = []string{"dev.localhost"}
	m.storeLivePosture(&next)
	status, _, body := hostDo(t, addr, http.MethodPost, "dev.localhost:"+port, "/v1/Snapshot", "{}")
	assert.Equal(t, http.StatusOK, status, "an operator may list a *.localhost name on purpose: %s", body)
}

// Every spelling a browser never sends is refused on purpose, and the empty Host
// is admitted only because the peer is loopback (an HTTP/1.0 tool).
func TestHostGuard_EdgeSpellings(t *testing.T) {
	_, addr, port := hostGuardListener(t)
	get := func(hostLine string) string {
		return "GET /v1/health HTTP/1.1\r\n" + hostLine + "Connection: close\r\n\r\n"
	}
	// Admitted.
	assert.Equal(t, http.StatusOK, rawStatus(t, addr, "GET /v1/health HTTP/1.0\r\n\r\n"),
		"an HTTP/1.0 request with no Host from a loopback peer is a local tool, never a browser")
	assert.Equal(t, http.StatusOK, rawStatus(t, addr, get("Host: \r\n")),
		"an empty Host from a loopback peer is admitted for the same reason")
	assert.Equal(t, http.StatusOK, rawStatus(t, addr, get("Host: [::1]\r\n")))
	assert.Equal(t, http.StatusOK, rawStatus(t, addr, get("Host: [::1]:"+port+"\r\n")))
	// Refused: an IPv6 literal must be bracketed, a port must be numeric, a second
	// trailing dot is an empty label, and a trailing dot does not launder a foreign
	// name.
	for _, host := range []string{"::1", "::1:" + port, "localhost:abc", "localhost..", "attacker.example.", "ATTACKER.example:" + port} {
		assert.Equal(t, http.StatusMisdirectedRequest, rawStatus(t, addr, get("Host: "+host+"\r\n")), "Host %q", host)
	}
	// Go's server already answers an HTTP/1.1 request with NO Host header 400, before
	// the guard — the guard's empty-Host branch is reached only by HTTP/1.0 or an
	// explicitly empty header.
	assert.Equal(t, http.StatusBadRequest, rawStatus(t, addr, "GET /v1/health HTTP/1.1\r\nConnection: close\r\n\r\n"))
}

// network.allowed_hosts admits the name it lists, on any port and spelling, and a
// change applies on the next request with no rebind — in both directions.
func TestHostGuard_ConfiguredNameAppliesLiveWithoutRebind(t *testing.T) {
	m, addr, port := hostGuardListener(t)
	const proxied = "siyer-dev-machine.lonk-forel.ts.net"
	status, _, body := hostDo(t, addr, http.MethodPost, proxied, "/v1/Snapshot", "{}")
	require.Equal(t, http.StatusMisdirectedRequest, status)
	assert.Contains(t, body, "af config set network.allowed_hosts "+proxied)

	next := *m.Config()
	next.AllowedHosts = []string{proxied}
	m.storeLivePosture(&next)
	for _, host := range []string{proxied, strings.ToUpper(proxied), proxied + ".", proxied + ":443", proxied + ":" + port} {
		status, _, body := hostDo(t, addr, http.MethodPost, host, "/v1/Snapshot", "{}")
		assert.Equal(t, http.StatusOK, status, "Host %q: %s", host, body)
	}
	subStatus, _, _ := hostDo(t, addr, http.MethodGet, "sub."+proxied, "/", "")
	assert.Equal(t, http.StatusMisdirectedRequest, subStatus, "an entry is exact: it does not admit its subdomains")
	require.Equal(t, addr, m.lifecycle.snapshot().listeners.TCPBoundAddr, "the allow-list change must not rebind")

	removed := *m.Config()
	removed.AllowedHosts = nil
	m.storeLivePosture(&removed)
	status, _, _ = hostDo(t, addr, http.MethodPost, proxied, "/v1/Snapshot", "{}")
	assert.Equal(t, http.StatusMisdirectedRequest, status, "removing the entry applies on the next request too")
}

// The Host verdict and the auth decision come from ONE config read: the guard
// builds the posture and the gate serves that same value rather than re-reading.
// A second read could pair an old allow-list with a newer, relaxed token posture.
func TestHostGuard_SharesThePostureReadWithTheGate(t *testing.T) {
	var reads int32
	// What a second read would see: a gate demanding a token nobody presents. It
	// must fail with 401, not panic, so a regression reads as a clean failure.
	strict := &authGate{expectedToken: func() (string, error) { return "unpresented", nil }}
	build := func() requestPosture {
		if atomic.AddInt32(&reads, 1) == 1 {
			return requestPosture{allowedHosts: []string{"box.example"}} // nil gate: authorized
		}
		return requestPosture{gate: strict} // what a second read would see: no allow-list, strict gate
	}
	served := 0
	inner := withLivePosture(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}), build)
	h := withLiveHostGuard(webShellHandler(inner), "test listener", "", build)

	for _, path := range []string{"/v1/health", "/"} {
		atomic.StoreInt32(&reads, 0)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "box.example:8443"
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s: served under the posture the guard read, not a second one", path)
		require.Equal(t, int32(1), atomic.LoadInt32(&reads), "%s: exactly one posture read per request", path)
	}
	require.Equal(t, 1, served, "only the /v1 request reaches the mux; / is the static shell")
}

func TestAdmitHost(t *testing.T) {
	req := func(host, remote string, local net.Addr) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.Host = host
		r.RemoteAddr = remote
		if local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		}
		return r
	}
	lan := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8443}

	t.Run("missing Host is admitted only from a loopback peer", func(t *testing.T) {
		_, ok := admitHost(req("", "127.0.0.1:5555", nil), "", nil)
		assert.True(t, ok)
		refusal, ok := admitHost(req("", "192.0.2.1:5555", nil), "", nil)
		assert.False(t, ok)
		assert.True(t, refusal.missing)
		assert.Contains(t, hostRefusalBody(refusal), "no Host header")
	})
	t.Run("the bind address's own host", func(t *testing.T) {
		_, ok := admitHost(req("Box.LAN:8443", "192.0.2.1:1", nil), "box.lan", nil)
		assert.True(t, ok)
		refusal, ok := admitHost(req("other.lan:8443", "192.0.2.1:1", nil), "box.lan", nil)
		assert.False(t, ok)
		assert.Equal(t, "other.lan", refusal.suggest)
	})
	t.Run("the address the client reached, for a wildcard bind", func(t *testing.T) {
		_, ok := admitHost(req("192.0.2.10:8443", "192.0.2.1:1", lan), "", nil)
		assert.True(t, ok)
		mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.10"), Port: 8443}
		_, ok = admitHost(req("192.0.2.10", "192.0.2.1:1", mapped), "", nil)
		assert.True(t, ok, "a dual-stack socket reports the IPv4 peer's local address 4-in-6")
		v6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::10"), Port: 8443}
		_, ok = admitHost(req("[2001:DB8::10]:8443", "[2001:db8::1]:1", v6), "", nil)
		assert.True(t, ok)
		refusal, ok := admitHost(req("192.0.2.11:8443", "192.0.2.1:1", lan), "", nil)
		assert.False(t, ok, "another IP is not this connection's address")
		assert.Equal(t, "192.0.2.11", refusal.suggest)
		_, ok = admitHost(req("0.0.0.0:8443", "127.0.0.1:1", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8443}), "", nil)
		assert.False(t, ok, "the unspecified address names no daemon unless the operator lists it")
	})
	t.Run("allow-list entries are canonicalized even when hand-built", func(t *testing.T) {
		_, ok := admitHost(req("box.ts.net", "127.0.0.1:1", nil), "", []string{"BOX.ts.net."})
		assert.True(t, ok)
		_, ok = admitHost(req("box.ts.net", "127.0.0.1:1", nil), "", []string{"https://box.ts.net"})
		assert.False(t, ok, "a malformed entry admits nothing")
	})
	t.Run("a malformed Host suggests no line", func(t *testing.T) {
		refusal, ok := admitHost(req("::1", "127.0.0.1:1", nil), "", nil)
		assert.False(t, ok)
		assert.Empty(t, refusal.suggest)
		body := hostRefusalBody(refusal)
		assert.Contains(t, body, "not a valid host name")
		assert.NotContains(t, body, "allowed_hosts")
	})
	t.Run("a huge Host is truncated in the body", func(t *testing.T) {
		long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example"
		refusal, ok := admitHost(req(long, "127.0.0.1:1", nil), "", nil)
		require.False(t, ok)
		assert.Contains(t, hostRefusalBody(refusal), "…")
	})
}

// A per-session editor-origin port runs the same guard in front of its handler,
// with the front-door name allowed only once it is listed.
func TestHostGuard_EditorOriginPort(t *testing.T) {
	f := newEditorPortFixture(t)
	h := editorPortServerHandler(f.cs, f.portA, fmt.Sprintf("127.0.0.1:%d", f.portA))
	do := func(host string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, editorPortProbePath, nil)
		r.Host = host
		r.RemoteAddr = "127.0.0.1:40000"
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	front := fmt.Sprintf("box.tailnet.ts.net:%d", f.portA)
	assert.Equal(t, http.StatusMisdirectedRequest, do(front), "the front-door name is refused until it is allowed")
	assert.Equal(t, http.StatusMisdirectedRequest, do(fmt.Sprintf("attacker.example:%d", f.portA)))
	assert.Equal(t, http.StatusMisdirectedRequest, do(fmt.Sprintf("x.localhost:%d", f.portA)), "no *.localhost subdomain here either")
	assert.NotEqual(t, http.StatusMisdirectedRequest, do(fmt.Sprintf("localhost:%d", f.portA)))

	next := *f.m.Config()
	next.AllowedHosts = []string{"box.tailnet.ts.net"}
	f.m.storeLivePosture(&next)
	assert.NotEqual(t, http.StatusMisdirectedRequest, do(front), "one allowed_hosts entry covers the pool ports too, on their own port")
	assert.Equal(t, http.StatusMisdirectedRequest, do(fmt.Sprintf("attacker.example:%d", f.portA)))
}

// The preview listener keeps its own, stricter Host rule: only af-minted
// *.localhost labels are served, and a rebound name gets the fixed 404 on every
// kind of route — static, /v1 read, /v1 mutating, WS upgrade — before the gate.
// af's minted preview origin keeps working there; a forged label of af's exact
// shape reaches the gate and never a tab.
func TestHostGuard_PreviewListenerRefusesForeignHosts(t *testing.T) {
	m, previewAddr, _, sid, tid := startPreviewDaemon(t)
	client := previewHTTPClient()
	minted := previewHostOf(t, m, previewAddr, sid, tid)
	require.Equal(t, http.StatusOK, previewHostStatus(t, client, previewAddr, minted, "/"),
		"the minted per-tab *.localhost origin is served")
	forgedResp := previewHostGet(t, client, previewAddr,
		"af"+strings.Repeat("a", previewLabelEncoding.EncodedLen(previewLabelBytes))+previewHostSuffix, "/")
	forgedBody := readAllString(t, forgedResp)
	assert.NotContains(t, forgedBody, "server=auth", "a shape-valid label af never minted must not reach a dev server")
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)
	foreign := "attacker.example:" + port
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/", ""},
		{http.MethodPost, "/v1/Snapshot", "{}"},
		{http.MethodPost, "/v1/KillSession", `{"title":"x"}`},
	} {
		status, _, body := hostDo(t, previewAddr, rt.method, foreign, rt.path, rt.body)
		assert.Equal(t, http.StatusNotFound, status, "%s %s", rt.method, rt.path)
		assert.Contains(t, body, "web-tab preview origin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws://"+loopbackDialAddr(previewAddr)+"/v1/events",
		&websocket.DialOptions{HTTPClient: loopbackHTTPClient, Host: foreign})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestTightenFrozenPostureIntersectsAllowedHosts(t *testing.T) {
	pinned := &config.Config{AllowedHosts: []string{"a.example", "b.example"}}
	live := &config.Config{AllowedHosts: []string{"b.example", "c.example"}}
	got := tightenFrozenPosture(pinned, live)
	assert.Equal(t, []string{"b.example"}, got.AllowedHosts,
		"a live removal reaches a frozen generation; a live addition never widens it")
	same := &config.Config{AllowedHosts: []string{"a.example", "b.example"}}
	assert.Same(t, pinned, tightenFrozenPosture(pinned, same), "nothing tightened ⇒ the pin itself, no allocation")
}

func TestHostRefusalLog_RateLimited(t *testing.T) {
	l := newHostRefusalLog()
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	var lines []string
	l.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	evil := hostRefusal{host: "evil.example:8443", suggest: "evil.example"}
	for i := 0; i < 5; i++ {
		l.record("web listener 127.0.0.1:8443", evil, "127.0.0.1:5000")
	}
	require.Len(t, lines, 1, "one line per refused name per interval")
	assert.Contains(t, lines[0], `"evil.example:8443"`)
	assert.Contains(t, lines[0], "127.0.0.1:5000")
	assert.Contains(t, lines[0], "af config set network.allowed_hosts evil.example")

	// A flood of distinct names is capped across all of them.
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("n%d.example", i)
		l.record("web listener 127.0.0.1:8443", hostRefusal{host: name, suggest: name}, "127.0.0.1:5000")
	}
	require.Len(t, lines, hostRefusalLogBurst, "at most hostRefusalLogBurst lines a minute")

	now = now.Add(time.Minute)
	l.record("web listener 127.0.0.1:8443", hostRefusal{host: "late.example", suggest: "late.example"}, "127.0.0.1:5000")
	require.Len(t, lines, hostRefusalLogBurst+1)
	assert.Contains(t, lines[len(lines)-1], "41 further refusals not logged", "suppressed refusals are counted, not lost")

	now = now.Add(hostRefusalLogInterval)
	l.record("web listener 127.0.0.1:8443", evil, "127.0.0.1:5000")
	require.Len(t, lines, hostRefusalLogBurst+2, "a name logs again once its interval has passed")

	snap := l.snapshot()
	require.LessOrEqual(t, len(snap), hostRefusalRecentMax)
	require.NotEmpty(t, snap)
	assert.Equal(t, "evil.example", snap[0].Name, "most recent first")
}

// A remote `af` CLI quotes only the first bodySnippetLimit bytes of a refusal, so
// the command that fixes it must land inside them even for a long tailnet name.
func TestHostRefusalBody_FixFitsTheCLISnippet(t *testing.T) {
	r := hostRefusal{host: "siyer-dev-machine.lonk-forel.ts.net:8443", suggest: "siyer-dev-machine.lonk-forel.ts.net"}
	snippet := strings.Join(strings.Fields(hostRefusalBody(r)), " ")
	require.Greater(t, len(snippet), 200)
	assert.Contains(t, snippet[:200], "af config set network.allowed_hosts siyer-dev-machine.lonk-forel.ts.net")
}
