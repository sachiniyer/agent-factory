package parity

// Field-level parity: the OPTION dimension.
//
// The verb-level checks answer "can this surface do X?". They cannot answer "can
// it do X with the options the daemon accepts?", and that is where the gaps
// actually live. Three found so far are the same shape — a field the daemon
// accepts that a surface never sends:
//
//   #1933  the TUI never sets CreateSessionRequest.Backend
//   #1948  the CLI never sets PreviewRequest.Tab/TabID/Full
//
// The earlier version of this check was hand-wired to CreateSession alone, so it
// would not have caught #1948 — the gap was reported by a person dogfooding
// instead. This generalizes it: every field of every audited request must be
// either reachable from a surface or declared, with the field lists derived from
// code (reflection for the wire structs, the AST for the Go surfaces, the call
// sites for the web).

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fieldDecl declares why a surface does not send a field. Exactly one of Gap
// (the capability id tracking it as a real divergence) or OK (why its absence is
// correct) must be set — an undeclared field is a decision nobody made.
type fieldDecl struct {
	Gap string `json:"gap"`
	OK  string `json:"ok"`
}

type fieldCoverage struct {
	// Requests is keyed by daemon request TYPE name, then surface ("cli"/"tui"),
	// then JSON field. Derived from the AST of api/ and app/.
	Requests map[string]map[string]map[string]fieldDecl `json:"requests"`
	// WebRPCs is keyed by RPC method name, then JSON field. Derived from the
	// route catalog's reflected fields vs the web's call sites.
	WebRPCs map[string]map[string]fieldDecl `json:"web_rpcs"`
}

// validate checks one surface's declarations against what it actually reaches.
// Both directions matter: an undeclared unreached field is an unnoticed gap, and
// a declared field the surface now DOES send is a gap that got fixed while the
// table still calls it broken.
func validateFieldDecls(t *testing.T, caps map[string]capability, surface, label string, unreached []string, decls map[string]fieldDecl) {
	t.Helper()

	declared := map[string]bool{}
	for f := range decls {
		declared[f] = true
	}

	for _, f := range unreached {
		d, ok := decls[f]
		if !ok {
			t.Errorf("%s never sets %q, and the inventory does not say why.\n\n"+
				"Add it to field_coverage in parity/inventory.json with either:\n"+
				"  {\"gap\": \"<capability-id>\"}  — a real divergence, tracked by that capability\n"+
				"  {\"ok\": \"<reason>\"}          — its absence is correct, and here is why\n\n"+
				"This is the option dimension: the verb is present but the option is not. "+
				"See docs/surface-parity.md.", label, f)
			continue
		}
		switch {
		case d.Gap != "" && d.OK != "":
			t.Errorf("%s field %q declares both gap and ok — pick one", label, f)
		case d.Gap == "" && d.OK == "":
			t.Errorf("%s field %q declares neither gap nor ok", label, f)
		case d.Gap != "":
			c, ok := caps[d.Gap]
			if !ok {
				t.Errorf("%s field %q names unknown capability %q", label, f, d.Gap)
				break
			}
			// The declaration says this surface cannot send the field, so the
			// capability must still agree that the surface lacks it. Otherwise
			// flipping a row to yes/parity while the field stays unsent would
			// leave the inventory quietly contradicting itself.
			if st := surfaceStatus(c, surface); st != "no" && st != "partial" {
				t.Errorf("%s declares %q a gap against capability %q, but that capability's "+
					"%s status is %q. Either the surface really does reach the field (then it "+
					"is not a gap) or the row is stale.", label, f, d.Gap, surface, st)
			}
		}
	}

	unreachedSet := map[string]bool{}
	for _, f := range unreached {
		unreachedSet[f] = true
	}
	for f := range declared {
		if !unreachedSet[f] {
			t.Errorf("%s: the inventory declares %q unreachable, but the surface now sends it. "+
				"A gap was fixed — remove the declaration and update the capability's verdict.", label, f)
		}
	}
}

