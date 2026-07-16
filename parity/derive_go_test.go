package parity

// Field-level derivation for the Go surfaces.
//
// The verb-level checks catch a surface gaining a COMMAND. They do not catch a
// surface that calls the right verb while silently declining half its request —
// which is where real parity gaps live: `af sessions preview` reaches the
// Preview RPC but never sets Tab/TabID/Full, so the CLI can only ever see tab 0
// (#1948), and the TUI reaches CreateSession but never sets Backend (#1933).
// Same shape, different route.
//
// Both Go surfaces build their requests as `daemon.XxxRequest{...}` composite
// literals, so which fields a surface can populate is a fact in the AST. This
// reads it out rather than trusting a hand-kept list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// goSurfaces maps a surface to the package directory that builds its daemon
// requests. api/ is the CLI's cobra commands; app/ is the TUI.
var goSurfaces = map[string]string{
	"cli": "api",
	"tui": "app",
}

// auditedRequests binds a daemon request type name to its reflect.Type so field
// names can be mapped to their JSON wire names. The AST discovers which types a
// surface actually constructs; any type it finds that is missing here fails the
// check, so this registry cannot silently fall behind the code.
var auditedRequests = map[string]reflect.Type{
	"ArchiveSessionRequest":   reflect.TypeOf(daemon.ArchiveSessionRequest{}),
	"CloseTabRequest":         reflect.TypeOf(daemon.CloseTabRequest{}),
	"CreateSessionRequest":    reflect.TypeOf(daemon.CreateSessionRequest{}),
	"CreateTabRequest":        reflect.TypeOf(daemon.CreateTabRequest{}),
	"DeleteProjectRequest":    reflect.TypeOf(daemon.DeleteProjectRequest{}),
	"DeliverPromptRequest":    reflect.TypeOf(daemon.DeliverPromptRequest{}),
	"KillSessionRequest":      reflect.TypeOf(daemon.KillSessionRequest{}),
	"PauseStatusPollRequest":  reflect.TypeOf(daemon.PauseStatusPollRequest{}),
	"PreviewRequest":          reflect.TypeOf(daemon.PreviewRequest{}),
	"RestoreSessionRequest":   reflect.TypeOf(daemon.RestoreSessionRequest{}),
	"ResumeFromLimitRequest":  reflect.TypeOf(daemon.ResumeFromLimitRequest{}),
	"ResumeStatusPollRequest": reflect.TypeOf(daemon.ResumeStatusPollRequest{}),
	"SendPromptRequest":       reflect.TypeOf(daemon.SendPromptRequest{}),
	"SetPRInfoRequest":        reflect.TypeOf(daemon.SetPRInfoRequest{}),
	"SnapshotRequest":         reflect.TypeOf(daemon.SnapshotRequest{}),
}

// minGoLiterals guards against a vacuous pass: if the surfaces are refactored to
// build requests some other way (a builder, field-by-field assignment) the AST
// walk would find nothing and every field would look reachable. Trip loudly
// instead.
const minGoLiterals = 20

// jsonFieldNames returns a struct's JSON wire field names keyed by Go field
// name, mirroring the daemon's own jsonFields semantics (skip unexported, skip
// json:"-", fall back to the Go name when untagged).
func jsonFieldNames(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported: gob and encoding/json both skip it
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[f.Name] = name
	}
	return out
}

// requestUse is what one surface can populate on one daemon request type: the
// union of the JSON fields set across every construction site, plus where those
// sites are.
type requestUse struct {
	Fields map[string]bool // json field names this surface can set
	Sites  []string        // file:line of each construction site
}

// deriveGoRequestUse walks a surface's package for `daemon.XxxRequest{...}`
// composite literals and reports, per request type, which JSON fields it sets.
//
// Limitation, stated plainly: this sees composite literals with named fields.
// A surface that built a request by field-by-field assignment would read as
// setting nothing. Every current site is a named composite literal (verified),
// and minGoLiterals trips if that stops being true.
func deriveGoRequestUse(t *testing.T, surface string) map[string]*requestUse {
	t.Helper()
	dir := filepath.Join(repoRoot(t), goSurfaces[surface])
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	out := map[string]*requestUse{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok || pkgIdent.Name != "daemon" || !strings.HasSuffix(sel.Sel.Name, "Request") {
					return true
				}

				typeName := sel.Sel.Name
				rt, known := auditedRequests[typeName]
				if !known {
					t.Errorf("%s constructs daemon.%s at %s but it is not in auditedRequests "+
						"(parity/derive_go_test.go) — add it so its fields are audited.",
						surface, typeName, fset.Position(lit.Pos()))
					return true
				}
				byGoName := jsonFieldNames(rt)

				use := out[typeName]
				if use == nil {
					use = &requestUse{Fields: map[string]bool{}}
					out[typeName] = use
				}
				use.Sites = append(use.Sites, relSite(t, fset.Position(lit.Pos()).String()))
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					if jsonName, ok := byGoName[key.Name]; ok {
						use.Fields[jsonName] = true
					}
				}
				return true
			})
		}
	}
	return out
}

// relSite trims an absolute position down to a repo-relative file:line.
func relSite(t *testing.T, pos string) string {
	t.Helper()
	root := repoRoot(t) + string(filepath.Separator)
	return strings.TrimPrefix(pos, root)
}

// unreachedFields returns the JSON fields of a request type that a surface never
// sets, sorted.
func unreachedFields(rt reflect.Type, use *requestUse) []string {
	var out []string
	for _, jsonName := range jsonFieldNames(rt) {
		if use == nil || !use.Fields[jsonName] {
			out = append(out, jsonName)
		}
	}
	sort.Strings(out)
	return out
}
