package overlay

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func pickerFixture() *ProjectPickerOverlay {
	// Pre-sorted by name, as the app's buildProjectList hands them in:
	// afterburner, agent-factory, widgets.
	projects := []Project{
		{Name: "afterburner", Root: "/repos/afterburner", SessionCount: 0},
		{Name: "agent-factory", Root: "/repos/agent-factory", SessionCount: 12},
		{Name: "widgets", Root: "/repos/widgets", SessionCount: 3},
	}
	return NewProjectPickerOverlay(projects, "/repos/widgets")
}

// keyRune builds the KeyMsg for a typed rune (e.g. the vim nav keys j/k), which
// arrive as KeyRunes and stringify to the bare character.
func keyRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func typeRunes(p *ProjectPickerOverlay, s string) {
	for _, r := range s {
		if r == ' ' {
			p.HandleKeyPress(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		p.HandleKeyPress(keyRune(r))
	}
}

func TestProjectPickerCurrentRootPreselected(t *testing.T) {
	p := pickerFixture()
	// widgets sorts after agent-factory/afterburner; the list is shown in the
	// caller's order: afterburner(0), agent-factory(1), widgets(2).
	// currentRoot=/repos/widgets, so the cursor starts on widgets (idx 2).
	proj, ok := p.selectedProjectForTest()
	if !ok || proj.Root != "/repos/widgets" {
		t.Fatalf("expected the current project preselected, got %+v (ok=%v)", proj, ok)
	}
}

func TestProjectPickerNavigatesFullListNoFilter(t *testing.T) {
	p := pickerFixture()
	// Preselected on widgets (idx 2). The navigable rows are the FULL list plus
	// the trailing add row: afterburner(0), agent-factory(1), widgets(2), add(3).
	if p.selectedIdx != 2 {
		t.Fatalf("expected preselect on widgets (idx 2), got %d", p.selectedIdx)
	}

	// down/j walks onto the add row and clamps there (no wrap), matching the rail.
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	if p.selectedIdx != 3 || !p.addRowSelected() {
		t.Fatalf("Down should move onto the add row (idx 3); got %d addRow=%v", p.selectedIdx, p.addRowSelected())
	}
	p.HandleKeyPress(keyRune('j'))
	if p.selectedIdx != 3 {
		t.Fatalf("j at the bottom should clamp, got %d", p.selectedIdx)
	}

	// k walks back up across every project — the whole list, unfiltered:
	// add(3) -> widgets(2) -> agent-factory(1) -> afterburner(0).
	for _, want := range []int{2, 1, 0} {
		p.HandleKeyPress(keyRune('k'))
		if p.selectedIdx != want {
			t.Fatalf("k should move to idx %d, got %d", want, p.selectedIdx)
		}
	}
	// up at the top clamps (no wrap).
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyUp})
	if p.selectedIdx != 0 {
		t.Fatalf("Up at the top should clamp, got %d", p.selectedIdx)
	}

	// The full project list is intact — navigation never filters.
	if len(p.all) != 3 {
		t.Fatalf("navigation must not change the list; len(all)=%d", len(p.all))
	}
}

func TestProjectPickerTypingDoesNotFilter(t *testing.T) {
	p := pickerFixture()
	before := p.selectedIdx
	// Typed letters do nothing in list mode: no filtering, no cursor movement,
	// no accidental add-mode entry.
	typeRunes(p, "af repos/widg zzz")
	if len(p.all) != 3 {
		t.Fatalf("typing must not filter the list; len(all)=%d", len(p.all))
	}
	if p.selectedIdx != before {
		t.Fatalf("typing must not move the cursor; got %d want %d", p.selectedIdx, before)
	}
	if p.adding {
		t.Fatalf("typing must not enter add mode")
	}
}

func TestProjectPickerAddRowAlwaysLastAndReachable(t *testing.T) {
	p := pickerFixture()
	// The add row is the last navigable index, one past the projects.
	if p.rowCount() != len(p.all)+1 {
		t.Fatalf("rowCount should be projects+1; got %d for %d projects", p.rowCount(), len(p.all))
	}
	// Jumping down past the last project lands on the add row.
	for i := 0; i < 5; i++ {
		p.HandleKeyPress(keyRune('j'))
	}
	if !p.addRowSelected() {
		t.Fatalf("Down past the last project should land on the add row; idx=%d", p.selectedIdx)
	}
}

func TestProjectPickerEnterSelectsProject(t *testing.T) {
	p := pickerFixture()
	// Move to the top project row and submit.
	for i := 0; i < 5; i++ {
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyUp})
	}
	closed := p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !closed || !p.IsSubmitted() {
		t.Fatalf("Enter on a project row should submit and close (closed=%v submitted=%v)", closed, p.IsSubmitted())
	}
	proj, ok := p.SelectedProject()
	if !ok || proj.Name != "afterburner" {
		t.Fatalf("expected afterburner selected, got %+v (ok=%v)", proj, ok)
	}
}

