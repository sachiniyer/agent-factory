package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// staticGate is an authGate whose expected token is a fixed string, for the
// enforcement matrix below. An empty tok models the fail-closed case (a daemon
// with no token must reject every credential). Its policy fields are the
// fail-safe zero value (token mandatory, no loopback exemption).
func staticGate(tok string) *authGate {
	return &authGate{expectedToken: func() (string, error) { return tok, nil }}
}

// webGate is the daemon's web-listener posture (#1696): a real token, loopback
// exempt, token disabled iff tokenDisabled. It is the gate the loopback /
// network / spoof / require_token=false matrix below exercises.
func webGate(tok string, tokenDisabled bool) *authGate {
	return &authGate{
		expectedToken:  func() (string, error) { return tok, nil },
		tokenDisabled:  tokenDisabled,
		loopbackExempt: true,
	}
}

// reqFrom builds a GET /v1/health request from a specific transport peer
// (remoteAddr, "host:port"), optionally carrying tok as a Bearer header. Header
// spoofing is layered on by the caller.
func reqFrom(remoteAddr, tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.RemoteAddr = remoteAddr
	if tok != "" {
		r.Header.Set(agentproto.AuthHeader, agentproto.BearerScheme+tok)
	}
	return r
}

// bearerReq builds a GET /v1/health request carrying tok as an Authorization
// Bearer header (empty tok ⇒ no header, an unauthenticated request).
func bearerReq(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	if tok != "" {
		r.Header.Set(agentproto.AuthHeader, agentproto.BearerScheme+tok)
	}
	return r
}

// TestWithAuthGateEnforcement pins the per-listener token gate: with a non-nil
// gate (the TCP listener, PR3), a request must present the exact bearer token.
func TestWithAuthGateEnforcement(t *testing.T) {
	const good = "s3cr3t-token"
	h := withAuth(newHTTPMux(&controlServer{}), staticGate(good), nil)

	// Valid token in the Authorization header → served (200).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq(good))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", rec.Code)
	}

	// Valid token via the ?access_token= WS/browser fallback → served (200).
	rec = httptest.NewRecorder()
	q := httptest.NewRequest(http.MethodGet, "/v1/health?"+agentproto.AccessTokenQueryParam+"="+good, nil)
	h.ServeHTTP(rec, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid ?access_token= status = %d, want 200", rec.Code)
	}

	// Wrong token → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq("wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}

	// Missing token → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}
}

// TestWithAuthLoopbackExemption is the #1696 core: with the daemon web policy
// (loopback exempt, token required for the rest), a loopback peer is served with
// NO token while a non-loopback peer still must present one.
func TestWithAuthLoopbackExemption(t *testing.T) {
	const good = "s3cr3t-token"
	h := withAuth(newHTTPMux(&controlServer{}), webGate(good, false), nil)

	// Loopback peers, no token → 200 (exempt, same trust as the unix socket).
	for _, addr := range []string{"127.0.0.1:5555", "[::1]:5555", "127.0.0.5:40000"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqFrom(addr, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("loopback %s no-token status = %d, want 200 (exempt)", addr, rec.Code)
		}
	}

	// Loopback peer with a WRONG token → still 200: on an exempt peer the token
	// is never consulted, so a bogus one cannot cause a rejection.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, reqFrom("127.0.0.1:5555", "garbage"))
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback wrong-token status = %d, want 200 (token ignored on exempt peer)", rec.Code)
	}

	// Network peer, no token → 401 (unchanged default: enabling listen_addr on a
	// LAN never silently exposes an unauthenticated control plane).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqFrom("192.0.2.10:33333", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("network no-token status = %d, want 401", rec.Code)
	}

	// Network peer, correct token → 200.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqFrom("192.0.2.10:33333", good))
	if rec.Code != http.StatusOK {
		t.Fatalf("network valid-token status = %d, want 200", rec.Code)
	}

	// Network peer, wrong token → 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqFrom("192.0.2.10:33333", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("network wrong-token status = %d, want 401", rec.Code)
	}
}

