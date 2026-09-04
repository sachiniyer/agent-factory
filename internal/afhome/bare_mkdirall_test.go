package afhome_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// The property, enforced rather than hand-listed.
//
// #3845 converted the state-write sites to config.MkdirAllUnderAFHome by
// enumerating them. #3850 is what that hand-list missed: an entire second family
// of creates on the SESSION-LAUNCH path — the claude plugin dir, the agent-skill
// dirs, the tmux server log, the worktree root under worktree_root=subdirectory,
// the per-agent account dirs — every one of them running BEFORE the first write
// a latch could refuse, so the home was already back by the time a guarded write
// was reached. This is the #3787 lesson: a list enumerated by the names known
// today is a lower bound on the sites that have the property.
//
// So the list below is inverted. Every os.MkdirAll in the module's non-test code
// is a violation unless it appears here with a reason its argument can never be
// at or inside the AF home. A new call site is a test failure by default, and the
// author either routes it through the guard or writes down why it does not need
// to be.
//
// Scoped to MkdirAll on purpose. os.MkdirAll creates every missing ANCESTOR, so
// any home-relative path re-creates the whole home; os.Mkdir creates one level
// and fails with ENOENT when its parent is gone, so it can only resurrect a home
// it is handed directly, and no site in this tree passes one (audited for #3850:
// upgradetxn's two are single-level by construction — createDurableDirectory
// asserts filepath.Dir(path) == parent — agentaccount's is a child of
// accounts/<agent>, and config/inrepo's is inside a repo).

// allowedBareMkdirAll lists the sites whose argument is proven never to be at or
// inside the AF home, keyed by "<repo-relative file>:<enclosing declaration>".
//
// sites is the number of os.MkdirAll calls that declaration is allowed to hold.
// It ratchets: adding one to an already-listed function fails just like adding a
// new one elsewhere, and removing one makes the entry stale and also fails. A
// list that silently absorbs its neighbours is the hand-list problem again.
var allowedBareMkdirAll = map[string]struct {
	sites  int
	reason string
}{
	"config/atomicwrite.go:ensureStorageParent": {1,
		"The guarded seam itself. requireObservedAFHomePresent is the line directly " +
			"above this MkdirAll, so every atomic write and every file lock in the " +
			"process is already covered (#3845)."},
	"internal/afhome/afhome.go:MkdirAll": {1,
		"The guarded helper. This is the os.MkdirAll every other site routes through."},
	"daemon/singleton_lock.go:acquireHomeLock": {1,
		"The one deliberate pre-arm creator, and creating the home is its job. Its " +
			"only production caller is RunDaemon (daemon/daemon.go:129), which arms " +
			"the latch a few lines later at :143 — so it runs once per daemon, and " +
			"only while there is no latch to refuse it. acquireHomeLockAt, the " +
			"variant that inspects ANOTHER home, deliberately creates nothing."},
	"daemon/autostart.go:InstallAutostart": {2,
		"The systemd unit directory and the launchd LaunchAgents directory. Both are " +
			"under the user's own config/library root, never under the AF home."},
	"commands/docs_gen.go:genDocsCmd": {1,
		"`af gen-docs <output-dir>` writes the committed reference pages to a " +
			"directory named on argv; scripts/gen-docs.sh points it at docs/. A hidden " +
			"dev/CI command, and its argument is never a home-derived path."},
	"commands/plugins_gen.go:writeAgentPlugins": {1,
		"Writes generated plugin files into an af SOURCE CHECKOUT selected by " +
			"--plugin-root, which the caller has already validated by reading its " +
			"go.mod module path. Never the AF home."},
}

func TestNoBareMkdirAllUnderTheAFHome(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	found := map[string][]token.Position{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		// Test files are excluded: a test's MkdirAll builds its own fixture home
		// in a t.TempDir, which is the one place creating a home IS the point.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Parsed with no build constraints applied, so a darwin- or windows-only
		// file is covered on a linux CI runner too.
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		if !importsStdlibOS(file) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		for _, call := range bareMkdirAllCalls(file) {
			site := rel + ":" + enclosingDecl(file, call.Pos())
			found[site] = append(found[site], fset.Position(call.Pos()))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	for _, site := range sortedKeys(found) {
		positions := found[site]
		allowed, ok := allowedBareMkdirAll[site]
		if !ok {
			file := site[:strings.LastIndex(site, ":")]
			for _, pos := range positions {
				t.Errorf("%s:%d: bare os.MkdirAll.\n"+
					"os.MkdirAll creates every missing ancestor, so a home-relative path re-creates the whole "+
					"AF home — and a daemon that re-creates its own deleted home never observes the deletion "+
					"and keeps firing schedules forever (#1093/#3845/#3850).\n"+
					"Use config.MkdirAllUnderAFHome (or internal/afhome.MkdirAll in a package config imports, "+
					"such as log or session/tmux). It is an exact os.MkdirAll except in an abandoned daemon.\n"+
					"If this path genuinely can never be at or inside the AF home, add %q to "+
					"allowedBareMkdirAll in %s with the reason.",
					file, pos.Line, site, testFileName())
			}
			continue
		}
		if len(positions) != allowed.sites {
			lines := make([]int, 0, len(positions))
			for _, pos := range positions {
				lines = append(lines, pos.Line)
			}
			t.Errorf("%s: allowlisted for %d bare os.MkdirAll call(s) but found %d (lines %v).\n"+
				"Allowlist reason on file: %s\n"+
				"A new call in an already-allowlisted function is NOT covered by that reason — route it "+
				"through config.MkdirAllUnderAFHome, or split the entry. A dropped call means the entry is "+
				"stale and must be removed.",
				site, allowed.sites, len(positions), lines, allowed.reason)
		}
	}

	for _, site := range sortedKeys(allowedBareMkdirAll) {
		if _, ok := found[site]; !ok {
			t.Errorf("%s: allowlisted in allowedBareMkdirAll but no bare os.MkdirAll is there any more. "+
				"Remove the entry — a stale allowlist silently covers whatever moves in next.", site)
		}
	}
}

// importsStdlibOS reports whether file imports "os" under its own name, which is
// what makes an `os.MkdirAll` selector the standard library's. A file that
// aliases something else to `os` would be flagged and can say so in the
// allowlist; no file in this tree does.
func importsStdlibOS(file *ast.File) bool {
	for _, spec := range file.Imports {
		if spec.Path.Value == `"os"` && (spec.Name == nil || spec.Name.Name == "os") {
			return true
		}
	}
	return false
}

func bareMkdirAllCalls(file *ast.File) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MkdirAll" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// enclosingDecl names the top-level declaration holding pos: a function or
// method (receiver-qualified), or the variable a function literal was assigned
// to. Keying on the declaration rather than on a line number keeps the allowlist
// stable when unrelated code above it moves.
func enclosingDecl(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		if pos < decl.Pos() || pos > decl.End() {
			continue
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				return receiverTypeName(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			return d.Name.Name
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || pos < vs.Pos() || pos > vs.End() || len(vs.Names) == 0 {
					continue
				}
				return vs.Names[0].Name
			}
		}
	}
	return "<file scope>"
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// moduleRoot walks up from this test's own source file to the directory holding
// go.mod, so the walk covers the whole module however the test is invoked.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

func testFileName() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "internal/afhome/bare_mkdirall_test.go"
	}
	return filepath.Base(file)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
