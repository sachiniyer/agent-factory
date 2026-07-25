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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// goSurfaces maps a surface to the sources that build its daemon requests.
//
// api/ is the CLI's cobra commands and app/ is the TUI, but neither is the whole
// story: both surfaces also reach the daemon through WRAPPER helpers that build
// the request for them, and a wrapper that never sets a field makes it
// unreachable just as surely as the surface omitting it. The task paths are
// exactly this — daemon/control_client.go:304,313 builds AddTaskRequest and
// UpdateTaskRequest for the CLI, apiclient/control.go:118,127 does the same for
// the TUI — so scanning only api/ and app/ would let those request types grow a
// field with no parity decision at all.
//
// The wrappers are listed under the surface whose calls they serve; a file may
// appear under both when both surfaces route through it.
var goSurfaces = map[string][]string{
	// daemon/control_client.go is the CLI's gob control-socket path.
	"cli": {"api", "daemon/control_client.go"},
	// apiclient/ is the TUI's HTTP path to the daemon.
	"tui": {"app", "apiclient"},
}

// auditedRequests binds a daemon request type name to its reflect.Type so field
// names can be mapped to their JSON wire names. The AST discovers which types a
// surface actually constructs; any type it finds that is missing here fails the
// check, so this registry cannot silently fall behind the code.
var auditedRequests = map[string]reflect.Type{
	"AddTaskRequest":          reflect.TypeOf(daemon.AddTaskRequest{}),
	"ArchiveSessionRequest":   reflect.TypeOf(daemon.ArchiveSessionRequest{}),
	"CloseTabRequest":         reflect.TypeOf(daemon.CloseTabRequest{}),
	"CreateSessionRequest":    reflect.TypeOf(daemon.CreateSessionRequest{}),
	"CreateTabRequest":        reflect.TypeOf(daemon.CreateTabRequest{}),
	"DeleteProjectRequest":    reflect.TypeOf(daemon.DeleteProjectRequest{}),
	"DeliverPromptRequest":    reflect.TypeOf(daemon.DeliverPromptRequest{}),
	"HandoffSessionRequest":   reflect.TypeOf(daemon.HandoffSessionRequest{}),
	"KillSessionRequest":      reflect.TypeOf(daemon.KillSessionRequest{}),
	"ListTasksRequest":        reflect.TypeOf(daemon.ListTasksRequest{}),
	"PauseStatusPollRequest":  reflect.TypeOf(daemon.PauseStatusPollRequest{}),
	"PingRequest":             reflect.TypeOf(daemon.PingRequest{}),
	"PreviewRequest":          reflect.TypeOf(daemon.PreviewRequest{}),
	"ReapConfigAgentRequest":  reflect.TypeOf(daemon.ReapConfigAgentRequest{}),
	"RegisterProjectRequest":  reflect.TypeOf(daemon.RegisterProjectRequest{}),
	"RenameTabRequest":        reflect.TypeOf(daemon.RenameTabRequest{}),
	"ReorderTabRequest":       reflect.TypeOf(daemon.ReorderTabRequest{}),
	"RestoreSessionRequest":   reflect.TypeOf(daemon.RestoreSessionRequest{}),
	"RestartTaskRequest":      reflect.TypeOf(daemon.RestartTaskRequest{}),
	"ResumeFromLimitRequest":  reflect.TypeOf(daemon.ResumeFromLimitRequest{}),
	"ResumeStatusPollRequest": reflect.TypeOf(daemon.ResumeStatusPollRequest{}),
	"SendPromptRequest":       reflect.TypeOf(daemon.SendPromptRequest{}),
	"SetPRInfoRequest":        reflect.TypeOf(daemon.SetPRInfoRequest{}),
	"RemoveTaskRequest":       reflect.TypeOf(daemon.RemoveTaskRequest{}),
	"SnapshotRequest":         reflect.TypeOf(daemon.SnapshotRequest{}),
	"TriggerTaskRequest":      reflect.TypeOf(daemon.TriggerTaskRequest{}),
	"UpdateTaskRequest":       reflect.TypeOf(daemon.UpdateTaskRequest{}),
}

// minGoLiterals guards against a vacuous pass: if the surfaces are refactored to
// build requests some other way (a builder, field-by-field assignment) the AST
// walk would find nothing and every field would look reachable. Trip loudly
// instead.
const minGoLiterals = 24

// maxNestDepth bounds the nested-payload walk. Request payloads are shallow;
// this only stops a self-referential type from looping.
const maxNestDepth = 3

