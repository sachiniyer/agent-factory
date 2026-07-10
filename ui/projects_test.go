package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testProjectRows() []SidebarProject {
	return []SidebarProject{
		{Name: "agent-factory", Root: "/repos/agent-factory", SessionCount: 3, Active: true},
		{Name: "website", Root: "/repos/website", SessionCount: 1},
		{Name: "infra", Root: "/repos/infra", SessionCount: 0},
	}
}

func newTestProjects(rows []SidebarProject) *ProjectsPane {
	p := NewProjectsPane()
	p.SetProjects(rows)
	return p
}

// TestProjectsPaneRendersHeaderAndRows: the section renders its "Projects (N)"
// header and one row per project, each row carrying the display name and its
// session count.
func TestProjectsPaneRendersHeaderAndRows(t *testing.T) {
	p := newTestProjects(testProjectRows())
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 30, H: 8})

	out := p.String()
	assert.Contains(t, out, "Projects (3)", "header names the project count")
	assert.Contains(t, out, "agent-factory (3)")
	assert.Contains(t, out, "website (1)")
	assert.Contains(t, out, "infra (0)")
}

// TestProjectsPaneMarksActiveProject: the active (scoped-to) project's row leads
// with the "●" accent marker; the others do not.
func TestProjectsPaneMarksActiveProject(t *testing.T) {
	p := newTestProjects(testProjectRows())
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 30, H: 8})

	lines := strings.Split(p.String(), "\n")
	var activeLine, inactiveLine string
	for _, l := range lines {
		if strings.Contains(l, "agent-factory") {
			activeLine = l
		}
		if strings.Contains(l, "website") {
			inactiveLine = l
		}
	}
	require.NotEmpty(t, activeLine)
	require.NotEmpty(t, inactiveLine)
	assert.Contains(t, activeLine, "●", "the active project row is marked")
	assert.NotContains(t, inactiveLine, "●", "inactive project rows are unmarked")
}

// TestProjectsPaneCursorNavAndSelection: while focused, up/down move the cursor
// and SelectedProject resolves the row under it.
func TestProjectsPaneCursorNavAndSelection(t *testing.T) {
	p := newTestProjects(testProjectRows())
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 30, H: 8})
	p.Focus()

	sel, ok := p.SelectedProject()
	require.True(t, ok)
	assert.Equal(t, "/repos/agent-factory", sel.Root, "cursor starts on the first row")

	_, consumed := p.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	require.True(t, consumed)
	_, consumed = p.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	require.True(t, consumed)
	sel, ok = p.SelectedProject()
	require.True(t, ok)
	assert.Equal(t, "/repos/infra", sel.Root, "Down twice lands on the third row")

	_, consumed = p.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	require.True(t, consumed)
	sel, _ = p.SelectedProject()
	assert.Equal(t, "/repos/website", sel.Root, "Up steps back to the second row")
}

// TestProjectsPaneBlurredHasNoCursor: an unfocused section consumes no cursor
// keys — they must bubble to the root's global bindings.
func TestProjectsPaneBlurredHasNoCursor(t *testing.T) {
	p := newTestProjects(testProjectRows())
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 30, H: 8})
	p.Blur()
	_, consumed := p.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	assert.False(t, consumed, "a blurred section must not swallow nav keys")
}

// TestProjectsPaneRegistersZones: the section registers a background zone plus a
// per-row zone keyed by repo root, so clicks resolve to focus/switch actions.
func TestProjectsPaneRegistersZones(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Reset()
	p := newTestProjects(testProjectRows())
	p.SetZoneRegistry(reg)
	p.SetRect(layout.Rect{X: 0, Y: 10, W: 30, H: 8})
	_ = p.String()

	// The background zone covers the section.
	bgID, _, ok := reg.Resolve(1, 10)
	require.True(t, ok)
	assert.Equal(t, zones.ProjectsBG, bgID, "section background zone")
	// A row zone resolves back to its repo root.
	rowID, _, ok := reg.Resolve(1, 11)
	require.True(t, ok)
	root, ok := zones.ProjectRoot(rowID)
	require.True(t, ok, "row zone id parses to a repo root")
	assert.Equal(t, "/repos/agent-factory", root)
}

// TestProjectsPaneCompactSummary: the degraded 1-line mode renders the project
// count only, still fitting its rect.
func TestProjectsPaneCompactSummary(t *testing.T) {
	p := newTestProjects(testProjectRows())
	p.SetCompact(true)
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 40, H: 2})
	out := p.String()
	assert.Contains(t, out, "Projects: 3", "compact mode shows the count summary")
}

// TestProjectsPaneEmptyShowsPlaceholder: with no other projects the section
// still renders its header (so it can be Tabbed into) plus a placeholder line.
func TestProjectsPaneEmptyShowsPlaceholder(t *testing.T) {
	p := newTestProjects(nil)
	p.SetRect(layout.Rect{X: 0, Y: 0, W: 30, H: 5})
	out := p.String()
	assert.Contains(t, out, "Projects (0)")
	_, ok := p.SelectedProject()
	assert.False(t, ok, "an empty section resolves no selected project")
}