// TestWithAuthLoopbackTokenRequired pins the require_loopback_token=true shape:
// a gate with loopbackExempt=false (the policy startHTTPServer builds from that
// config flag) makes loopback peers present the token exactly like network peers.
// This is the shared/multi-user tighten-up — the default no-token loopback access
// would otherwise hand every local account the full control plane.
func TestWithAuthLoopbackTokenRequired(t *testing.T) {
	const good = "s3cr3t-token"
	gate := &authGate{
		expectedToken:  func() (string, error) { return good, nil },
		tokenDisabled:  false,
		loopbackExempt: false, // require_loopback_token=true
	}
	h := withAuth(newHTTPMux(&controlServer{}), gate, nil)

	// Loopback peers, no token → 401 (exemption withdrawn).
	for _, addr := range []string{"127.0.0.1:5555", "[::1]:5555"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqFrom(addr, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("loopback %s no-token status = %d, want 401 (require_loopback_token)", addr, rec.Code)
		}
	}

	// Loopback peer WITH the correct token → 200.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, reqFrom("127.0.0.1:5555", good))
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback valid-token status = %d, want 200", rec.Code)
	}

	// The auth-info probe now tells a loopback client it needs a token, so the
	// SPA shows its login instead of assuming a free pass.
	probe := httptest.NewRecorder()
	probeReq := reqFrom("127.0.0.1:5555", "")
	probeReq.URL.Path = authInfoPath
	h.ServeHTTP(probe, probeReq)
	if probe.Code != http.StatusOK {
		t.Fatalf("auth-info probe status = %d, want 200", probe.Code)
	}
	var env struct {
		Data authInfoResponse `json:"data"`
	}
	if err := json.NewDecoder(probe.Body).Decode(&env); err != nil {
		t.Fatalf("decode auth-info body: %v", err)
	}
	if !env.Data.AuthRequired {
		t.Fatalf("auth_required = false, want true for loopback under require_loopback_token")
	}
}

// TestWithAuthLoopbackSpoofResistant pins the security-critical property: loopback
// is judged ONLY from the transport RemoteAddr, so a NETWORK peer that forges
// X-Forwarded-For / X-Real-IP / Forwarded / Host to claim 127.0.0.1 is STILL
// rejected without a token. A header can never grant the loopback exemption.
func TestWithAuthLoopbackSpoofResistant(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}), webGate("s3cr3t-token", false), nil)

	spoofHeaders := map[string]string{
		"X-Forwarded-For": "127.0.0.1",
		"X-Real-Ip":       "127.0.0.1",
		"Forwarded":       "for=127.0.0.1",
		"Host":            "127.0.0.1",
		"Origin":          "http://127.0.0.1",
	}
	// A real network peer dressing up every loopback-implying header, no token.
	req := reqFrom("192.0.2.66:44444", "")
	for k, v := range spoofHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header-spoofed loopback status = %d, want 401 (RemoteAddr wins, headers ignored)", rec.Code)
	}

	// And the auth-info probe must not be fooled either: the network peer learns
	// it DOES need a token despite the spoofed headers.
	req = httptest.NewRequest(http.MethodGet, authInfoPath, nil)
	req.RemoteAddr = "192.0.2.66:44444"
	for k, v := range spoofHeaders {
		req.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := decodeAuthInfo(t, rec); got.AuthRequired != true {
		t.Fatalf("spoofed auth-info auth_required = %v, want true", got.AuthRequired)
	}
}

// TestWithAuthTokenDisabled pins require_token=false: with the token disabled a
// NETWORK peer with no token is served (the deliberate trusted-network opt-out),
// and so is loopback.
func TestWithAuthTokenDisabled(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}), webGate("s3cr3t-token", true), nil)

	for _, addr := range []string{"192.0.2.10:33333", "127.0.0.1:5555"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqFrom(addr, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("token-disabled %s no-token status = %d, want 200", addr, rec.Code)
		}
	}
}

