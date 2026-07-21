package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backendReadersUnderLock is the allowlist of functions permitted to touch the
// Instance.backend field directly. Every entry either holds i.mu across the read
// or is documented below as running before the instance is reachable by another
// goroutine.
//
//   - currentBackend / capabilitiesLocked — the synchronized accessors themselves.
//   - SetBackend / bindProvisionResult — the writers; both take i.mu.Lock.
//   - AgentServer / reprovisionRemote / toInstanceDataLocked — read inside an
//     i.mu section their callers established.
//   - PreviewTabSnapshotByID — snapshots the backend while its stable tab target
//     is selected under i.mu, then performs the potentially blocking capture
//     after releasing the lock.
//   - FromInstanceData — a constructor. It populates a local *Instance that no
//     other goroutine can observe yet, so there is nothing to synchronize with.
var backendReadersUnderLock = map[string]bool{
	"currentBackend":         true,
	"capabilitiesLocked":     true,
	"SetBackend":             true,
	"bindProvisionResult":    true,
	"AgentServer":            true,
	"reprovisionRemote":      true,
	"toInstanceDataLocked":   true,
	"PreviewTabSnapshotByID": true,
	"FromInstanceData":       true,
}

// TestBackendFieldIsOnlyReadUnderLock is a source-level guard for #2096/#2165.
//
// That fix routed every delegating method (Start, Recover, Respawn, …) through
// currentBackend() so the pointer read is synchronized against a restore
// rebinding it. Nothing enforced the rule afterwards: the constraint lived in a
// doc comment, and the accompanying race tests only exercise Capabilities() and
// LifecycleView(). A new delegating method that reads i.backend bare therefore
// compiles, passes -race (the racing write only happens during a concurrent
// re-provision, which no unit test drives for it), and reads as correct.
//
// That is exactly how it regressed: rebasing the #2013 handoff work onto the
// #2165 fix merged cleanly — the two touched different lines — and reintroduced
// a bare read in Instance.SwapAgent. Only a rule that scans the source can catch
// a method that was written before the constraint existed.
//
// The check is deliberately syntactic and package-scoped: it asks "who names
// this field", which is the whole question, without needing type resolution.
func TestBackendFieldIsOnlyReadUnderLock(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, sources, "no sources found — is the test running outside the package dir?")

	fset := token.NewFileSet()
	seen := 0
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, src, nil, 0)
		require.NoError(t, err, "parse %s", src)

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "backend" {
					return true
				}
				if _, ok := sel.X.(*ast.Ident); !ok {
					return true
				}
				seen++
				assert.True(t, backendReadersUnderLock[fn.Name.Name],
					"%s reads the Instance.backend field directly, but only the "+
						"synchronized accessors may (#2096). Call i.currentBackend() "+
						"instead, or — if this genuinely runs under i.mu — add it to "+
						"backendReadersUnderLock with the reason.",
					fset.Position(sel.Pos()))
				return true
			})
		}
	}

	// Guard the guard: if the field is ever renamed, the scan above silently
	// matches nothing and this test passes vacuously forever.
	assert.NotZero(t, seen, "found no reads of Instance.backend at all — the field "+
		"was probably renamed, which makes this test vacuous")
}
