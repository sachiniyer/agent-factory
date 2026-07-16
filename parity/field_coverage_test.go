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
	"sort"
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
func validateFieldDecls(t *testing.T, caps map[string]capability, label string, unreached []string, decls map[string]fieldDecl) {
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
			if _, ok := caps[d.Gap]; !ok {
				t.Errorf("%s field %q names unknown capability %q", label, f, d.Gap)
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
		for typeName, u := range use {
			total += len(u.Sites)
			rt := auditedRequests[typeName]
			unreached := unreachedFields(rt, u)
			decls := inv.FieldCoverage.Requests[typeName][surface]
			label := fmt.Sprintf("%s (%s, %v)", typeName, surface, u.Sites)
			validateFieldDecls(t, caps, label, unreached, decls)
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
		var unreached []string
		for _, f := range r.Fields {
			if !sent[f] {
				unreached = append(unreached, f)
			}
		}
		sort.Strings(unreached)
		validateFieldDecls(t, caps, "web "+rpc, unreached, inv.FieldCoverage.WebRPCs[rpc])
	}
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