func TestProjectPickerEnterOnAddRowEntersAddMode(t *testing.T) {
	p := pickerFixture()
	// Jump to the add row.
	for i := 0; i < 5; i++ {
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !p.addRowSelected() {
		t.Fatalf("cursor should be on the add row")
	}
	closed := p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if closed {
		t.Fatalf("entering add mode must not close the overlay")
	}
	if !p.adding {
		t.Fatalf("Enter on the add row should switch to add mode")
	}

	// Type a path; Enter requests the add (does not close), Esc cancels back.
	typeRunes(p, "/some/repo")
	if closed := p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}); closed {
		t.Fatalf("add-mode Enter must not close the overlay (caller validates)")
	}
	path, ok := p.TakeAddRequest()
	if !ok || path != "/some/repo" {
		t.Fatalf("TakeAddRequest = (%q,%v), want (/some/repo,true)", path, ok)
	}
	// Consumed once.
	if _, ok := p.TakeAddRequest(); ok {
		t.Fatalf("TakeAddRequest should only fire once")
	}
}

func TestProjectPickerAddErrorKeepsOpen(t *testing.T) {
	p := pickerFixture()
	for i := 0; i < 5; i++ {
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter add mode
	typeRunes(p, "/bad")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	p.TakeAddRequest()
	p.SetAddError("not a git repository: /bad")
	p.SetMaxSize(80, 24)
	out := renderedText(p.Render())
	if !strings.Contains(out, "not a git repository") {
		t.Fatalf("add error should render inline; got:\n%s", out)
	}
	// Esc from add mode returns to the list without closing.
	if closed := p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc}); closed {
		t.Fatalf("Esc in add mode should return to the list, not close")
	}
	if p.adding {
		t.Fatalf("Esc should leave add mode")
	}
}

func TestProjectPickerEscCancels(t *testing.T) {
	p := pickerFixture()
	closed := p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if !closed || p.IsSubmitted() {
		t.Fatalf("Esc should cancel-close without submitting (closed=%v submitted=%v)", closed, p.IsSubmitted())
	}
	if !p.canceled {
		t.Fatalf("Esc should mark the picker canceled")
	}
}

func TestProjectPickerRenderShowsCountsAndNavHint(t *testing.T) {
	p := pickerFixture()
	p.SetMaxSize(80, 24)
	out := renderedText(p.Render())
	if !strings.Contains(out, "agent-factory") || !strings.Contains(out, "(12)") {
		t.Fatalf("render should show project names and session counts; got:\n%s", out)
	}
	if !strings.Contains(out, "Add project") {
		t.Fatalf("render should show the add-project affordance; got:\n%s", out)
	}
	// The footer advertises rail-style navigation, not search.
	if !strings.Contains(out, "select") || !strings.Contains(out, "switch") {
		t.Fatalf("render should show the j/k navigate hint; got:\n%s", out)
	}
}

func TestProjectPickerRebindFlow(t *testing.T) {
	// A registry-backed row (RegistryID set) is what `b` rebinds; a session-
	// derived row has no registration to move.
	p := NewProjectPickerOverlay([]Project{
		{Name: "moved", Root: "/old/moved", RegistryID: "prj_aaa", MissingPath: true},
		{Name: "derived", Root: "/repos/derived"},
	}, "")
	if p.selectedIdx != 0 {
		t.Fatalf("cursor should start on the first row, got %d", p.selectedIdx)
	}

	// b on the registry row enters rebind mode; the hint names the project.
	if closed := p.HandleKeyPress(keyRune('b')); closed {
		t.Fatalf("entering rebind mode must not close the overlay")
	}
	if !p.rebinding {
		t.Fatalf("b on a registry-backed row should enter rebind mode")
	}
	p.SetMaxSize(80, 24)
	if out := renderedText(p.Render()); !strings.Contains(out, "moved") {
		t.Fatalf("rebind mode should name the target project; got:\n%s", out)
	}

	// Type a replacement path; Enter submits it for the caller once.
	typeRunes(p, "/new/moved")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok := p.TakeRebindRequest()
	if !ok || req.Path != "/new/moved" || req.Project.RegistryID != "prj_aaa" {
		t.Fatalf("TakeRebindRequest = (%+v, %v), want the registry row + typed path", req, ok)
	}
	if _, ok := p.TakeRebindRequest(); ok {
		t.Fatalf("TakeRebindRequest should only fire once")
	}
}

