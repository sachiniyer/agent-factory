package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

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