// jsonFieldPaths returns a struct's JSON wire field paths, RECURSING into nested
// struct payloads so a wrapper request cannot hide its options.
//
// This matters because several routes are thin wrappers: UpdateTaskRequest is
// {id, update} where update is a task.TaskUpdate carrying eight real options, and
// AddTaskRequest is {task}. Counting only top-level fields would mark the whole
// payload covered the moment a surface sends the wrapper — which is precisely how
// task.TaskUpdate.ProjectPath stayed invisible: every surface "sends update", but
// only the TUI can populate project_path.
//
// Keys are Go field paths ("Update.ProjectPath"), values the JSON path
// ("update.project_path"). Mirrors the daemon's jsonFields semantics: skip
// unexported, skip json:"-", fall back to the Go name when untagged.
func jsonFieldPaths(t reflect.Type) map[string]string {
	out := map[string]string{}
	var walk func(rt reflect.Type, goPrefix, jsonPrefix string, depth int)
	walk = func(rt reflect.Type, goPrefix, jsonPrefix string, depth int) {
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || depth > maxNestDepth {
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
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
			goPath, jsonPath := goPrefix+f.Name, jsonPrefix+name
			out[goPath] = jsonPath

			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			// time.Time and friends are leaf values despite being structs: they
			// marshal whole, so their internals are not options a surface picks.
			if ft.Kind() == reflect.Struct && !isLeafStruct(ft) {
				walk(ft, goPath+".", jsonPath+".", depth+1)
			}
		}
	}
	walk(t, "", "", 0)
	return out
}

// isLeafStruct reports structs that marshal as a single opaque value, so their
// fields are not per-surface options.
func isLeafStruct(rt reflect.Type) bool {
	switch rt.String() {
	case "time.Time":
		return true
	}
	return false
}

// jsonFieldNames returns only the TOP-LEVEL json names keyed by Go field name —
// what a composite literal can set directly.
func jsonFieldNames(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
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
// union of the JSON field paths set across every construction site, plus where
// those sites are.
type requestUse struct {
	Fields map[string]bool // json field paths this surface provably sets
	Sites  []string        // file:line of each construction site
	// Unanalyzable records sites that MENTION this request type in a shape the
	// walk cannot read — `var r daemon.PreviewRequest` with later field
	// assignment, a builder, a value returned from elsewhere. Before this
	// existed such a site simply vanished: the type dropped out of the derived
	// set, its declarations were never visited, and the audit reported parity
	// over a call it had not read. An analyzer that cannot understand a
	// construct must produce a FINDING, not a shrug.
	Unanalyzable []string
}

// goSurfaceFiles expands a surface's configured dirs/files into concrete
// non-test .go paths.
func goSurfaceFiles(t *testing.T, surface string) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, entry := range goSurfaces[surface] {
		full := filepath.Join(root, entry)
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("stat %s: %v", full, err)
		}
		if !info.IsDir() {
			out = append(out, full)
			continue
		}
		// RECURSE, symmetric with webSourceFiles. A non-recursive read skips any
		// module in a subdirectory of api/app/apiclient — a request-construction
		// moved into api/tasks/… would go unaudited and the audit would report
		// parity over code it never opened. That is the exact under-coverage the
		// web side already fixed in this PR; leaving the Go side flat makes the
		// audit honest on one half and blind on the other.
		err = filepath.WalkDir(full, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			n := d.Name()
			if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", full, err)
		}
	}
	sort.Strings(out)
	return out
}

// requestTypeName returns the audited request type a composite literal builds, or
// "" if it is not one.
//
// Both spellings must be recognised: api/, app/ and apiclient/ write
// `daemon.XxxRequest{...}` (a SelectorExpr), while daemon/control_client.go is
// INSIDE package daemon and writes the bare `XxxRequest{...}` (an Ident). Only
// matching the qualified form would silently skip the CLI's own task wrapper —
// the exact blind spot this fix exists to close.
func requestTypeName(lit *ast.CompositeLit, pkgName string) string {
	switch typ := lit.Type.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)
		if !ok || pkg.Name != "daemon" {
			return ""
		}
		return typ.Sel.Name
	case *ast.Ident:
		// A bare identifier is only a daemon request when the file IS package
		// daemon. Accepting it everywhere would match a surface's own
		// same-suffixed types (app's sessionStartRequest) and report them as
		// unaudited daemon requests.
		if pkgName != "daemon" {
			return ""
		}
		return typ.Name
	}
	return ""
}

