package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every path the daemon serves must live under /v1/ (#2928).
//
// The TCP listener composes shell(gate(mux)), and all three shells route by
// PREFIX: `/v1/…` goes to the gated mux, everything else goes to the static SPA,
// the preview page, or a 404. A route registered outside /v1/ therefore never
// reaches the mux on that listener at all — it is shadowed before the gate is
// consulted.
//
// It fails SAFE (a dead route, never an exposed one), which is exactly why it
// will recur: the symptom is confusing rather than alarming. `GET /metrics` or
// `/healthz` are natural names, they work over the unix socket so local testing
// passes, and over the web listener they silently return index.html with a 200 —
// serveSPA's fallback — rather than a 404 that would name the mistake.
//
// httproutes_test.go already asserts this for the public catalog. This covers the
// HAND-REGISTERED routes, which are the ones a person adds by hand and the ones
// that assertion never saw.

// muxRegistration matches a `mux.HandleFunc(<arg>, …)` call and captures the
// pattern argument, literal or not.
var muxRegistration = regexp.MustCompile(`mux\.HandleFunc\(\s*([^,]+),`)

// nonLiteralPatterns are registration arguments that are not string literals,
// each with WHY it is already covered. An unlisted non-literal fails the test:
// an argument this scanner cannot resolve is a path it cannot check, and
// reporting a clean surface it did not read is the failure mode this guards.
var nonLiteralPatterns = map[string]string{
	"rt.Path":          "the catalog's own paths, asserted /v1/-prefixed by TestHTTPRoutes_HealthShape",
	"webtabPathPrefix": "a const equal to \"/v1/webtab/\", asserted below",
}

func TestEveryMuxPatternIsUnderV1(t *testing.T) {
	// Pin the const the allowlist vouches for, so the vouching cannot go stale.
	require.Equal(t, "/v1/webtab/", webtabPathPrefix,
		"the allowlist entry for webtabPathPrefix asserts this value; if it changed, the invariant changed with it")

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	registrations := 0
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		require.NoError(t, err)
		for _, m := range muxRegistration.FindAllStringSubmatch(string(src), -1) {
			registrations++
			arg := strings.TrimSpace(m[1])
			if !strings.HasPrefix(arg, `"`) {
				// A non-literal: resolvable only via the allowlist above.
				base := strings.SplitN(arg, "+", 2)[0]
				if _, ok := nonLiteralPatterns[strings.TrimSpace(base)]; ok {
					continue
				}
				offenders = append(offenders, name+": non-literal pattern "+arg+
					" — this scanner cannot resolve it, so add it to nonLiteralPatterns with why it is safe")
				continue
			}
			pattern := strings.Trim(arg, `"`)
			// Strip an optional leading METHOD ("GET /v1/health" -> "/v1/health").
			if i := strings.LastIndex(pattern, " "); i >= 0 {
				pattern = pattern[i+1:]
			}
			// "/" is the catch-all every mux ends with: it is the 404 fallback, not
			// a served surface.
			if pattern == "/" || strings.HasPrefix(pattern, "/v1/") {
				continue
			}
			offenders = append(offenders, name+": "+pattern)
		}
	}

	// Anti-vacuous: a refactor that moves or renames these registrations must fail
	// here rather than silently reporting an empty, clean surface.
	assert.GreaterOrEqual(t, registrations, 20,
		"found only %d mux registrations: the scan is blind, so this check is not reading the surface it claims to. Fix the pattern, do not lower this floor.", registrations)

	assert.Emptyf(t, offenders,
		"these mux patterns are not under /v1/, so the TCP listener's shell shadows them before the gate "+
			"and they are unreachable there (they answer index.html with a 200, not a 404):\n  %s\n\n"+
			"Register the route under /v1/, or — if it genuinely belongs outside the API namespace — the "+
			"shells in webserve.go have to learn about it first.", strings.Join(offenders, "\n  "))
}
