package overlay

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	out := p.Render()
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
	out := p.Render()
	if !strings.Contains(out, "agent-factory") || !strings.Contains(out, "(12)") {
		t.Fatalf("render should show project names and session counts; got:\n%s", out)
	}
	if !strings.Contains(out, "Add project") {
		t.Fatalf("render should show the add-project affordance; got:\n%s", out)
	}
	// The footer advertises rail-style navigation, not search.
	if !strings.Contains(out, "navigate") || !strings.Contains(out, "switch") {
		t.Fatalf("render should show the j/k navigate hint; got:\n%s", out)
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
