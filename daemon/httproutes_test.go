package daemon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPRoutes_MatchRegisteredMux is the drift guard: it proves the mux the
// daemon actually serves registers PRECISELY the routes HTTPRoutes() reports —
// no more, no less. Since both newHTTPMux and HTTPRoutes read the same
// httpRoutes table this holds by construction; the test locks it so a future
// change that hand-registers a route (or drops one) fails loudly instead of
// letting the `af api` catalog silently drift from the server.
func TestHTTPRoutes_MatchRegisteredMux(t *testing.T) {
	cs := &controlServer{}
	mux := newHTTPMux(cs)

	routes := HTTPRoutes()
	require.NotEmpty(t, routes)

	// Every cataloged route must be registered: the mux resolves it to a real
	// handler, not the catch-all. An unknown path hits the catch-all (404 with
	// an "unknown route" envelope), so a registered path is one whose handler
	// pattern is NOT "/".
	for _, rt := range routes {
		_, pattern := mux.Handler(mustRequest(t, rt.Method, rt.Path))
		assert.Equalf(t, rt.Path, pattern,
			"route %s %s is in the catalog but not registered on the served mux", rt.Method, rt.Path)
	}

	// Conversely, a path NOT in the catalog must fall through to the catch-all,
	// proving the mux serves nothing beyond the catalog.
	_, pattern := mux.Handler(mustRequest(t, http.MethodPost, "/v1/NotACatalogRoute"))
	assert.Equal(t, "/", pattern,
		"an off-catalog path must hit the catch-all, i.e. the mux serves only the catalog")
}

// TestHTTPRoutes_HealthShape pins the two structural invariants the catalog
// promises: exactly one GET route (health) and every other route a POST under
// /v1/ carrying no leaked unexported handler in its serialized form.
func TestHTTPRoutes_HealthShape(t *testing.T) {
	routes := HTTPRoutes()

	var gets int
	for _, rt := range routes {
		assert.True(t, len(rt.Path) > len("/v1/") && rt.Path[:4] == "/v1/",
			"route path %q must be under /v1/", rt.Path)
		switch rt.Method {
		case http.MethodGet:
			gets++
			assert.Equal(t, "/v1/health", rt.Path, "the only GET route is /v1/health")
			assert.Empty(t, rt.RequestFields, "health takes no request body")
		case http.MethodPost:
			// fine
		default:
			t.Fatalf("unexpected method %q for %q", rt.Method, rt.Path)
		}
	}
	assert.Equal(t, 1, gets, "exactly one GET route (health)")
}

// TestHTTPRoutes_RequestFieldsMatchWireStruct spot-checks that request_fields is
// derived from the actual RPC request struct's json tags (not a drifting
// hand-list): CreateSession's fields must match its wire shape.
func TestHTTPRoutes_RequestFieldsMatchWireStruct(t *testing.T) {
	var create *HTTPRoute
	for i := range httpRoutes {
		if httpRoutes[i].Path == "/v1/CreateSession" {
			create = &httpRoutes[i]
			break
		}
	}
	require.NotNil(t, create)
	assert.Equal(t,
		[]string{"title", "title_base", "repo_path", "program", "prompt", "auto_yes", "in_place", "force_remote"},
		create.RequestFields,
		"request_fields must mirror CreateSessionRequest's json tags in declaration order")
}

func mustRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://localhost"+path, nil)
	require.NoError(t, err)
	return req
}
