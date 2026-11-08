package overlay

import "testing"

// TestRefreshRowsParksCursorOnAddRowWhenTheHighlightedProjectVanishes pins the
// #4742 review finding on the background picker refresh: when a refresh removes
// the highlighted project, retaining the stale numeric index lands the cursor on
// whichever unrelated project shifted into that slot, and the next Enter or `D`
// acts on it. The cursor must park on the neutral trailing add-project row
// instead.
func TestRefreshRowsParksCursorOnAddRowWhenTheHighlightedProjectVanishes(t *testing.T) {
	p := pickerFixture() // afterburner(0), agent-factory(1), widgets(2); cursor on widgets
	cur, ok := p.HighlightedProject()
	if !ok || cur.Root != "/repos/widgets" {
		t.Fatalf("precondition: cursor must start on widgets, got %+v (ok=%v)", cur, ok)
	}

	// The refresh drops widgets (e.g. the daemon no longer reports it and the
	// registry never vouched for it). Retaining index 2 would land the cursor
	// on whatever shifted into that slot — here nothing does, but the stale
	// index would still point past the rows.
	p.RefreshRows([]Project{
		{Name: "afterburner", Root: "/repos/afterburner", SessionCount: 0},
		{Name: "agent-factory", Root: "/repos/agent-factory", SessionCount: 12},
	})
	if !p.addRowSelected() {
		t.Fatalf("a vanished highlighted project must park the cursor on the add row, got idx %d over %d rows", p.selectedIdx, len(p.all))
	}
	if row, ok := p.HighlightedProject(); ok {
		t.Fatalf("the vanished project must not stay highlightable, got %+v", row)
	}
}

// TestRefreshRowsKeepsCursorOnAddRowWhenTheListGrows pins the other half of the
// #4742 review finding: the trailing "+ Add project…" row is a sentinel, not a
// numeric slot. When the cursor is on it and a refresh grows the list, retaining
// the stale index would slide the cursor onto a newly appended project. The
// cursor must stay on the add row.
func TestRefreshRowsKeepsCursorOnAddRowWhenTheListGrows(t *testing.T) {
	p := pickerFixture() // cursor on widgets(2)
	// Park the cursor on the trailing add row (idx == len(p.all) == 3).
	for !p.addRowSelected() {
		p.HandleKeyPress(keyRune('j'))
	}
	if !p.addRowSelected() {
		t.Fatalf("precondition: cursor must be on the add row, got idx %d over %d rows", p.selectedIdx, len(p.all))
	}

	// A refresh appends a new project. The stale index (3) now points at the
	// new project; keeping the sentinel parks the cursor on the new add row
	// (idx 4) instead.
	p.RefreshRows([]Project{
		{Name: "afterburner", Root: "/repos/afterburner", SessionCount: 0},
		{Name: "agent-factory", Root: "/repos/agent-factory", SessionCount: 12},
		{Name: "newproject", Root: "/repos/newproject", SessionCount: 1},
		{Name: "widgets", Root: "/repos/widgets", SessionCount: 3},
	})
	if !p.addRowSelected() {
		t.Fatalf("a cursor on the add row must stay on the add row when the list grows, got idx %d over %d rows", p.selectedIdx, len(p.all))
	}
	if row, ok := p.HighlightedProject(); ok {
		t.Fatalf("the add row is not a project and must not become highlightable, got %+v", row)
	}
}
