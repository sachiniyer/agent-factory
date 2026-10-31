package git

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestGoroutinesReadNoTestSwappedPackageState pins the property behind #6226:
// no goroutine a bounded operation may abandon reads mutable package state.
// A worker abandoned at its deadline keeps running after the operation returns,
// so a test's cleanup restoring a seam races whatever the worker still reads
// (the race detector caught relocationIdentityTimeout this way). The seam must
// be read at spawn and passed in by value, or be atomic.
//
// The scan is lexical and conservative: any package-level variable that some
// test file assigns must not be named inside the body of a `go` statement in
// non-test code. It looks only at the goroutine's own body — a seam read by a
// function the worker calls is reached through that function's own spawn site
// or, for the shared timeout, through the atomic.
func TestGoroutinesReadNoTestSwappedPackageState(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["git"]
	if pkg == nil {
		t.Fatal("package git not found")
	}
	vars := map[string]bool{}
	assigned := map[string]bool{}
	type spawn struct {
		pos  string
		call ast.Node
	}
	var spawns []spawn
	for name, file := range pkg.Files {
		isTest := strings.HasSuffix(name, "_test.go")
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR || isTest {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					vars[id.Name] = true
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if isTest {
					for _, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							assigned[id.Name] = true
						}
					}
				}
			case *ast.GoStmt:
				// Arguments of `go f(x)` are evaluated at spawn, which is the
				// safe shape; only a function literal's body runs later.
				if literal, ok := n.Call.Fun.(*ast.FuncLit); ok && !isTest {
					spawns = append(spawns, spawn{fset.Position(n.Pos()).String(), literal.Body})
				}
			}
			return true
		})
	}
	var offenders []string
	for _, s := range spawns {
		seen := map[string]bool{}
		ast.Inspect(s.call, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name != "_" && vars[id.Name] && assigned[id.Name] {
				seen[id.Name] = true
			}
			return true
		})
		for name := range seen {
			offenders = append(offenders, s.pos+": "+name)
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("goroutines read package variables that tests swap — read them before `go` and pass them in:\n- %s",
			strings.Join(offenders, "\n- "))
	}
}
