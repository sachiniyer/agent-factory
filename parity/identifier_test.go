package parity

// Identifier parity: the string we SHOW is presentation-only and never resolves;
// the string we ACCEPT is discoverable from what we show.
//
// A fourth dimension, and the one that is invisible until someone types it. The
// others ask whether a surface has a verb, its options, or the right values.
// This asks something narrower and nastier: when a surface DISPLAYS a label and
// a user types it, does the tool lead them to the real handle rather than assert
// a tab they can see does not exist?
//
// It was found by hand, like the others (#1984):
//
//	$ af sessions tab-delete alpha --name Terminal
//	session "alpha" has no tab named "Terminal"     # the TUI tab bar says "Terminal"
//	$ af sessions tab-delete alpha --name shell
//	# works
//
// The TUI rendered a LABEL and the CLI demanded a NAME, so the error asserted a
// tab was absent while the user could see it on screen. One concept, two
// representations, and the mapping left for the user to discover — the same
// disease as #1972 (create --name vs a positional <title>) and #1970 (the web's
// copy of the agent enum).
//
// #1937 first closed the gap by ACCEPTING the label as an alias. #1986 undid that
// choice deliberately: the label is presentation-only and must never be an
// identifier, because two strings addressing one tab is the ambiguity #1929/#1904
// spent a PR removing from the tab surface. So the invariant flips. It is no
// longer "the label must be accepted"; it is two clauses that together keep the
// #1984 symptom from returning WITHOUT the ambiguity:
//
//  1. Name is the SOLE handle — the label never resolves (session.TabMatches).
//  2. The label stays DISCOVERABLE — when it differs from the name, a "no tab
//     named …" error names the real handle (session.TabIdentifiers), so a user
//     who read "Terminal" learns to type "shell" rather than hitting a dead end.
//
// Enforced for every tab kind, including kinds nobody has shipped a UI for yet,
// so a new TabKind cannot introduce a label that strands the user who reads it.

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// tabKindsUnderAudit is every TabKind the Tab type defines. A new kind added
// without an entry here fails TestTabKindCoverageIsComplete below, so this list
// cannot silently fall behind the enum it audits.
var tabKindsUnderAudit = []struct {
	kind session.TabKind
	name string // the canonical Tab.Name such a tab carries
}{
	{session.TabKindAgent, "agent"},
	{session.TabKindShell, "shell"},
	{session.TabKindProcess, "build"},
	{session.TabKindWeb, "docs"},
	{session.TabKindVSCode, "editor"},
}

// TestLabelIsNeverAnAcceptedIdentifier is the #1986 invariant: the name is the
// sole handle, the label never resolves, and where the two differ the label is
// still discoverable through the miss error.
func TestLabelIsNeverAnAcceptedIdentifier(t *testing.T) {
	for _, c := range tabKindsUnderAudit {
		tab := &session.Tab{Name: c.name, Kind: c.kind}
		label := session.TabLabel(tab)

		if label == "" {
			t.Errorf("kind %v renders an EMPTY label — a user can read nothing off the screen", c.kind)
			continue
		}
		// 1. The canonical name is the sole handle and must always resolve.
		if !session.TabMatches(tab, c.name) {
			t.Errorf("kind %v does not accept its canonical name %q — Name is the one handle a "+
				"user types, and every tab verb resolves against it.", c.kind, c.name)
		}
		// The label is presentation-only and must NOT be an identifier. This only
		// bites where the label diverges from the name (agent/shell); for kinds
		// whose label IS the name (process/web/vscode) it resolves because it is
		// the name, not because the label is accepted, so skip those.
		if label != c.name {
			if session.TabMatches(tab, label) {
				t.Errorf("kind %v is DISPLAYED as %q but that label is ACCEPTED as an identifier.\n\n"+
					"The label is presentation-only (#1986): accepting it makes two strings address "+
					"one tab, the ambiguity #1929/#1904 removed. Resolve on Name alone in "+
					"session.TabMatches; let the miss error name the real handle instead.", c.kind, label)
			}
			// 2. …but the label must not be a dead end: the error the miss produces
			//    must name it, so the user who read it can find the name to type.
			if ids := session.TabIdentifiers(tab); !strings.Contains(ids, label) {
				t.Errorf("kind %v is shown as %q, which does not resolve, yet TabIdentifiers(%q) = %s "+
					"does not surface %q — a user who read the label off the bar is stranded with no "+
					"path to the real name (#1984).", c.kind, label, c.name, ids, label)
			}
		}
		// Not blind: a matcher that accepts everything would satisfy clause 1.
		if session.TabMatches(tab, "definitely-not-a-tab-name") {
			t.Errorf("kind %v matches an arbitrary token — TabMatches is accepting anything, "+
				"so the assertions above prove nothing.", c.kind)
		}
	}
}

// TestTabIdentifiersNameTheAlternatives pins the discoverability half of #1984,
// which is now the whole mechanism (there is no alias): the error a real typo —
// or a label a user typed — produces must name the tabs that exist rather than
// assert an absence.
func TestTabIdentifiersNameTheAlternatives(t *testing.T) {
	shell := &session.Tab{Name: "shell", Kind: session.TabKindShell}
	got := session.TabIdentifiers(shell)
	// Both spellings, because the whole point is that the user knows only one.
	if got != `"shell" (shown as "Terminal")` {
		t.Errorf("TabIdentifiers(shell) = %s; it must give BOTH the name and the label when "+
			"they differ, since a user who saw \"Terminal\" cannot guess \"shell\" from an "+
			"error that only says \"shell\".", got)
	}

	// When the two agree, saying it twice is noise.
	proc := &session.Tab{Name: "build", Kind: session.TabKindProcess}
	if got := session.TabIdentifiers(proc); got != `"build"` {
		t.Errorf("TabIdentifiers(build) = %s, want %q — a tab whose label equals its name "+
			"should not be rendered as 'build (shown as build)'.", got, `"build"`)
	}
}

// TestTabKindCoverageIsComplete keeps the audit's denominator honest: a TabKind
// added without an entry above would never be checked, and the suite would go
// green while a new label went unchecked — under-coverage, which is worse than
// no check.
func TestTabKindCoverageIsComplete(t *testing.T) {
	// TabKind is an iota enum with no reflective listing, so probe upward until
	// the labels stop being distinct from the generic fallback. A new kind gets
	// its own label or falls through to the default; either way an unlisted kind
	// shows up as a gap between the highest audited kind and the first unnamed
	// one.
	highest := session.TabKind(-1)
	for _, c := range tabKindsUnderAudit {
		if c.kind > highest {
			highest = c.kind
		}
	}
	next := &session.Tab{Name: "", Kind: highest + 1}
	if label := session.TabLabel(next); label != "Tab" {
		t.Errorf("TabKind %v renders as %q, not the generic %q fallback — it looks like a real "+
			"kind that tabKindsUnderAudit (parity/identifier_test.go) does not list, so its "+
			"label is never audited against the #1986 identity split.", highest+1, label, "Tab")
	}
}
