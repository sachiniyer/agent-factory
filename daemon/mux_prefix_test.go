package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every route in the served table must live under /v1/ (#2928).
//
// The listeners compose shell(gate(mux)), and all three shells — webShellHandler,
// previewShellHandler's control-plane sibling, and noWebShellHandler — forward on
// strings.HasPrefix(path, "/v1/"). A route outside that prefix never reaches the
// mux on a TCP listener: it is shadowed before the gate is consulted, and answers
// the SPA's index.html with a 200 rather than a 404 that would name the mistake.
// It fails safe — a dead route, never an exposed one — which is exactly why it is
// worth a test: the symptom is confusing rather than alarming, and `/metrics` or
// `/healthz` work fine over the unix socket, so local testing passes.
//
// SCOPE, stated exactly, because the scope is the whole design of this test.
// It covers the served route TABLE — servedHTTPRoutes(), i.e. the public catalog
// PLUS internalHTTPRoutes — exhaustively, by iteration. That was the real gap:
// TestHTTPRoutes_HealthShape asserts the same prefix but iterates only
// HTTPRoutes(), so the internal routes were covered by nothing.
//
// It does NOT cover the hand-registered routes (the WS stream/events routes, the
// config-assistant trio, preview-auth, the webtab proxy). Four attempts to cover
// them are worth recording, because the lesson generalizes past this file:
//
//   - A source scan for `mux.HandleFunc` missed a receiver named anything else,
//     a second table reusing the `rt.Path` idiom, a method-qualified `GET /`, and
//     an identifier merely PREFIXED with `webtabPathPrefix`.
//   - Probing the built mux for representative non-/v1 paths missed whichever
//     path was not probed — `/metrics` caught, `/metrics/foo` not — plus
//     host-qualified patterns, since a probe fixes the Host.
//
// Both are approximations of "what does this mux serve", and Go's ServeMux does
// not expose its patterns, so neither can be made complete. A guard that reports
// clean over the paths it happens to look at is the failure this file exists to
// prevent, so the incomplete half was removed rather than extended again. What
// remains is exhaustive over what it claims and silent about the rest.
//
// If the hand-registered half ever needs real coverage, the honest way is a
// recording mux at the registration seam — newHTTPMux taking an interface the
// test can substitute — not a cleverer approximation from the outside.
func TestServedRoutesAreAllUnderV1(t *testing.T) {
	served := servedHTTPRoutes()
	require.NotEmpty(t, served, "an empty table would make every assertion below vacuous")

	for _, rt := range served {
		assert.Truef(t, strings.HasPrefix(rt.Path, "/v1/"),
			"served route %s %s is not under /v1/, so a TCP listener's shell shadows it before the gate: "+
				"it is unreachable there and answers the SPA shell with a 200, not a 404. Register it under /v1/.",
			rt.Method, rt.Path)
	}
}