// TestAuthInfoProbe pins the unauthenticated discovery endpoint the SPA uses to
// decide whether to show its paste-token login. It is answered with NO token and
// reports auth_required per the requesting peer + gate policy.
func TestAuthInfoProbe(t *testing.T) {
	cases := []struct {
		name       string
		gate       *authGate
		remoteAddr string
		want       bool
	}{
		{"nil gate (unix socket) never requires", nil, "192.0.2.10:1", false},
		{"web gate + loopback ⇒ no token", webGate("tok", false), "127.0.0.1:1", false},
		{"web gate + network ⇒ token required", webGate("tok", false), "192.0.2.10:1", true},
		{"require_token=false + network ⇒ no token", webGate("tok", true), "192.0.2.10:1", false},
		{"strict gate + loopback ⇒ token required", staticGate("tok"), "127.0.0.1:1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := withAuth(newHTTPMux(&controlServer{}), tc.gate, nil)
			req := httptest.NewRequest(http.MethodGet, authInfoPath, nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("auth-info status = %d, want 200", rec.Code)
			}
			if got := decodeAuthInfo(t, rec); got.AuthRequired != tc.want {
				t.Fatalf("auth_required = %v, want %v", got.AuthRequired, tc.want)
			}
		})
	}
}

// decodeAuthInfo unwraps the {data:{auth_required},error} envelope the probe
// returns.
func decodeAuthInfo(t *testing.T, rec *httptest.ResponseRecorder) authInfoResponse {
	t.Helper()
	var env struct {
		Data  authInfoResponse `json:"data"`
		Error *string          `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode auth-info envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Error != nil {
		t.Fatalf("auth-info returned error envelope: %q", *env.Error)
	}
	return env.Data
}

// TestWithAuthGateFailsClosed pins that an empty expected token denies every
// request (a daemon with no token must never accept the empty credential), and
// that an error resolving the expected token also denies.
func TestWithAuthGateFailsClosed(t *testing.T) {
	// Empty expected token: even a request presenting "" is rejected.
	h := withAuth(newHTTPMux(&controlServer{}), staticGate(""), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty expected token, no cred status = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq("anything"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty expected token, some cred status = %d, want 401", rec.Code)
	}

	// Expected-token resolution error (e.g. token file unreadable): deny.
	errGate := &authGate{expectedToken: func() (string, error) {
		return "", http.ErrNoLocation
	}}
	h = withAuth(newHTTPMux(&controlServer{}), errGate, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerReq("whatever"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token-resolution error status = %d, want 401", rec.Code)
	}
}

// TestWithAuthNilGateBypasses is the regression guard for local trust: the unix
// socket passes a nil gate, so every request is authorized regardless of the
// (missing or bogus) credential. This is why enforcement stays DARK until PR3.
func TestWithAuthNilGateBypasses(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}), nil, nil)

	for _, tok := range []string{"", "bogus"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, bearerReq(tok))
		if rec.Code != http.StatusOK {
			t.Fatalf("nil-gate request (token=%q) status = %d, want 200 (unix peer trusted)", tok, rec.Code)
		}
	}
}

// TestWithAuthPreflightBeforeGate pins that a CORS preflight is answered (204)
// before the token gate runs — browsers never attach credentials to preflight,
// so cross-origin discovery must work without a token even on a gated listener.
func TestWithAuthPreflightBeforeGate(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}), staticGate("tok"), []string{"https://af.example.com"})
	req := httptest.NewRequest(http.MethodOptions, "/v1/sessions/x/stream", nil)
	req.Header.Set("Origin", "https://af.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://af.example.com" {
		t.Fatalf("preflight ACAO = %q, want the allowed origin echoed", got)
	}
}

// TestCORSAllowList pins the config allow-list (§1.5): an allowed origin is
// echoed with the method/header advertisements, a disallowed origin gets no
// ACAO, and an empty allow-list (the default) emits no ACAO for any origin.
func TestCORSAllowList(t *testing.T) {
	allow := []string{"https://af.example.com", "https://ops.internal:8443"}

	// Allowed origin → echoed verbatim, with methods/headers advertised.
	h := withAuth(newHTTPMux(&controlServer{}), nil, allow)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "https://af.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://af.example.com" {
		t.Fatalf("allowed origin ACAO = %q, want it echoed", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatalf("allowed origin missing Access-Control-Allow-Methods")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("allowed origin Vary = %q, want Origin", got)
	}

	// Disallowed origin → no ACAO.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin ACAO = %q, want empty", got)
	}

	// Empty allow-list (default) → no ACAO even for an origin that would match
	// an entry in a populated list, and the request still passes (nil gate).
	h = withAuth(newHTTPMux(&controlServer{}), nil, nil)
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Origin", "https://af.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("empty allow-list ACAO = %q, want empty", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("empty allow-list request status = %d, want 200", rec.Code)
	}
}