// TestGoFieldCoverage audits the CLI and TUI: for every daemon request they
// construct, every field they never set must be declared.
func TestGoFieldCoverage(t *testing.T) {
	inv := loadInventory(t)
	caps := inv.byID()

	total := 0
	for _, surface := range []string{"cli", "tui"} {
		use := deriveGoRequestUse(t, surface)
		typeUse := deriveTypeFieldUse(t, surface)
		for typeName, u := range use {
			total += len(u.Sites)
			rt := auditedRequests[typeName]

			// Fail closed. A construction the walk could not read means every
			// field of that request is unverified, so the coverage below is
			// fiction. Report it as a finding instead of quietly grading the
			// part we happened to understand.
			if len(u.Unanalyzable) > 0 {
				t.Errorf("%s builds %s in a shape this analyzer cannot read: %v\n\n"+
					"Its field coverage is therefore UNVERIFIED — the audit must not report "+
					"parity over a call site it did not understand. Either build the request "+
					"as a composite literal or `var r T; r.Field = …` (both are analyzed), or "+
					"teach deriveGoRequestUse the new shape. Do not ignore this: an "+
					"unanalyzable site used to vanish silently, which is how an audit ends up "+
					"asserting parity over code it never looked at.",
					surface, typeName, u.Unanalyzable)
			}

			unreached := unreachedFields(rt, u, typeUse)
			decls := inv.FieldCoverage.Requests[typeName][surface]
			label := fmt.Sprintf("%s (%s, %v)", typeName, surface, u.Sites)
			validateFieldDecls(t, caps, surface, label, unreached, decls)
		}
	}
	if total < minGoLiterals {
		t.Fatalf("AST found only %d daemon request literals across api/ and app/ (expected >= %d): "+
			"the surfaces now build requests some other way, so this check is blind. Fix "+
			"deriveGoRequestUse rather than lowering minGoLiterals.", total, minGoLiterals)
	}
}

// TestWebFieldCoverage audits the web: for every RPC it calls, every field the
// route accepts but the client never sends must be declared. This is the check
// that pins the reported remote-instance gap (#1933) — CreateSession accepts
// nine fields and the web sends five.
func TestWebFieldCoverage(t *testing.T) {
	inv := loadInventory(t)
	caps := inv.byID()
	routes := deriveRoutes(t)

	for rpc := range deriveWebRPCs(t) {
		r, ok := routes["POST /v1/"+rpc]
		if !ok {
			// Not in the public catalog: nothing to compare against here.
			continue
		}
		sent := map[string]bool{}
		for _, f := range webCallBody(t, rpc) {
			sent[f] = true
		}

		// Prefer the request type's RECURSIVE field paths over the route
		// catalog's list: HTTPRoutes reflects only top-level fields, so a
		// wrapper route like UpdateTask ({id, update}) would look fully covered
		// the moment the web sends `update` — hiding every option inside
		// task.TaskUpdate, which is exactly where the web's missing
		// project_path lives.
		fields := r.Fields
		if rt, audited := auditedRequests[rpc+"Request"]; audited {
			fields = nil
			for _, jsonPath := range jsonFieldPaths(rt) {
				fields = append(fields, jsonPath)
			}
		}

		var unreached []string
		for _, f := range fields {
			if sent[f] {
				continue
			}
			// A nested payload is sent whole (`{ task }`), so the object parse
			// only ever sees the wrapper key. Read the reach from the VALUES the
			// web actually passes — not from the payload's TS interface, which
			// says what is possible and would credit fields no call site sends.
			if base, leaf, nested := strings.Cut(f, "."); nested {
				if reach, known := webNestedReach(t, rt(rpc), base); known {
					if len(reach.Unanalyzable) > 0 {
						t.Errorf("web %s sends its %q payload from a call site this analyzer "+
							"cannot read: %v\n\nIts field coverage is UNVERIFIED — the audit "+
							"must not report parity over a payload it did not understand.",
							rpc, base, reach.Unanalyzable)
					}
					if reach.Fields[leaf] {
						continue
					}
					unreached = append(unreached, f)
					continue
				}
			}
			unreached = append(unreached, f)
		}
		sort.Strings(unreached)
		validateFieldDecls(t, caps, "web", "web "+rpc, unreached, inv.FieldCoverage.WebRPCs[rpc])
	}
}

// rt resolves an RPC name to its audited request type, or nil.
func rt(rpc string) reflect.Type { return auditedRequests[rpc+"Request"] }

