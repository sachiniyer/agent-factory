package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithAuthCORSPreflight pins the auth/CORS seam: a CORS preflight is answered
// by the middleware (204 + permissive CORS headers) before any route runs, and a
// real request still passes through to its handler with the CORS headers set.
func TestWithAuthCORSPreflight(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}))

	// Preflight: answered by the seam, never reaching a route handler.
	pre := httptest.NewRequest(http.MethodOptions, "/v1/sessions/x/stream", nil)
	preRec := httptest.NewRecorder()
	h.ServeHTTP(preRec, pre)
	if preRec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", preRec.Code)
	}
	if got := preRec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("preflight CORS origin = %q, want *", got)
	}

	// A real request passes through the seam to its handler and still carries the
	// CORS headers.
	get := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("health status through seam = %d, want 200", getRec.Code)
	}
	if got := getRec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("health CORS origin = %q, want *", got)
	}
}

// TestWithAuthTokenSeamIsNoOp pins that the auth seam does NOT enforce a token in
// Phase 2 — a request with no credential is served normally (the unix-socket peer
// is trusted; Phase 3 fills in enforcement here without reshaping routes).
func TestWithAuthTokenSeamIsNoOp(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated request status = %d, want 200 (seam is a no-op in Phase 2)", rec.Code)
	}
}