// TestProjectPickerRebindConflictRebuildsRows pins the #4888-review follow-up
// to #4822: a rebind conflict means the registration moved, so every field of
// the row the picker returns to — Root, RepoID, name, missing-path — is stale.
// The open picker is rebuilt from the caller's refreshed list rather than
// patched in place, so an Esc back to it cannot select a checkout the registry
// no longer records, and a retried rebind expects the root reported NOW.
func TestProjectPickerRebindConflictRebuildsRows(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "old", Root: "/old/root", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/root", MissingPath: true},
		{Name: "other", Root: "/repos/other", RepoID: "repo-other", RegistryID: "prj_bbb", RegistryRoot: "/repos/other"},
	}, "")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}

	// The caller's registry re-read puts the project somewhere else entirely:
	// different root, identity, display name, and no longer missing.
	p.SetRebindConflict("Rebound elsewhere, to /new/root · Enter retries from there", []Project{
		{Name: "moved", Root: "/new/root", RepoID: "repo-new", RegistryID: "prj_aaa", RegistryRoot: "/new/root"},
		{Name: "other", Root: "/repos/other", RepoID: "repo-other", RegistryID: "prj_bbb", RegistryRoot: "/repos/other"},
	})

	// Esc returns to the list — the row must describe the rebound
	// registration, not the pre-conflict snapshot.
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := p.HighlightedProject()
	if !ok {
		t.Fatalf("the rebuilt list should still have a highlighted row")
	}
	if row.Root != "/new/root" || row.RepoID != "repo-new" || row.Name != "moved" || row.MissingPath || row.RegistryRoot != "/new/root" {
		t.Fatalf("the conflict row must be the refreshed one, got %+v", row)
	}

	// Rebinding again from that row carries the registry's CURRENT root as the
	// expected root — the retry is a compare-and-set on present state.
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/final")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok := p.TakeRebindRequest()
	if !ok || req.Project.RegistryRoot != "/new/root" || req.Project.Root != "/new/root" {
		t.Fatalf("the retry must carry the refreshed row, got %+v (ok=%v)", req.Project, ok)
	}
}

// TestProjectPickerRebindConflictKeepsCursorOnReboundRow pins the other half
// of the #4888-review picker rebuild: a conflict rebuild can REORDER rows —
// the record's name feeds the sort — so leaving selectedIdx at its old numeric
// position highlights whichever row now sits there. Esc must return to a list
// whose cursor rests on the registration the rebind targeted.
func TestProjectPickerRebindConflictKeepsCursorOnReboundRow(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha"},
		{Name: "omega", Root: "/repos/omega"},
		{Name: "zed", Root: "/repos/zed"},
	}, "")
	// The cursor is on alpha (idx 0); its rebind is refused. The rebuild names
	// the rebound registration "zeta" — sorting it LAST — so a stale index 0
	// would highlight omega instead.
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}
	p.SetRebindConflict("Rebound elsewhere, to /new/zeta · Enter retries from there", []Project{
		{Name: "omega", Root: "/repos/omega"},
		{Name: "zed", Root: "/repos/zed"},
		{Name: "zeta", Root: "/new/zeta", RepoID: "repo-new", RegistryID: "prj_aaa", RegistryRoot: "/new/zeta"},
	})
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})

	row, ok := p.HighlightedProject()
	if !ok {
		t.Fatalf("the rebuilt list should still have a highlighted row")
	}
	if row.RegistryID != "prj_aaa" || row.Name != "zeta" || row.Root != "/new/zeta" {
		t.Fatalf("the cursor must follow the rebound row to its rebuilt position, got %+v", row)
	}
}

// TestProjectPickerRebindConflictClampsCursor: if the refreshed list is
// shorter than the cursor position — the record was deleted outright, so it
// has no row at all — the rebuild must not leave selectedIdx pointing past
// the rows.
func TestProjectPickerRebindConflictClampsCursor(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "a", Root: "/repos/a"},
		{Name: "b", Root: "/repos/b"},
		{Name: "gone", Root: "/old/gone", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/gone"},
	}, "")
	// The cursor sits on the last row, the registry record being rebound.
	p.HandleKeyPress(keyRune('j'))
	p.HandleKeyPress(keyRune('j'))
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}
	// The record is gone: two rows remain where the cursor was at index 2.
	// Clamping lands it on the trailing add-project row — a valid cursor spot —
	// never past it, and never on a stale row for the vanished record.
	p.SetRebindConflict("Rebound elsewhere — its record is gone", []Project{
		{Name: "a", Root: "/repos/a"},
		{Name: "b", Root: "/repos/b"},
	})
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if p.selectedIdx > len(p.all) {
		t.Fatalf("cursor must stay within the navigable rows, got %d over %d projects", p.selectedIdx, len(p.all))
	}
	if row, ok := p.HighlightedProject(); ok && row.RegistryID == "prj_aaa" {
		t.Fatalf("a deleted record's stale row must not be highlightable, got %+v", row)
	}
	// Navigating up off the add row lands on a live one that selects normally.
	p.HandleKeyPress(keyRune('k'))
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	row, ok := p.SelectedProject()
	if !ok || row.RegistryID == "prj_aaa" {
		t.Fatalf("only a live row may be selectable after the record vanished, got %+v (ok=%v)", row, ok)
	}
}