// requestTypeExprName renders a type expression as a daemon request type name,
// or "" when it is not one. Mirrors requestTypeName's rules for the qualified
// and in-package spellings.
func requestTypeExprName(e ast.Expr, pkgName string) string {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok || pkg.Name != "daemon" || !strings.HasSuffix(t.Sel.Name, "Request") {
			return ""
		}
		return t.Sel.Name
	case *ast.Ident:
		if pkgName != "daemon" || !strings.HasSuffix(t.Name, "Request") {
			return ""
		}
		return t.Name
	}
	return ""
}

// typeExprName renders a composite-literal or var type as written ("task.Task"),
// so it can be matched against reflect.Type.String().
func typeExprName(e ast.Expr, pkgName string) string {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
	case *ast.Ident:
		return pkgName + "." + t.Name
	}
	return ""
}

// deriveTypeFieldUse reports, per struct type NAME as reflect renders it
// ("task.TaskUpdate"), which JSON fields a surface provably populates anywhere in
// its sources.
//
// This is what makes nested payloads honest. A wrapper like
// UpdateTaskRequest{ID: id, Update: update} proves only that `update` is set —
// the sub-fields are chosen upstream, and the two surfaces do it differently:
// api/tasks.go:172 builds a task.Task composite literal, while
// api/tasks.go:296 declares `var patch task.TaskUpdate` and assigns
// `patch.Name = …` field by field. Both forms are read here, so
// task.TaskUpdate.ProjectPath shows up as genuinely unreachable from the CLI
// rather than being hidden behind a covered wrapper.
func deriveTypeFieldUse(t *testing.T, surface string) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]map[string]bool{}

	record := func(typeName, jsonName string) {
		if out[typeName] == nil {
			out[typeName] = map[string]bool{}
		}
		out[typeName][jsonName] = true
	}

	for _, path := range goSurfaceFiles(t, surface) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		pkgName := file.Name.Name
		// varTypes maps a local variable to the struct type it holds, so
		// `patch.Name = x` can be attributed to task.TaskUpdate.
		varTypes := map[string]string{}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec: // var patch task.TaskUpdate
				if name := typeExprName(node.Type, pkgName); name != "" {
					for _, id := range node.Names {
						varTypes[id.Name] = name
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					// x := task.TaskUpdate{...}
					if id, ok := lhs.(*ast.Ident); ok && i < len(node.Rhs) {
						if lit, ok := node.Rhs[i].(*ast.CompositeLit); ok {
							if name := typeExprName(lit.Type, pkgName); name != "" {
								varTypes[id.Name] = name
							}
						}
					}
					// patch.Name = ...
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						if base, ok := sel.X.(*ast.Ident); ok {
							if typeName, known := varTypes[base.Name]; known {
								record(typeName, sel.Sel.Name)
							}
						}
					}
				}
			case *ast.CompositeLit: // task.Task{Name: ...}
				name := typeExprName(node.Type, pkgName)
				if name == "" {
					return true
				}
				for _, el := range node.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok {
						record(name, key.Name)
					}
				}
			}
			return true
		})
	}
	return out
}

