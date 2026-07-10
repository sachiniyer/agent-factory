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

func typeRunes(p *ProjectPickerOverlay, s string) {
	for _, r := range s {
		if r == ' ' {
			p.HandleKeyPress(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func resultNames(p *ProjectPickerOverlay) []string {
	names := make([]string, len(p.results))
	for i, r := range p.results {
		names[i] = r.Name
	}
	return names
}

func TestProjectPickerCurrentRootPreselected(t *testing.T) {
	p := pickerFixture()
	// widgets sorts after agent-factory/afterburner; results are sorted by name:
	// afterburner(0), agent-factory(1), widgets(2). currentRoot=/repos/widgets.
	proj, ok := p.selectedProjectForTest()
	if !ok || proj.Root != "/repos/widgets" {
		t.Fatalf("expected the current project preselected, got %+v (ok=%v)", proj, ok)
	}
}

func TestProjectPickerFuzzyAndSubstringFilter(t *testing.T) {
	p := pickerFixture()
	// "af" is a subsequence of both "agent-factory" and "afterburner" but not
	// "widgets".
	typeRunes(p, "af")
	got := resultNames(p)
	if len(got) != 2 {
		t.Fatalf("filter 'af' = %v, want 2 matches", got)
	}
	for _, n := range got {
		if n == "widgets" {
			t.Fatalf("filter 'af' should not match widgets: %v", got)
		}
	}
}

func TestProjectPickerFilterMatchesPath(t *testing.T) {
	p := pickerFixture()
	typeRunes(p, "repos/widg")
	if got := resultNames(p); len(got) != 1 || got[0] != "widgets" {
		t.Fatalf("path filter = %v, want [widgets]", got)
	}
}

func TestProjectPickerAddRowAlwaysNavigableAndLast(t *testing.T) {
	p := pickerFixture()
	// Filter to a single project: rows are [widgets, +Add]. The add row is the
	// last navigable index and is always reachable, even when the filter empties
	// the project list.
	typeRunes(p, "zzz-no-match")
	if len(p.results) != 0 {
		t.Fatalf("expected no project matches, got %v", resultNames(p))
	}
	if p.rowCount() != 1 {
		t.Fatalf("add row must remain navigable with an empty filter; rowCount=%d", p.rowCount())
	}
	if !p.addRowSelected() {
		t.Fatalf("with no project matches the cursor should land on the add row")
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

func TestProjectPickerRenderShowsCounts(t *testing.T) {
	p := pickerFixture()
	p.SetMaxSize(80, 24)
	out := p.Render()
	if !strings.Contains(out, "agent-factory") || !strings.Contains(out, "(12)") {
		t.Fatalf("render should show project names and session counts; got:\n%s", out)
	}
	if !strings.Contains(out, "Add project") {
		t.Fatalf("render should show the add-project affordance; got:\n%s", out)
	}
}

// selectedProjectForTest returns the row under the cursor as a Project without
// requiring submission, for assertions on the initial highlight.
func (p *ProjectPickerOverlay) selectedProjectForTest() (Project, bool) {
	if p.selectedIdx >= 0 && p.selectedIdx < len(p.results) {
		return p.results[p.selectedIdx], true
	}
	return Project{}, false
}
