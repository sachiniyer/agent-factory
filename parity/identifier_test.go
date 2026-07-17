package parity

// Identifier parity: is the string we SHOW the string we ACCEPT?
//
// A fourth dimension, and the one that is invisible until someone types it. The
// others ask whether a surface has a verb, its options, or the right values.
// This asks something narrower and nastier: when a surface DISPLAYS an
// identifier, does every surface that TAKES that identifier accept the string
// the user just read?
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
// The invariant is one line: anything a user can read off a surface must be
// accepted by the surfaces that take it. This test enforces it for every tab
// kind, including kinds nobody has shipped a UI for yet, so a new TabKind cannot
// introduce a label its own CLI rejects.

import (
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

// TestDisplayedTabIdentifiersAreAccepted is the invariant: every label a surface
// shows must be accepted by the surfaces that take a tab name.
func TestDisplayedTabIdentifiersAreAccepted(t *testing.T) {
	for _, c := range tabKindsUnderAudit {
		tab := &session.Tab{Name: c.name, Kind: c.kind}
		label := session.TabLabel(tab)

		if label == "" {
			t.Errorf("kind %v renders an EMPTY label — a user can read nothing to type", c.kind)
			continue
		}
		if !session.TabMatches(tab, label) {
			t.Errorf("kind %v is DISPLAYED as %q but %q is not accepted as its identifier.\n\n"+
				"A user reads the label off the screen and types it; if only %q works, the "+
				"error tells them a tab they can see does not exist (#1984). Accept the label "+
				"as an alias in session.TabMatches — additively, never by renaming the tab or "+
				"changing what the UI shows.", c.kind, label, label, c.name)
		}
		// The canonical name must keep working: the alias is additive, and every
		// script passing "shell" today must be unaffected.
		if !session.TabMatches(tab, c.name) {
			t.Errorf("kind %v no longer accepts its canonical name %q — the label alias must "+
				"ADD a spelling, never replace one. Scripts depend on this.", c.kind, c.name)
		}
		// Not blind: a matcher that accepts everything would satisfy the above.
		if session.TabMatches(tab, "definitely-not-a-tab-name") {
			t.Errorf("kind %v matches an arbitrary token — TabMatches is accepting anything, "+
				"so the assertions above prove nothing.", c.kind)
		}
	}
}

// TestTabIdentifiersNameTheAlternatives pins the other half of #1984, which is
// worth more than the alias: the error a real typo produces must name the tabs
// that exist rather than assert an absence.
func TestTabIdentifiersNameTheAlternatives(t *testing.T) {
	shell := &session.Tab{Name: "shell", Kind: session.TabKindShell}
	got := session.TabIdentifiers(shell)
	// Both spellings, because the whole point is that the user knows only one.
	if !contains([]string{got}, `"shell" (shown as "Terminal")`) {
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
// green while a new label went unaccepted — under-coverage, which is worse than
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
			"label is never checked against what the CLI accepts.", highest+1, label, "Tab")
	}
}
