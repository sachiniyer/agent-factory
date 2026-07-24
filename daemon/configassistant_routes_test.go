package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConfigAssistantRoutesRegistered proves the three routes are wired onto the
// daemon mux with their intended methods. With a nil-manager controlServer each
// handler returns 503 ("no session manager") — so a 503 is proof the route was
// REACHED, distinguishable from the catch-all's 404 "unknown route" for a path
// that was never registered, and from a 405 for the wrong method. The routes ride
// the same withAuth listener wrap as every other route (httpserver.go), so no
// per-route auth check is needed or tested here.
func TestConfigAssistantRoutesRegistered(t *testing.T) {
	mux := newHTTPMux(&controlServer{}) // nil manager → handlers answer 503
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path string) int {
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/v1/config-assistant", http.StatusServiceUnavailable},
		{http.MethodGet, "/v1/config-assistant/stream", http.StatusServiceUnavailable},
		{http.MethodDelete, "/v1/config-assistant", http.StatusServiceUnavailable},
		// A method the routes do not serve does NOT reach the 503 handler: the
		// method-scoped patterns miss and the request falls to the catch-all 404.
		// So PUT → 404 (not 503) is proof the registration is method-scoped.
		{http.MethodPut, "/v1/config-assistant", http.StatusNotFound},
		// An unregistered neighbour path also falls to the catch-all 404, so the
		// 503s above are not just "everything answers 503".
		{http.MethodGet, "/v1/config-assistant/nope", http.StatusNotFound},
	}
	for _, c := range cases {
		if got := do(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

// TestStatusForConfigAssistant pins the hub-error → HTTP status mapping the stream
// and spawn routes rely on: no assistant is a settle-and-stop 404 for the browser,
// an unwired build is a 503, and any other failure is a 500.
func TestStatusForConfigAssistant(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errNoConfigAssistant, http.StatusNotFound},
		{fmt.Errorf("wrapped: %w", errNoConfigAssistant), http.StatusNotFound},
		// An aborted create is retryable (409), NOT the stream route's settle-and-stop
		// 404 (#2467 review round 2).
		{errConfigAssistantSpawnAborted, http.StatusConflict},
		{fmt.Errorf("wrapped: %w", errConfigAssistantSpawnAborted), http.StatusConflict},
		{errConfigAssistantUnavailable, http.StatusServiceUnavailable},
		{errors.New("spawn blew up"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := statusForConfigAssistant(c.err); got != c.want {
			t.Errorf("statusForConfigAssistant(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