// TestProjectPickerRebindConflictPreservesRowsOnFailedSnapshot pins the #4888
// round-4 review finding: when the conflict-time session snapshot never
// answers, the refreshed list holds only the registry-side union — installing
// it wholesale would drop every session-derived row and zero the live counts
// the daemon still runs. The merge must keep session-derived rows, carry each
// surviving registry row's last-known counts onto its fresh record, and still
// drop a registry row whose record a healthy read proves is gone.
func TestProjectPickerRebindConflictPreservesRowsOnFailedSnapshot(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha", MissingPath: true, SessionCount: 4, InPlaceCount: 1},
		{Name: "sessions", Root: "/repos/sessions", RepoID: "repo-sess", SessionCount: 3},
		{Name: "gone", Root: "/repos/gone", RepoID: "repo-gone", RegistryID: "prj_gone", RegistryRoot: "/repos/gone", SessionCount: 2},
	}, "/old/alpha")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}

	// The snapshot fetch failed: fresh rows carry the registry union only —
	// alpha's record moved (/old/alpha → /new/alpha, new repo identity, no
	// longer missing), gone's record is deleted, and a registration newer
	// than the picker appears. No row carries a session count.
	p.SetRebindConflictPreserving("Rebound elsewhere, to /new/alpha · Enter retries from there", []Project{
		{Name: "alpha", Root: "/new/alpha", RepoID: "repo-new", RegistryID: "prj_aaa", RegistryRoot: "/new/alpha"},
		{Name: "newbie", Root: "/repos/newbie", RepoID: "repo-nb", RegistryID: "prj_new", RegistryRoot: "/repos/newbie"},
	}, false)

	if len(p.all) != 3 {
		t.Fatalf("merged list must keep the session row, refresh the registry row, and add the new record: %+v", p.all)
	}
	if got := p.all[0]; got.RegistryID != "prj_aaa" || got.Root != "/new/alpha" || got.RepoID != "repo-new" || got.MissingPath {
		t.Fatalf("the conflict row must be the fresh registry record, got %+v", got)
	}
	if p.all[0].SessionCount != 4 || p.all[0].InPlaceCount != 1 {
		t.Fatalf("the snapshot that failed could not recount sessions — the row keeps its last counts, got %+v", p.all[0])
	}
	if got := p.all[1]; got.Name != "sessions" || got.SessionCount != 3 || got.RegistryID != "" {
		t.Fatalf("the session-derived row must be kept whole, got %+v", got)
	}
	for _, row := range p.all {
		if row.RegistryID == "prj_gone" {
			t.Fatalf("a record absent from a healthy registry read must still drop its stale row: %+v", p.all)
		}
	}
	if got := p.all[2]; got.RegistryID != "prj_new" {
		t.Fatalf("a registration newer than the picker still lands, got %+v", p.all)
	}

	// The cursor and rebind target re-seat onto the rebound row as before.
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := p.HighlightedProject()
	if !ok || row.RegistryID != "prj_aaa" || row.Root != "/new/alpha" {
		t.Fatalf("Esc must highlight the rebound row from the merge, got %+v (ok=%v)", row, ok)
	}
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/final")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok := p.TakeRebindRequest()
	if !ok || req.Project.RegistryRoot != "/new/alpha" {
		t.Fatalf("the retry must expect the fresh registry root, got %+v (ok=%v)", req.Project, ok)
	}
}

// TestProjectPickerRebindConflictPreservesRowsWhenRegistryIsDegraded is the
// other half: when the registry read failed TOO, no record's absence is
// proven — every registry row must survive behind the degraded warning rather
// than the merge emptying the list it was meant to repair.
func TestProjectPickerRebindConflictPreservesRowsWhenRegistryIsDegraded(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha", SessionCount: 4},
		{Name: "sessions", Root: "/repos/sessions", RepoID: "repo-sess", SessionCount: 3},
	}, "/old/alpha")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}

	// Both reads failed: the fresh list cannot describe any registry record,
	// so nothing is proven gone — the old rows stay, counts included.
	p.SetRebindConflictPreserving("Rebound elsewhere — refresh and retry", nil, true)

	if len(p.all) != 2 {
		t.Fatalf("with both reads failed the existing rows must be kept whole, got %+v", p.all)
	}
	if p.all[0].RegistryID != "prj_aaa" || p.all[0].SessionCount != 4 {
		t.Fatalf("the registry row must survive a degraded read untouched, got %+v", p.all[0])
	}
	if p.all[1].Name != "sessions" || p.all[1].SessionCount != 3 {
		t.Fatalf("the session-derived row must survive a degraded read untouched, got %+v", p.all[1])
	}
}

