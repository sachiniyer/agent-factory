package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// staticGate is an authGate whose expected token is a fixed string, for the
// enforcement matrix below. An empty tok models the fail-closed case (a daemon
// with no token must reject every credential).
func staticGate(tok string) *authGate {
	return &authGate{expectedToken: func() (string, error) { return tok, nil }}
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
