package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

const relocationIdentityTestTimeoutScale = 20

// useRelocationIdentityTimeoutForTest preserves a real deadline while giving
// filesystem and scheduler work enough headroom on a contended runner. The
// compressed values still choose the relative test budget; scaling is capped at
// the production value that was active on entry, so tests never weaken the
// shipped two-second bound. Cleanup is registered here to keep restoration
// independent from a later cleanup that calls t.Fatal (#4160).
func useRelocationIdentityTimeoutForTest(t *testing.T, compressed time.Duration) {
	t.Helper()
	previous := relocationIdentityTimeout
	relocationIdentityTimeout = scaledRelocationIdentityTestTimeout(compressed, previous)
	t.Cleanup(func() { relocationIdentityTimeout = previous })
}

func scaledRelocationIdentityTestTimeout(compressed, production time.Duration) time.Duration {
	if compressed <= 0 || production <= 0 || compressed > production/relocationIdentityTestTimeoutScale {
		return production
	}
	return compressed * relocationIdentityTestTimeoutScale
}

func TestScaledRelocationIdentityTestTimeout(t *testing.T) {
	const production = 2 * time.Second
	for _, test := range []struct {
		compressed time.Duration
		want       time.Duration
	}{
		{compressed: 25 * time.Millisecond, want: 500 * time.Millisecond},
		{compressed: 50 * time.Millisecond, want: time.Second},
		{compressed: 80 * time.Millisecond, want: 1600 * time.Millisecond},
		{compressed: 100 * time.Millisecond, want: production},
		{compressed: time.Second, want: production},
	} {
		if got := scaledRelocationIdentityTestTimeout(test.compressed, production); got != test.want {
			t.Errorf("scaledRelocationIdentityTestTimeout(%s, %s) = %s, want %s",
				test.compressed, production, got, test.want)
		}
	}
}

// TestRelocationIdentityTimeoutUsesSharedTestPolicy keeps every compressed
// test deadline behind one policy. A new direct assignment would otherwise
// silently restore the 25-100ms wall-clock race that made unrelated CI runs
// fail across both darwin and linux (#4155).
func TestRelocationIdentityTimeoutUsesSharedTestPolicy(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || function.Name.Name == "useRelocationIdentityTimeoutForTest" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, target := range assignment.Lhs {
					identifier, ok := target.(*ast.Ident)
					if ok && identifier.Name == "relocationIdentityTimeout" {
						files[name] = true
					}
				}
				return true
			})
		}
	}
	if len(files) == 0 {
		return
	}
	direct := make([]string, 0, len(files))
	for name := range files {
		direct = append(direct, name)
	}
	sort.Strings(direct)
	t.Fatalf("%d test files bypass the shared relocation timeout policy:\n- %s",
		len(direct), strings.Join(direct, "\n- "))
}