func TestProjectPickerRebindOnlyOnRegistryRows(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "derived", Root: "/repos/derived"}, // no RegistryID: session-derived
		{Name: "registered", Root: "/repos/registered", RegistryID: "prj_bbb"},
	}, "")
	p.HandleKeyPress(keyRune('b'))
	if p.rebinding {
		t.Fatalf("b on a session-derived row must not enter rebind mode — there is no registration to move")
	}
	p.HandleKeyPress(keyRune('j'))
	p.HandleKeyPress(keyRune('b'))
	if !p.rebinding {
		t.Fatalf("b on the registry-backed row should enter rebind mode")
	}
	// Esc returns to the list without submitting, like add mode.
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if p.rebinding {
		t.Fatalf("Esc should leave rebind mode")
	}
}

func TestProjectPickerRebindErrorKeepsOpen(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "gone", Root: "/old/gone", RegistryID: "prj_ccc", MissingPath: true},
	}, "")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/bad")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	p.TakeRebindRequest()
	p.SetRebindError("path is already bound to another project")
	p.SetMaxSize(80, 24)
	if out := renderedText(p.Render()); !strings.Contains(out, "already bound") {
		t.Fatalf("rebind error should render inline; got:\n%s", out)
	}
	// While rebinding, the highlighted row is withheld so a destructive
	// shortcut (D) cannot fire against it mid-edit.
	if _, ok := p.HighlightedProject(); ok {
		t.Fatalf("HighlightedProject must be empty while the rebind input is active")
	}
}

func TestProjectPickerMissingPathMarker(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "here", Root: "/repos/here", RegistryID: "prj_ddd"},
		{Name: "gone", Root: "/old/gone", RegistryID: "prj_eee", MissingPath: true},
	}, "")
	p.SetMaxSize(80, 24)
	out := renderedText(p.Render())
	if !strings.Contains(out, "missing") {
		t.Fatalf("a registry row whose checkout is gone must say so; got:\n%s", out)
	}
	// The rebind verb is advertised on the registry row's hint.
	p.HandleKeyPress(keyRune('j'))
	out = renderedText(p.Render())
	if !strings.Contains(out, "rebind") {
		t.Fatalf("the hint should advertise b rebind on a registry-backed row; got:\n%s", out)
	}
}

// selectedProjectForTest returns the row under the cursor as a Project without
// requiring submission, for assertions on the initial highlight.
func (p *ProjectPickerOverlay) selectedProjectForTest() (Project, bool) {
	if p.selectedIdx >= 0 && p.selectedIdx < len(p.all) {
		return p.all[p.selectedIdx], true
	}
	return Project{}, false
}

// TestProjectPickerDegradedNotice pins #3298 in the picker: a failed registry
// read renders the incompleteness warning above the rows, and a healthy read
// does not.
func TestProjectPickerDegradedNotice(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{{Name: "alpha", Root: "/repos/alpha"}}, "")
	p.SetMaxSize(60, 20)
	if out := renderedText(p.Render()); strings.Contains(out, "Cannot read registry") {
		t.Fatalf("a healthy picker must not warn, got:\n%s", out)
	}
	p.SetDegraded(true)
	out := renderedText(p.Render())
	if !strings.Contains(out, "Cannot read registry") {
		t.Fatalf("a degraded picker must warn that the list may be incomplete, got:\n%s", out)
	}
}

// TestProjectPickerStaysWithinMaxHeight pins the #3331 review P2: the scroll
// indicators and the degraded warning must be budgeted inside the frame,
// because PlaceOverlay refuses to composite an oversized foreground — one
// extra row makes the whole TUI background disappear. Sweep list sizes,
// cursor positions, and degraded state at heights where the chrome fits; no
// rendered frame may exceed the configured maximum.
func TestProjectPickerStaysWithinMaxHeight(t *testing.T) {
	for _, maxH := range []int{10, 14, 20} {
		for n := 1; n <= 18; n++ {
			for _, degraded := range []bool{false, true} {
				projects := make([]Project, n)
				for i := range projects {
					projects[i] = Project{Name: fmt.Sprintf("p%02d", i), Root: fmt.Sprintf("/repos/p%02d", i)}
				}
				p := NewProjectPickerOverlay(projects, "")
				p.SetMaxSize(60, maxH)
				p.SetDegraded(degraded)
				for step := 0; step <= n+1; step++ {
					if got := lipgloss.Height(p.Render()); got > maxH {
						t.Fatalf("picker rendered %d rows with maxHeight=%d (n=%d degraded=%v cursor step %d)", got, maxH, n, degraded, step)
					}
					p.HandleKeyPress(keyRune('j'))
				}
			}
		}
	}
}

