package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every path served behind the /v1 shell must live under /v1/ (#2928).
//
// The daemon's web listener composes shell(gate(mux)), and webShellHandler /
// noWebShellHandler route by PREFIX: `/v1/…` reaches the gated mux, everything
// else goes to the static SPA or a 404. A route registered outside /v1/ never
// reaches the mux there — it is shadowed before the gate is consulted.
//
// It fails SAFE (a dead route, never an exposed one), which is why it recurs:
// the symptom is confusing rather than alarming. `GET /metrics` or `/healthz`
// are natural names, they work over the unix socket so local testing passes, and
// over the web listener they silently answer index.html with a 200 — serveSPA's
// fallback — rather than a 404 that would name the mistake.
//
// WHY THIS IS RUNTIME AND NOT A SOURCE SCAN. The first four versions of this
// guard parsed the package's own registrations, and each review round found
// another thing the parse could not see: a receiver not named `mux`, a second
// route table reusing the `rt.Path` idiom, a `GET /` that looked like the 404
// fallback, an identifier merely PREFIXED with `webtabPathPrefix`. Every one was
// the same defect — a check claiming coverage it did not have — which is exactly
// what this guard exists to prevent, reproduced inside the guard itself. A
// static approximation of "what does this mux serve" will keep having holes,
// so this asks the mux instead.
//
// WHAT IT DOES AND DOES NOT PROVE, stated plainly rather than implied:
//   - The route TABLE half is exhaustive. Every servedHTTPRoutes() entry — the
//     public catalog plus internalHTTPRoutes — is checked, by iteration.
//   - The hand-registered half is PROBE-based. It asks the real mux where a set
//     of realistic non-/v1 paths route, and a new route outside /v1/ is caught
//     only if it collides with a probe. That is narrower than a source scan
//     aspires to be, and unlike the source scan it never reports clean over
//     something it did not read.

// nonV1Probes are paths a reasonable person might register outside /v1/. Each
// must reach the catch-all and nothing else. Names, not exotic strings: the
// failure being guarded is someone adding an ordinary-looking operational route.
var nonV1Probes = []string{
	"/metrics",
	"/healthz",
	"/health",
	"/status",
	"/ready",
	"/livez",
	"/debug/pprof/",
	"/api/sessions",
	"/webtab/s/t/index.html", // the webtab proxy WITHOUT its /v1 prefix
	"/events",
	"/stream",
	"/v2/Snapshot",
}

func TestNoNonV1PathIsServedByTheDaemonMux(t *testing.T) {
	// The route table, exhaustively. newHTTPMux registers rt.Path for every
	// servedHTTPRoutes() entry, while TestHTTPRoutes_HealthShape iterates only
	// HTTPRoutes() — so internalHTTPRoutes was covered by nothing before this.
	served := servedHTTPRoutes()
	require.NotEmpty(t, served)
	for _, rt := range served {
		assert.Truef(t, strings.HasPrefix(rt.Path, "/v1/"),
			"served route %s %s is not under /v1/, so the web listener's shell shadows it before the gate",
			rt.Method, rt.Path)
	}

	// The hand-registered routes, by asking the mux. mux.Handler returns the
	// PATTERN that matched, so "the catch-all answered" is directly observable
	// rather than inferred from source.
	mux := newHTTPMux(&controlServer{})
	for _, probe := range nonV1Probes {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			_, pattern := mux.Handler(mustRequest(t, method, probe))
			assert.Equalf(t, "/", pattern,
				"%s %s is served by pattern %q, but every non-/v1 path must fall through to the catch-all: "+
					"the web listener's shell routes only /v1/… into this mux, so that route is unreachable "+
					"there and answers index.html with a 200. Register it under /v1/.",
				method, probe, pattern)
		}
	}

	// …and the catch-all really is the 404 fallback, not some real handler that
	// happens to sit in the "/" slot. Without this the assertion above would be
	// satisfied by a mux whose root pattern SERVES those paths.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mustRequest(t, http.MethodGet, "/metrics"))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"the \"/\" pattern must be the unknown-route 404, or 'it matched the catch-all' means nothing")
	assert.Contains(t, rec.Body.String(), "unknown route",
		"the catch-all must be the envelope 404 (newHTTPMux), not a route that adopted the root slot")
}
