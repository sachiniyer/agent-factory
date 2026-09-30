package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readWrapperMethods are the only apiclient methods a withDaemonHTTP closure may
// call. withDaemonHTTP re-sends a request whose reply was lost, which is safe
// only for a read; a mutation must use withDaemonHTTPMutation (#4820).
var readWrapperMethods = map[string]bool{
	"SnapshotWithAlarms": true,
	"PreviewSnapshot":    true,
	"ListAccounts":       true,
	"ListBackends":       true,
}

// TestReadWrapperCallsOnlyReads parses the package's production source and
// fails on any withDaemonHTTP closure that calls a client method outside
// readWrapperMethods. It exists because #4820's fix nearly shipped with a
// replaying mutation: ReorderTab landed on master through withDaemonHTTP while
// the fix was in review, and the hand-maintained seam list in
// TestMutationCommittedThenReplyLost_SentExactlyOnce could not know about it.
func TestReadWrapperCallsOnlyReads(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	wrapperCalls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "withDaemonHTTP" {
				return true
			}
			wrapperCalls++
			if len(call.Args) != 1 {
				offenders = append(offenders, fset.Position(call.Pos()).String()+": withDaemonHTTP with an argument that is not a closure")
				return true
			}
			lit, ok := call.Args[0].(*ast.FuncLit)
			if !ok || len(lit.Type.Params.List) != 1 || len(lit.Type.Params.List[0].Names) != 1 {
				offenders = append(offenders, fset.Position(call.Pos()).String()+": withDaemonHTTP argument must be a func(c *apiclient.Client) literal the guard can read")
				return true
			}
			client := lit.Type.Params.List[0].Names[0].Name
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == client && !readWrapperMethods[sel.Sel.Name] {
					offenders = append(offenders, fset.Position(sel.Pos()).String()+": "+client+"."+sel.Sel.Name+
						" is not a read; route it through withDaemonHTTPMutation")
				}
				return true
			})
			return true
		})
	}
	if wrapperCalls == 0 {
		t.Fatal("found no withDaemonHTTP calls; the guard is no longer reading the code it protects")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Error(o)
	}
}