// TestProjectPickerRebindPendingIsInert pins the single-flight property Codex
// flagged on #4789: after Enter hands a request to the caller, the daemon may
// be slow to answer — the form must not let a second Enter (or edits that
// change what the on-screen path appears to be) race a second mutation. While
// pending, every key is inert; a rejection (SetRebindError) re-arms the form,
// and the path the user corrects is the one that was actually submitted.
func TestProjectPickerRebindPendingIsInert(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{
		{Name: "gone", Root: "/old/gone", RegistryID: "prj_p1", MissingPath: true},
	}, "")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/first/path")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	req, ok := p.TakeRebindRequest()
	if !ok || req.Path != "/first/path" || req.Project.RegistryID != "prj_p1" {
		t.Fatalf("TakeRebindRequest = (%+v, %v), want the registry row + typed path", req, ok)
	}

	// The daemon is still deciding: a second Enter must NOT submit again, and
	// edits must not change the input the pending answer refers to.
	typeRunes(p, "/second/path")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); ok {
		t.Fatalf("a second rebind submitted while the first was still in flight")
	}
	if p.rebindInput != "/first/path" {
		t.Fatalf("pending edits must be inert: input drifted to %q", p.rebindInput)
	}
	// Esc is inert while pending too — leaving the mode mid-flight could
	// re-arm `b` and admit a second request.
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if !p.rebinding {
		t.Fatalf("Esc must not leave rebind mode while a request is pending")
	}
	// The pending hint replaces "enter rebind" so nothing invites the second
	// submission.
	p.SetMaxSize(80, 24)
	if out := renderedText(p.Render()); !strings.Contains(out, "rebinding") {
		t.Fatalf("a pending rebind should say it is in flight; got:\n%s", out)
	}

	// A rejection re-arms the form on the submitted path — the user corrects
	// what was actually sent.
	p.SetRebindError("path is already bound to another project")
	typeRunes(p, "-fixed")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok = p.TakeRebindRequest()
	if !ok || req.Path != "/first/path-fixed" {
		t.Fatalf("after a rejection the corrected path should resubmit; got (%q, %v)", req.Path, ok)
	}
}

// TestProjectPickerRebindDeniedRefuses pins the remote-target refusal Codex
// flagged on #4789: when the caller marks rebind unavailable (a remote
// daemon — the picker's prj_ ids and the path both resolve on the CLIENT but
// the mutation would land on the remote's registry), `b` still enters the
// mode so the refusal is visible, and Enter never produces a request.
func TestProjectPickerRebindDeniedRefuses(t *testing.T) {
	const deny = "local registry — `af projects rebind` on daemon host"
	p := NewProjectPickerOverlay([]Project{
		{Name: "gone", Root: "/old/gone", RegistryID: "prj_p2", MissingPath: true},
	}, "")
	p.SetRebindDenied(deny)

	p.HandleKeyPress(keyRune('b'))
	if !p.rebinding {
		t.Fatalf("b should still enter rebind mode so the refusal is visible")
	}
	p.SetMaxSize(80, 24)
	if out := renderedText(p.Render()); !strings.Contains(out, "af projects rebind") {
		t.Fatalf("the refusal should render on entry; got:\n%s", out)
	}

	typeRunes(p, "/any/path")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); ok {
		t.Fatalf("a denied rebind must never produce a request")
	}
	if p.rebindErr != deny {
		t.Fatalf("Enter should re-show the refusal, got %q", p.rebindErr)
	}
}

// submitRebind drives a picker over one registry row through b, a typed path
// and Enter, and returns the request the caller would send.
func submitRebind(t *testing.T, p *ProjectPickerOverlay, path string) RebindRequest {
	t.Helper()
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, path)
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok := p.TakeRebindRequest()
	if !ok {
		t.Fatalf("Enter in rebind mode should produce a request")
	}
	return req
}