// webNestedReach returns what the web can actually put in a nested payload,
// derived from the VALUES it sends rather than the payload's TypeScript type.
//
// The distinction is the whole point. TaskUpdate DECLARES seven options; the one
// call site (web/src/index.ts:862) sends `{ enabled }`. Reading the interface
// credits the web with six options it cannot reach and reports parity over them
// — a field missing from the client passing the audit, which is precisely the
// failure this package exists to prevent.
//
// known=false means the payload has no mapped TS type, so the caller falls back
// to an explicit declaration.
func webNestedReach(t *testing.T, root reflect.Type, goBase string) (webValueReach, bool) {
	t.Helper()
	if root == nil {
		return webValueReach{}, false
	}
	// goBase arrives as a JSON name; find the field that carries it.
	var ft reflect.Type
	for goName, jsonName := range jsonFieldNames(root) {
		if jsonName != goBase {
			continue
		}
		f, _ := root.FieldByName(goName)
		ft = f.Type
	}
	if ft == nil {
		return webValueReach{}, false
	}
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	tsName, ok := webTSTypes[ft.String()]
	if !ok {
		return webValueReach{}, false
	}
	return webNestedValueReach(t, tsName), true
}

// TestFieldCoverageHasNoStaleTypes keeps the declarations honest: a request type
// or web RPC that no surface constructs any more should not linger.
func TestFieldCoverageHasNoStaleTypes(t *testing.T) {
	inv := loadInventory(t)
	for typeName := range inv.FieldCoverage.Requests {
		if _, ok := auditedRequests[typeName]; !ok {
			t.Errorf("field_coverage declares %q but it is not in auditedRequests", typeName)
		}
	}
	web := deriveWebRPCs(t)
	for rpc := range inv.FieldCoverage.WebRPCs {
		if !web[rpc] {
			t.Errorf("field_coverage.web_rpcs declares %q but the web no longer calls it", rpc)
		}
	}
}

// TestFieldCoverageDeclarationsAreLive catches a surface DROPPING a request,
// which every other check is blind to.
//
// TestGoFieldCoverage iterates the DERIVED requests, so it can only notice a
// surface that is missing a declaration. If a surface stops constructing a
// request another surface still uses — the CLI dropping PreviewRequest while the
// TUI keeps it — that type simply vanishes from the derived set for the CLI, its
// declarations are never visited, and they rot in place while the suite stays
// green. The inventory would keep describing a gap on a call site that no longer
// exists.
//
// So the declarations are walked from the OTHER direction: every declared
// (type, surface) must still be a thing that surface really does.
func TestFieldCoverageDeclarationsAreLive(t *testing.T) {
	inv := loadInventory(t)

	// One derivation per surface: the AST walk is the expensive part.
	use := map[string]map[string]*requestUse{}
	for _, surface := range []string{"cli", "tui"} {
		use[surface] = deriveGoRequestUse(t, surface)
	}

	for typeName, bySurface := range inv.FieldCoverage.Requests {
		for surface, decls := range bySurface {
			derived, known := use[surface]
			if !known {
				t.Errorf("field_coverage declares %s for unknown surface %q", typeName, surface)
				continue
			}
			u, constructs := derived[typeName]
			if !constructs {
				t.Errorf("field_coverage declares %d field(s) on %s for the %s surface, but %s "+
					"no longer constructs %s anywhere. Either the surface dropped the call "+
					"(remove the declarations, and re-check the capability's %s cell — it may "+
					"have become a gap) or the derivation lost sight of the call site.",
					len(decls), typeName, surface, surface, typeName, surface)
				continue
			}
			// A declared field that is not a real field of the request is dead
			// too: the wire struct renamed or dropped it and the note stayed.
			paths := map[string]bool{}
			for _, jsonPath := range jsonFieldPaths(auditedRequests[typeName]) {
				paths[jsonPath] = true
			}
			for field := range decls {
				if !paths[field] {
					t.Errorf("field_coverage declares %s.%s for %s, but %s has no such field "+
						"(sites: %v). The wire struct changed and the declaration was left behind.",
						typeName, field, surface, typeName, u.Sites)
				}
			}
		}
	}

	// Same for the web: a declared field must still be a field of the route.
	for rpc, decls := range inv.FieldCoverage.WebRPCs {
		rt, audited := auditedRequests[rpc+"Request"]
		if !audited {
			continue
		}
		paths := map[string]bool{}
		for _, jsonPath := range jsonFieldPaths(rt) {
			paths[jsonPath] = true
		}
		for field := range decls {
			if !paths[field] {
				t.Errorf("field_coverage.web_rpcs declares %s.%s, but %sRequest has no such "+
					"field — the wire struct changed and the declaration was left behind.",
					rpc, field, rpc)
			}
		}
	}
}
