package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every path served BEHIND THE /v1 SHELL must live under /v1/ (#2928).
//
// The daemon's web listener composes shell(gate(mux)), and webShellHandler /
// noWebShellHandler route by PREFIX: `/v1/…` reaches the gated mux, everything
// else goes to the static SPA or a 404. A route registered outside /v1/ never
// reaches the mux there — it is shadowed before the gate is consulted.
//
// It fails SAFE (a dead route, never an exposed one), which is exactly why it
// recurs: the symptom is confusing rather than alarming. `GET /metrics` or
// `/healthz` are natural names, they work over the unix socket so local testing
// passes, and over the web listener they silently return index.html with a 200 —
// serveSPA's fallback — rather than a 404 that would name the mistake.
//
// httproutes_test.go asserts this for the public catalog only. This covers the
// HAND-REGISTERED routes, which are the ones a person adds by hand.
//
// AST-based rather than a regex over `mux.` (#2934 review): a registration made
// through a helper parameter or a renamed local — `registerRoutes(m)` calling
// `m.HandleFunc` — is invisible to a scan keyed on one variable name, while the
// anti-vacuity floor stays satisfied by the registrations it does see. That is a
// guard reporting clean over code it never read, which is the failure this
// exists to prevent.

// muxesNotBehindTheV1Shell are mux builders whose listener does NOT route by the
// /v1/ prefix, so the shadowing invariant does not apply to them. Each entry
// states why, because an exemption without a reason is indistinguishable from an
// oversight.
var muxesNotBehindTheV1Shell = map[string]string{
	"newPreviewMux": "the preview listener is wrapped by previewShellHandler, which filters by HOST " +
		"(previewHostIsProbe/previewHostLabel) and then passes the request straight through — it never " +
		"consults the path. The whole path space belongs to the previewed tab by design (#1856), so a " +
		"preview asset or probe path outside /v1/ is correct there, not a bug (#2934 review).",
}

// registration is one `X.HandleFunc(pattern, …)` or `X.Handle(pattern, …)` call.
type registration struct {
	file     string
	function string
	// pattern is the unquoted string literal, or "" when the argument was not a
	// literal — in which case expr carries its source-level text.
	pattern string
	expr    string
	// servedLoop reports whether the enclosing function ranges over
	// servedHTTPRoutes(), which is what licenses an `rt.Path` argument.
	servedLoop bool
}

func collectMuxRegistrations(t *testing.T) []registration {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var out []registration
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoErrorf(t, err, "parse %s: a file this scan cannot read is a path it cannot check", name)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			served := rangesOverServedRoutes(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") || len(call.Args) == 0 {
					return true
				}
				reg := registration{file: name, function: fn.Name.Name, servedLoop: served}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if unquoted, err := strconv.Unquote(lit.Value); err == nil {
						reg.pattern = unquoted
					}
				}
				if reg.pattern == "" {
					reg.expr = exprText(call.Args[0])
				}
				out = append(out, reg)
				return true
			})
		}
	}
	return out
}

// rangesOverServedRoutes reports whether fn contains `for … := range
// servedHTTPRoutes()`. That loop is what makes an `rt.Path` argument safe, and
// scoping the exemption to it stops a DIFFERENT table reusing the same idiom
// from inheriting the exemption (#2934 review).
func rangesOverServedRoutes(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if call, ok := rng.X.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "servedHTTPRoutes" {
				found = true
			}
		}
		return true
	})
	return found
}

// exprText renders a non-literal argument well enough to name it in a failure.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.BinaryExpr:
		return exprText(v.X) + " + …"
	case *ast.BasicLit:
		return v.Value
	default:
		return "<unrecognized expression>"
	}
}

func TestEveryMuxPatternIsUnderV1(t *testing.T) {
	// Make good on the allowlist's other half rather than citing a test that does
	// not cover it: newHTTPMux registers rt.Path for every servedHTTPRoutes()
	// entry — the public catalog PLUS internalHTTPRoutes — while
	// TestHTTPRoutes_HealthShape iterates only HTTPRoutes().
	served := servedHTTPRoutes()
	require.NotEmpty(t, served)
	for _, rt := range served {
		assert.Truef(t, strings.HasPrefix(rt.Path, "/v1/"),
			"served route %s %s is not under /v1/, so the web listener's shell shadows it before the gate",
			rt.Method, rt.Path)
	}
	require.Equal(t, "/v1/webtab/", webtabPathPrefix,
		"the webtabPathPrefix exemption below asserts this value; if it changed, the invariant changed with it")

	regs := collectMuxRegistrations(t)

	// Anti-vacuous: a refactor that moves or renames these registrations must fail
	// here rather than silently reporting an empty, clean surface.
	assert.GreaterOrEqualf(t, len(regs), 20,
		"found only %d mux registrations: the AST scan is blind, so this check is not reading the surface it "+
			"claims to. Fix the walk, do not lower this floor.", len(regs))

	var offenders []string
	for _, reg := range regs {
		if _, exempt := muxesNotBehindTheV1Shell[reg.function]; exempt {
			continue
		}
		if reg.pattern == "" {
			switch {
			case reg.expr == "rt.Path" && reg.servedLoop:
				// Every servedHTTPRoutes() path, asserted /v1/-prefixed above.
			case strings.HasPrefix(reg.expr, "webtabPathPrefix"):
				// The const, pinned above.
			default:
				offenders = append(offenders, reg.file+" "+reg.function+"(): non-literal pattern "+reg.expr+
					" — this scan cannot resolve it, so it cannot check it")
			}
			continue
		}
		// The bare "/" is the catch-all every mux ends with: the 404 fallback, not a
		// served surface. Checked BEFORE the method is stripped, because `GET /` is a
		// real method-specific root route.
		if reg.pattern == "/" {
			continue
		}
		path := reg.pattern
		if i := strings.LastIndex(path, " "); i >= 0 {
			path = path[i+1:]
		}
		if strings.HasPrefix(path, "/v1/") {
			continue
		}
		offenders = append(offenders, reg.file+" "+reg.function+"(): "+reg.pattern)
	}

	assert.Emptyf(t, offenders,
		"these mux patterns are not under /v1/, so the web listener's shell shadows them before the gate and "+
			"they are unreachable there (they answer index.html with a 200, not a 404):\n  %s\n\n"+
			"Register the route under /v1/, or — if its listener genuinely does not route by that prefix — add "+
			"its mux builder to muxesNotBehindTheV1Shell with the reason.", strings.Join(offenders, "\n  "))
}