// TestProjectPickerRebindReplyOwnership pins the Breken finding on #4789: a
// rebind reply is async and can outlive the picker that asked. It must be
// accepted only by the picker instance waiting on that exact request — never
// by a freshly opened picker, never by the same picker for an older request it
// already settled, and never by a picker with nothing in flight.
func TestProjectPickerRebindReplyOwnership(t *testing.T) {
	rows := []Project{{Name: "gone", Root: "/old/gone", RegistryID: "prj_own", MissingPath: true}}

	a := NewProjectPickerOverlay(rows, "")
	reqA := submitRebind(t, a, "/new/a")
	if !a.OwnsRebindReply(reqA.Token) {
		t.Fatalf("picker A must own the reply to the request it submitted")
	}

	// A closes; B opens on the same row. A's reply is not B's, whether B is
	// idle or has its own request in flight.
	b := NewProjectPickerOverlay(rows, "")
	if b.OwnsRebindReply(reqA.Token) {
		t.Fatalf("an idle, freshly opened picker must not own an older picker's reply")
	}
	reqB := submitRebind(t, b, "/new/b")
	if reqB.Token == reqA.Token {
		t.Fatalf("two pickers issued the same request token %d", reqA.Token)
	}
	if b.OwnsRebindReply(reqA.Token) {
		t.Fatalf("picker B must not own picker A's reply while waiting on its own")
	}
	if !b.OwnsRebindReply(reqB.Token) {
		t.Fatalf("picker B must own its own reply")
	}

	// Once B's request is answered (a rejection), neither it nor a later
	// duplicate delivery of the same reply belongs to B any more.
	b.SetRebindError("not a git repository")
	if b.OwnsRebindReply(reqB.Token) {
		t.Fatalf("a settled request's reply must not be owned a second time")
	}
	if b.OwnsRebindReply(0) {
		t.Fatalf("the zero token must never be owned")
	}
}

// TestProjectPickerRebindPendingReportsForCtrlC pins the Codex hard-exit
// finding on #4789: the pending form consumes every key, so the picker must
// report that it is pending — the app routes Ctrl+C past it to quit — and the
// pending hint must say Ctrl+C still works.
func TestProjectPickerRebindPendingReportsForCtrlC(t *testing.T) {
	p := NewProjectPickerOverlay([]Project{{Name: "gone", Root: "/old/gone", RegistryID: "prj_cc"}}, "")
	if p.RebindPending() {
		t.Fatalf("an idle picker must not report a pending rebind")
	}
	submitRebind(t, p, "/new/path")
	if !p.RebindPending() {
		t.Fatalf("a submitted rebind must report pending so Ctrl+C can bypass the inert form")
	}
	p.SetMaxSize(80, 24)
	if out := renderedText(p.Render()); !strings.Contains(out, "ctrl+c") {
		t.Fatalf("the pending hint should say ctrl+c still quits; got:\n%s", out)
	}
	p.SetRebindError("refused")
	if p.RebindPending() {
		t.Fatalf("a rejection must clear pending")
	}
}

// TestProjectPickerFormsStayWithinMaxHeight pins the Codex height finding on
// #4789: TestProjectPickerStaysWithinMaxHeight sweeps only list mode, but the
// add and rebind forms — with an inline error, a degraded registry, a pending
// hint or the remote refusal pre-shown — must fit the same frame, down to the
// 10-row terminal where the dialog leaves six text rows.
func TestProjectPickerFormsStayWithinMaxHeight(t *testing.T) {
	longErr := strings.Repeat("path is already bound to another project ", 3)
	for _, maxH := range []int{8, 9, 10, 11, 14, 24} {
		for _, degraded := range []bool{false, true} {
			for _, mode := range []string{"add", "add-err", "rebind", "rebind-err", "rebind-pending", "rebind-denied"} {
				p := NewProjectPickerOverlay([]Project{{Name: "gone", Root: "/old/gone", RegistryID: "prj_h", MissingPath: true}}, "")
				p.SetMaxSize(60, maxH)
				p.SetDegraded(degraded)
				switch mode {
				case "add", "add-err":
					p.HandleKeyPress(keyRune('j'))
					p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
					typeRunes(p, "/some/path")
					if mode == "add-err" {
						p.SetAddError(longErr)
					}
				case "rebind-denied":
					p.SetRebindDenied("local registry — `af projects rebind` on daemon host")
					p.HandleKeyPress(keyRune('b'))
				default:
					p.HandleKeyPress(keyRune('b'))
					typeRunes(p, "/some/path")
					if mode != "rebind" {
						p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
						p.TakeRebindRequest()
					}
					if mode == "rebind-err" {
						p.SetRebindError(longErr)
					}
				}
				out := p.Render()
				if got := lipgloss.Height(out); got > maxH {
					t.Fatalf("%s form rendered %d rows with maxHeight=%d (degraded=%v):\n%s", mode, got, maxH, degraded, renderedText(out))
				}
				// The input line survives every budget: it is what the user acts on.
				if !strings.Contains(renderedText(out), "/some/path") && mode != "rebind-denied" {
					t.Fatalf("%s form at maxHeight=%d dropped the input line:\n%s", mode, maxH, renderedText(out))
				}
			}
		}
	}
}

