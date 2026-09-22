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
	target, path, ok := p.TakeRebindRequest()
	if !ok || path != "/new/moved" || target.RegistryID != "prj_aaa" {
		t.Fatalf("TakeRebindRequest = (%+v, %q, %v), want the registry row + typed path", target, path, ok)
	}
	if _, _, ok := p.TakeRebindRequest(); ok {
		t.Fatalf("TakeRebindRequest should only fire once")
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

	target, path, ok := p.TakeRebindRequest()
	if !ok || path != "/first/path" || target.RegistryID != "prj_p1" {
		t.Fatalf("TakeRebindRequest = (%+v, %q, %v), want the registry row + typed path", target, path, ok)
	}

	// The daemon is still deciding: a second Enter must NOT submit again, and
	// edits must not change the input the pending answer refers to.
	typeRunes(p, "/second/path")
	p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if _, _, ok := p.TakeRebindRequest(); ok {
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
	_, path, ok = p.TakeRebindRequest()
	if !ok || path != "/first/path-fixed" {
		t.Fatalf("after a rejection the corrected path should resubmit; got (%q, %v)", path, ok)
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
	if _, _, ok := p.TakeRebindRequest(); ok {
		t.Fatalf("a denied rebind must never produce a request")
	}
	if p.rebindErr != deny {
		t.Fatalf("Enter should re-show the refusal, got %q", p.rebindErr)
	}
}
