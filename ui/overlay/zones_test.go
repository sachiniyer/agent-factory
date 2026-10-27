package overlay

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

func keyEnter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

// ----------------------------------------------------------------------------
// Overlay button zones (#1024 R4): clicking the confirmation's y/n words or a
// selection/search row is equivalent to the key. The zones are derived by
// scanning the rendered output, and these tests verify each zone lands on the
// rendered text it stands for.
// ----------------------------------------------------------------------------

// cellSliceAt returns the w terminal cells starting at absolute column x of a
// rendered line, given the overlay was registered at origin.
func cellSliceAt(line string, x, w int) string {
	plain := xansi.Strip(line)
	// All confirm/selection glyphs are single-cell, so walk runes by width.
	col, start := 0, -1
	var out []rune
	for i, r := range []rune(plain) {
		if col == x && start < 0 {
			start = i
		}
		if start >= 0 && col < x+w {
			out = append(out, r)
		}
		col += runewidth.RuneWidth(r)
	}
	return string(out)
}

func TestConfirmationOverlayRegistersYesNoZones(t *testing.T) {
	c := NewConfirmationOverlay("[!] Kill session 'alpha'?")
	c.SetWidth(50)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 12, Y: 5}
	c.RegisterZones(reg, origin)

	lines := strings.Split(c.Render(), "\n")

	yes, ok := reg.Find(zones.OverlayConfirmYes)
	require.True(t, ok, "yes zone; got %v", reg.IDs())
	no, ok := reg.Find(zones.OverlayConfirmNo)
	require.True(t, ok, "no zone")

	assert.Equal(t, yes.Y, no.Y, "both buttons sit on the instruction line")
	line := lines[yes.Y-origin.Y]
	assert.Equal(t, "y/enter confirm", cellSliceAt(line, yes.X-origin.X, yes.W),
		"the yes zone covers exactly its rendered words, including the enter alias (#2405)")
	assert.Equal(t, "n/esc cancel", cellSliceAt(line, no.X-origin.X, no.W),
		"the no zone covers exactly its rendered words")

	// Resolve precedence sanity: a click on each zone resolves to it.
	id, _, ok := reg.Resolve(yes.X, yes.Y)
	require.True(t, ok)
	assert.Equal(t, zones.OverlayConfirmYes, id)
	id, _, ok = reg.Resolve(no.X+2, no.Y)
	require.True(t, ok)
	assert.Equal(t, zones.OverlayConfirmNo, id)
}

// TestConfirmationOverlayZonesFollowCustomKeys: overlays with a custom
// confirm key register the zone over the custom instruction text.
func TestConfirmationOverlayZonesFollowCustomKeys(t *testing.T) {
	c := NewConfirmationOverlay("proceed?")
	c.SetWidth(50)
	c.SetConfirmKey("d")
	reg := zones.NewRegistry()
	c.RegisterZones(reg, layout.Point{})

	yes, ok := reg.Find(zones.OverlayConfirmYes)
	require.True(t, ok)
	line := strings.Split(c.Render(), "\n")[yes.Y]
	assert.Equal(t, "d confirm", cellSliceAt(line, yes.X, yes.W))
}

func TestSelectionOverlayRegistersRowZones(t *testing.T) {
	items := []string{"claude", "aider", "codex"}
	s := NewSelectionOverlay("Select program", items)
	s.SetWidth(50)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 8, Y: 4}
	s.RegisterZones(reg, origin)

	lines := strings.Split(s.Render(), "\n")
	for i, item := range items {
		r, ok := reg.Find(zones.OverlaySelectRow(i))
		require.True(t, ok, "row zone for %q; got %v", item, reg.IDs())
		assert.Equal(t, origin.X, r.X, "row zones span the full overlay width")
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), item,
			"row %d's zone must sit on the line rendering %q", i, item)
	}
}