// TestProjectPickerRegistryHintKeepsRebindAtNarrowWidths pins Codex on #4789:
// the hint is the only on-screen discovery point for the picker-local `b`, so a
// registry row must advertise it — and D — at every supported width, down to
// the 40-column terminal minimum, where the old fallback dropped both.
func TestProjectPickerRegistryHintKeepsRebindAtNarrowWidths(t *testing.T) {
	for w := 40; w <= 100; w++ {
		p := NewProjectPickerOverlay([]Project{
			{Name: "gone", Root: "/old/gone", RegistryID: "prj_w", MissingPath: true},
		}, "")
		p.SetMaxSize(w, 24)
		out := renderedText(p.Render())
		if !strings.Contains(out, "b rebind") || !strings.Contains(out, "D delete") {
			t.Fatalf("width %d: a registry row's hint must keep `b rebind` and `D delete`; got:\n%s", w, out)
		}
		if got := lipgloss.Width(p.Render()); got > w {
			t.Fatalf("width %d: picker rendered %d columns", w, got)
		}
	}
}

// TestProjectPickerRebindConflictCoalescesAReboundOntoASessionRepo pins the
// #4888 round-6 review finding: a registration that rebounds onto a repo the
// picker already lists from live sessions must not produce a second row for
// the same RepoID — the registry copy would inherit counts tallied against
// the OLD root while the session row kept the real ones. The merge grafts the
// registration fields onto the session row instead.
func TestProjectPickerRebindConflictCoalescesAReboundOntoASessionRepo(t *testing.T) {
	// The session row sorts before AND after the registry row across the two
	// orderings — the graft must land either way.
	for _, sessionFirst := range []bool{true, false} {
		rows := []Project{
			{Name: "sessions", Root: "/repos/shared", RepoID: "repo-shared", SessionCount: 3, InPlaceCount: 2},
			{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha", RegistryCheckoutID: "chk_old", MissingPath: true, SessionCount: 4},
		}
		if !sessionFirst {
			rows[0], rows[1] = rows[1], rows[0]
		}
		p := NewProjectPickerOverlay(rows, "/old/alpha")
		p.HandleKeyPress(keyRune('b'))
		typeRunes(p, "/candidate")
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
		if _, ok := p.TakeRebindRequest(); !ok {
			t.Fatalf("the submitted rebind should reach the caller")
		}

		// The conflict refresh failed on the session side: the fresh rows carry
		// the registry union only, and the alpha record's rebind landed it on
		// the repo the "sessions" row already owns.
		p.SetRebindConflictPreserving("Rebound elsewhere, to /repos/shared · Enter retries from there", []Project{
			{Name: "alpha", Root: "/repos/shared", RepoID: "repo-shared", RegistryID: "prj_aaa", RegistryRoot: "/repos/shared", RegistryCheckoutID: "chk_new"},
		}, false)

		if len(p.all) != 1 {
			t.Fatalf("sessionFirst=%v: the rebound record must not produce a second row for the same repo, got %+v", sessionFirst, p.all)
		}
		row := p.all[0]
		if row.Name != "sessions" || row.SessionCount != 3 || row.InPlaceCount != 2 {
			t.Fatalf("sessionFirst=%v: the session row keeps its own live counts, got %+v", sessionFirst, row)
		}
		if row.RegistryID != "prj_aaa" || row.RegistryRoot != "/repos/shared" || row.RegistryCheckoutID != "chk_new" || row.MissingPath {
			t.Fatalf("sessionFirst=%v: the session row gains the registration's fresh identity, got %+v", sessionFirst, row)
		}
	}

	// The rebind target re-seats onto the merged row — its retry expects the
	// pair the grafted registry fields carry.
	p := NewProjectPickerOverlay([]Project{
		{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha", RegistryCheckoutID: "chk_old"},
		{Name: "sessions", Root: "/repos/shared", RepoID: "repo-shared", SessionCount: 3},
	}, "/old/alpha")
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/candidate")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := p.TakeRebindRequest(); !ok {
		t.Fatalf("the submitted rebind should reach the caller")
	}
	p.SetRebindConflictPreserving("Rebound elsewhere, to /repos/shared · Enter retries from there", []Project{
		{Name: "alpha", Root: "/repos/shared", RepoID: "repo-shared", RegistryID: "prj_aaa", RegistryRoot: "/repos/shared", RegistryCheckoutID: "chk_new"},
	}, false)
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	p.HandleKeyPress(keyRune('b'))
	typeRunes(p, "/final")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	req, ok := p.TakeRebindRequest()
	if !ok || req.Project.RegistryID != "prj_aaa" || req.Project.RegistryRoot != "/repos/shared" || req.Project.RegistryCheckoutID != "chk_new" {
		t.Fatalf("the retry must expect the pair the merged row carries, got %+v (ok=%v)", req.Project, ok)
	}
}