// deriveGoRequestUse walks a surface's sources for daemon request composite
// literals and reports, per request type, which JSON field paths it provably
// sets.
//
// Limitations, stated plainly rather than left for someone to discover:
//   - Only composite literals with named fields are seen. A request built by
//     field-by-field assignment would read as setting nothing; minGoLiterals
//     trips if the surfaces ever move to that style.
//   - A nested payload assigned from a VARIABLE (`Update: update`) proves only
//     that the wrapper field is set, never which of its sub-fields the surface
//     can populate — that depends on flags and forms upstream. Those sub-paths
//     are therefore reported as unreached and must be declared, which is the
//     forcing function: a new field inside task.TaskUpdate makes every surface
//     answer for it.
func deriveGoRequestUse(t *testing.T, surface string) map[string]*requestUse {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*requestUse{}

	for _, path := range goSurfaceFiles(t, surface) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		pkgName := file.Name.Name

		// Fail closed on a request BUILT in a shape the composite-literal walk
		// cannot read: `var r daemon.PreviewRequest` with the fields assigned
		// afterwards. Those are analyzed where possible (below) and reported as
		// findings where not — never dropped, which is what used to happen.
		//
		// A function PARAMETER or RESULT of a request type is deliberately NOT a
		// construction: the caller builds the value, and that caller is walked
		// on its own. Counting signatures would flag every wrapper in
		// control_client.go and drown the real findings in noise.
		varRequests := map[string]string{} // var name -> request type
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Values) > 0 {
				return true // `var x = expr` is an initialiser, handled below
			}
			typeName := requestTypeExprName(spec.Type, pkgName)
			if typeName == "" {
				return true
			}
			if _, known := auditedRequests[typeName]; !known {
				return true
			}
			for _, id := range spec.Names {
				varRequests[id.Name] = typeName
			}
			use := out[typeName]
			if use == nil {
				use = &requestUse{Fields: map[string]bool{}}
				out[typeName] = use
			}
			use.Sites = append(use.Sites, relSite(t, fset.Position(spec.Pos()).String()))
			return true
		})

		// Fold in `r.Field = …` assignments against those vars, so a
		// field-by-field build is analyzed rather than merely flagged.
		if len(varRequests) > 0 {
			ast.Inspect(file, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					base, ok := sel.X.(*ast.Ident)
					if !ok {
						continue
					}
					typeName, known := varRequests[base.Name]
					if !known {
						continue
					}
					if jsonName, ok := jsonFieldNames(auditedRequests[typeName])[sel.Sel.Name]; ok {
						out[typeName].Fields[jsonName] = true
					}
				}
				return true
			})
			// A var declared and never assigned is a build this walk cannot
			// read — report it rather than crediting an all-zero request.
			for name, typeName := range varRequests {
				if len(out[typeName].Fields) == 0 {
					out[typeName].Unanalyzable = append(out[typeName].Unanalyzable,
						relSite(t, fset.Position(file.Pos()).String())+" (var "+name+")")
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typeName := requestTypeName(lit, pkgName)
			if typeName == "" || !strings.HasSuffix(typeName, "Request") {
				return true
			}
			rt, known := auditedRequests[typeName]
			if !known {
				t.Errorf("%s constructs %s at %s but it is not in auditedRequests "+
					"(parity/derive_go_test.go) — add it so its fields are audited.",
					surface, typeName, relSite(t, fset.Position(lit.Pos()).String()))
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
				jsonName, ok := byGoName[key.Name]
				if !ok {
					continue
				}
				use.Fields[jsonName] = true
				// A nested payload written as an inline literal DOES prove which
				// sub-fields the surface sets; one assigned from a variable does
				// not, and its sub-paths stay unreached.
				if nested, ok := kv.Value.(*ast.CompositeLit); ok {
					collectNested(nested, rt, key.Name, jsonName, use)
				}
			}
			return true
		})
	}
	return out
}

// collectNested records the sub-paths an inline nested literal sets.
func collectNested(lit *ast.CompositeLit, parent reflect.Type, goField, jsonPrefix string, use *requestUse) {
	f, ok := parent.FieldByName(goField)
	if !ok {
		return
	}
	ft := f.Type
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	if ft.Kind() != reflect.Struct {
		return
	}
	byGoName := jsonFieldNames(ft)
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
			use.Fields[jsonPrefix+"."+jsonName] = true
		}
	}
}

// relSite trims an absolute position down to a repo-relative file:line.
func relSite(t *testing.T, pos string) string {
	t.Helper()
	root := repoRoot(t) + string(filepath.Separator)
	return strings.TrimPrefix(pos, root)
}

// unreachedFields returns the JSON fields of a request type that a surface never
// sets, sorted.
// unreachedFields returns the JSON field paths of a request type that a surface
// never populates.
//
// A TOP-LEVEL path is reached when a construction site sets it. A NESTED path is
// reached when the surface populates that field on the nested type ANYWHERE —
// the wrapper hands the payload through, so what matters is whether the surface
// can build one carrying that field (typeUse), not whether the wrapper literal
// mentioned it.
func unreachedFields(rt reflect.Type, use *requestUse, typeUse map[string]map[string]bool) []string {
	byGoPath := jsonFieldPaths(rt)
	var out []string
	for goPath, jsonPath := range byGoPath {
		if !strings.Contains(goPath, ".") {
			if use == nil || !use.Fields[jsonPath] {
				out = append(out, jsonPath)
			}
			continue
		}
		if nestedTypeReaches(rt, goPath, typeUse) {
			continue
		}
		if use != nil && use.Fields[jsonPath] {
			continue // an inline nested literal set it directly
		}
		out = append(out, jsonPath)
	}
	sort.Strings(out)
	return out
}

// nestedTypeReaches reports whether the surface populates the Go field named by
// a dotted path on its owning nested type.
func nestedTypeReaches(root reflect.Type, goPath string, typeUse map[string]map[string]bool) bool {
	parts := strings.Split(goPath, ".")
	rt := root
	for i := 0; i < len(parts)-1; i++ {
		f, ok := rt.FieldByName(parts[i])
		if !ok {
			return false
		}
		rt = f.Type
		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return false
		}
	}
	return typeUse[rt.String()][parts[len(parts)-1]]
}