// A row too long for the overlay box renders truncated with an ellipsis. It
// still has to be clickable: matching the full item text registered no zone at
// all, so every picker whose labels are a sentence — the #4429 resolve picker
// is one — answered the keyboard and swallowed the mouse (#4528).
func TestSelectionOverlayRegistersTruncatedRowZones(t *testing.T) {
	items := []string{
		"Retry send — submit the pending mission again",
		"Mark delivered — retire the pending mission without resending (the pane already shows it landed)",
	}
	s := NewSelectionOverlay("Resolve delivery for 'alpha' — inspect the pane first", items)
	s.SetWidth(50)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 8, Y: 4}
	s.RegisterZones(reg, origin)

	lines := strings.Split(s.Render(), "\n")
	for i := range items {
		r, ok := reg.Find(zones.OverlaySelectRow(i))
		require.True(t, ok, "row zone for item %d; got %v", i, reg.IDs())
		line := xansi.Strip(lines[r.Y-origin.Y])
		require.Contains(t, line, "…", "fixture: row %d must render truncated", i)
		head := strings.TrimSuffix(strings.Trim(line, "│ ▸"), "…")
		assert.True(t, strings.HasPrefix(items[i], head),
			"row %d's zone must sit on the line rendering its own item: %q", i, line)
	}
}

func TestSelectionOverlayRegistersBudgetedWindowZones(t *testing.T) {
	items := []string{"claude", "aider", "codex"}
	s := NewSelectionOverlay("Select program", items)
	s.SetMaxSize(50, 8)
	s.SetSelectedIndex(1)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 8, Y: 4}
	s.RegisterZones(reg, origin)

	r, ok := reg.Find(zones.OverlaySelectRow(1))
	require.True(t, ok, "zone for the visible budgeted row; got %v", reg.IDs())
	line := strings.Split(s.Render(), "\n")[r.Y-origin.Y]
	assert.Contains(t, xansi.Strip(line), items[1])
	for _, hidden := range []int{0, 2} {
		_, ok := reg.Find(zones.OverlaySelectRow(hidden))
		assert.False(t, ok, "hidden item %d must not have a zone", hidden)
	}
}

func TestSearchOverlayRegistersRowZonesAndSetSelectedIndex(t *testing.T) {
	instances := []*session.Instance{
		{Title: "alpha"},
		{Title: "alpha-2"},
		{Title: "beta"},
	}
	s := NewSearchOverlay(instances)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 10, Y: 3}
	s.RegisterZones(reg, origin)

	lines := strings.Split(s.Render(), "\n")
	for i, inst := range instances {
		r, ok := reg.Find(zones.OverlaySearchRow(i))
		require.True(t, ok, "row zone for %q; got %v", inst.Title, reg.IDs())
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), inst.Title,
			"result %d's zone must sit on the line rendering %q", i, inst.Title)
	}
	// "alpha" is a prefix of "alpha-2": the ordered scan must not have bound
	// both zones to the same line.
	r0, _ := reg.Find(zones.OverlaySearchRow(0))
	r1, _ := reg.Find(zones.OverlaySearchRow(1))
	assert.NotEqual(t, r0.Y, r1.Y, "prefix-colliding titles must map to distinct rows")

	// SetSelectedIndex is the click primitive: it moves the selection the
	// subsequent enter submits.
	s.SetSelectedIndex(2)
	require.True(t, s.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[2], s.GetSelectedInstance())

	// Out-of-range clicks are refused.
	s2 := NewSearchOverlay(instances)
	s2.SetSelectedIndex(99)
	require.True(t, s2.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[0], s2.GetSelectedInstance())
}

func TestSearchOverlayRegistersLimitReachedRowZone(t *testing.T) {
	inst := &session.Instance{Title: "blocked-session"}
	_ = inst.Transition(session.ObserveLiveness(session.LiveLimitReached))
	s := NewSearchOverlay([]*session.Instance{inst})
	reg := zones.NewRegistry()

	s.RegisterZones(reg, layout.Point{})

	r, ok := reg.Find(zones.OverlaySearchRow(0))
	require.True(t, ok, "limit-reached search rows must stay mouse-clickable")
	line := strings.Split(s.Render(), "\n")[r.Y]
	assert.Contains(t, xansi.Strip(line), "◆", "test must exercise the limit glyph")
	assert.Contains(t, xansi.Strip(line), inst.Title)
}

// TestSearchOverlayScrolledWindowRegistersVisibleRows pins the Greptile P1 on
// the original mouse PR (#1086): once the selection scrolls past the first
// page, Render windows the results — and the registered zones must be the
// rows actually on screen, keyed by their FULL-LIST indices, instead of
// matching the visible rows against results[0] and registering nothing.
func TestSearchOverlayScrolledWindowRegistersVisibleRows(t *testing.T) {
	var instances []*session.Instance
	for i := 1; i <= 12; i++ {
		instances = append(instances, &session.Instance{Title: sessionTitle(i)})
	}
	s := NewSearchOverlay(instances)
	// Walk the selection to the last result: the visible window is now
	// results[2..12).
	for range instances {
		_ = s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	reg := zones.NewRegistry()
	origin := layout.Point{X: 5, Y: 2}
	s.RegisterZones(reg, origin)

	_, hasFirst := reg.Find(zones.OverlaySearchRow(0))
	assert.False(t, hasFirst, "rows scrolled above the window must not register")

	lines := strings.Split(s.Render(), "\n")
	for i := 2; i < 12; i++ {
		r, ok := reg.Find(zones.OverlaySearchRow(i))
		require.True(t, ok, "visible row %d must register; got %v", i, reg.IDs())
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), sessionTitle(i+1),
			"row %d's zone must sit on the line rendering it", i)
	}

	// The click primitive round-trips: clicking the row registered for index
	// 5 selects session-06.
	s.SetSelectedIndex(5)
	require.True(t, s.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[5], s.GetSelectedInstance())
}

func sessionTitle(i int) string {
	return "session-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestConfirmationOverlayPendingRegistersOnlyCancel: while pending (#4848) the
// hint carries no confirm words, so no yes zone may exist for a click to hit;
// the cancel zone stays.
func TestConfirmationOverlayPendingRegistersOnlyCancel(t *testing.T) {
	c := NewConfirmationOverlay("Delete session 'alpha'?")
	c.SetWidth(60)
	c.SetPending("Checking for unsaved work…")
	reg := zones.NewRegistry()
	c.RegisterZones(reg, layout.Point{})

	_, ok := reg.Find(zones.OverlayConfirmYes)
	assert.False(t, ok, "a pending dialog must not register a confirm zone")
	no, ok := reg.Find(zones.OverlayConfirmNo)
	require.True(t, ok, "the cancel zone stays clickable")
	line := strings.Split(c.Render(), "\n")[no.Y]
	assert.Equal(t, "n/esc cancel", cellSliceAt(line, no.X, no.W))
}

// TestProjectPickerRegistersRowZones pins the #1461 mouse fix: the project
// picker is the one keyboard-navigable list overlay built as its own widget
// rather than reusing *SelectionOverlay, so it shipped without the
// RegisterZones call every sibling picker has — and a left click on any row
// fell through handleModalClick to a silent swallow. It now registers one
// full-width zone per visible project row (and the trailing "+ Add project…"
// row), each sitting on the exact line Render painted it on.
func TestProjectPickerRegistersRowZones(t *testing.T) {
	p := pickerFixture() // afterburner, agent-factory, widgets; cursor on widgets
	p.SetMaxSize(80, 24)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 8, Y: 4}
	p.RegisterZones(reg, origin)

	lines := strings.Split(p.Render(), "\n")
	// All four navigable rows (three projects + the add row) are visible at
	// this size, so each must have a zone on the line that renders it.
	for i := 0; i < p.rowCount(); i++ {
		r, ok := reg.Find(zones.OverlaySelectRow(i))
		require.True(t, ok, "row %d must register a zone; got %v", i, reg.IDs())
		assert.Equal(t, origin.X, r.X, "row %d zone spans the full overlay width", i)
		line := xansi.Strip(lines[r.Y-origin.Y])
		if i == len(p.all) {
			assert.Contains(t, line, "Add project", "the add row's zone sits on its rendered line")
		} else {
			assert.Contains(t, line, p.all[i].Name, "row %d's zone sits on the line rendering %q", i, p.all[i].Name)
		}
	}
}

// TestProjectPickerRegistersBudgetedWindowZones: once the list windows, only the
// visible rows register (keyed by their FULL-LIST indices), exactly like the
// sibling *SelectionOverlay — a click cannot resolve to a row scrolled off the
// frame, and cannot resolve to the chrome the windowed rows were replaced with.
func TestProjectPickerRegistersBudgetedWindowZones(t *testing.T) {
	projects := make([]Project, 12)
	for i := range projects {
		projects[i] = Project{Name: fmt.Sprintf("p%02d", i), Root: fmt.Sprintf("/repos/p%02d", i)}
	}
	p := NewProjectPickerOverlay(projects, "/repos/p05") // cursor on p05
	p.SetMaxSize(60, 10)
	reg := zones.NewRegistry()
	p.RegisterZones(reg, layout.Point{})

	// The cursor (p05) and a window around it are visible; rows far from it
	// are not. The add row (index == len(projects)) is scrolled off the top
	// of the window and must NOT register.
	for _, hidden := range []int{0, 11, len(projects)} {
		_, ok := reg.Find(zones.OverlaySelectRow(hidden))
		assert.False(t, ok, "off-window row %d must not register a zone; got %v", hidden, reg.IDs())
	}
	// The cursor's own row is visible and registered.
	r, ok := reg.Find(zones.OverlaySelectRow(5))
	require.True(t, ok, "the cursor's row must register; got %v", reg.IDs())
	lines := strings.Split(p.Render(), "\n")
	assert.Contains(t, xansi.Strip(lines[r.Y]), "p05", "the cursor row's zone sits on its rendered line")
}

// TestProjectPickerRegisterZonesEmptyInFormModes: the add and rebind path-input
// forms have no clickable rows — their only interactive surface is the text
// field, which a row click could not submit — so they register nothing and a
// stray click on the form stays swallowed, exactly as it already does in
// keyboard mode (the keyboard drives the input, not a row).
func TestProjectPickerRegisterZonesEmptyInFormModes(t *testing.T) {
	p := pickerFixture()
	p.SetMaxSize(80, 24)
	// Enter add mode: list-mode Enter on the add row drops into the form.
	for range [5]struct{}{} {
		p.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	p.HandleKeyPress(keyEnter())
	require.True(t, p.adding)

	reg := zones.NewRegistry()
	p.RegisterZones(reg, layout.Point{})
	assert.Empty(t, reg.IDs(), "add mode must register no row zones")

	// Rebind mode is the same: a registry-backed row, then `b`.
	p2 := NewProjectPickerOverlay([]Project{
		{Name: "gone", Root: "/old/gone", RegistryID: "prj_z", MissingPath: true},
	}, "")
	p2.SetMaxSize(80, 24)
	p2.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	require.True(t, p2.rebinding)
	reg2 := zones.NewRegistry()
	p2.RegisterZones(reg2, layout.Point{})
	assert.Empty(t, reg2.IDs(), "rebind mode must register no row zones")
}

// TestProjectPickerSetSelectedIndexIsClickPrimitive: SetSelectedIndex is the
// click primitive the sibling *SelectionOverlay exposes — a row index moves the
// cursor there and the subsequent Enter submits it, and out-of-range indices
// no-op so a malformed/stale zone cannot move the cursor off the list. The
// add-row index is a valid target: it drops into add mode, mirroring the
// keyboard.
func TestProjectPickerSetSelectedIndexIsClickPrimitive(t *testing.T) {
	p := pickerFixture() // cursor on widgets (idx 2)
	p.SetSelectedIndex(0)
	require.Equal(t, 0, p.selectedIdx, "SetSelectedIndex moves the cursor onto the row")
	require.True(t, p.HandleKeyPress(keyEnter()), "Enter on a project row submits and closes")
	proj, ok := p.SelectedProject()
	require.True(t, ok)
	assert.Equal(t, "afterburner", proj.Name, "the clicked row's project is chosen")

	// Clicking the add row enters add mode rather than switching.
	p2 := pickerFixture()
	p2.SetSelectedIndex(p2.rowCount() - 1) // the add row
	require.True(t, p2.addRowSelected())
	require.False(t, p2.HandleKeyPress(keyEnter()), "Enter on the add row enters add mode, not closes")
	require.True(t, p2.adding, "the add row's click primitive drops into add mode")

	// Out-of-range indices are refused — a stale zone id off the list cannot
	// move the cursor.
	p3 := pickerFixture()
	before := p3.selectedIdx
	p3.SetSelectedIndex(-1)
	p3.SetSelectedIndex(p3.rowCount()) // == len(all)+1, past the add row
	p3.SetSelectedIndex(999)
	assert.Equal(t, before, p3.selectedIdx, "out-of-range indices must not move the cursor")
}
