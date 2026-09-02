package ui

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SidebarProject is one row of the Projects section: a repo af has seen, with
// its display name (repo basename), absolute main-worktree root, tracked
// session count, and whether it is the project the rail is currently scoped to.
// The app derives these from the same discovery the ctrl+p picker uses
// (buildProjectList) and pushes them via ProjectsPane.SetProjects.
type SidebarProject struct {
	// RepoID is the identity established while aggregating the row. Destructive
	// actions must carry it rather than infer identity again from Root.
	RepoID       string
	Name         string
	Root         string
	SessionCount int
	// InPlaceCount is how many of SessionCount's live sessions sit on an
	// in-place/external worktree, which delete-project tears down rather than
	// archives (#1973). Carried so the delete confirmation can state the real
	// split; the row label itself renders only the total.
	InPlaceCount int
	Active       bool
}

// projectsTitleStyle / projectsTitleDimStyle paint the section header — the
// accent when the section holds focus, the muted foreground when it does not —
// mirroring the Automations header so the two bottom sections read as peers.
var projectsTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(AccentColor)

var projectsTitleDimStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.ForegroundMuted)

var projectsHintStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundDim)

// projectRowStyle / projectRowActiveStyle / projectRowSelectedStyle render the
// Projects rows: the plain row, the active (scoped-to) project's accent marker,
// and the cursor-selected row (selection background wins over the active
// accent). Assigned in applyThemeStyles so a theme switch re-tints them.
var projectRowStyle lipgloss.Style
var projectRowActiveStyle lipgloss.Style
var projectRowSelectedStyle lipgloss.Style

// ProjectsPane is the bottom-most section of the left rail (#1588 follow-up):
// one row per project af has seen — the active project marked with a "●" accent
// marker — pinned BELOW the Automations section, under its own horizontal rule.
// It is a peer of AutomationsPane, not a row-group inside the instances tree:
// the focus ring cycles tree → panes → automations → projects → tree, so the
// user Tabs INTO it and picks a project to switch (reusing the #1547
// switchProject path). The rows render off a list the app pushes (SetProjects)
// from the same cross-repo discovery the ctrl+p picker uses, so the section and
// the picker can never show different project sets.
//
// It implements layout.Pane. While focused the rows carry a cursor (up/down/j/k
// via HandleKey); the root switches the rail on Enter (SelectedProject) and
// moves the focus ring on Esc.
type ProjectsPane struct {
	projects []SidebarProject
	// degraded marks a failed project-registry read (#3298): the list still
	// renders from the other discovery sources, but it may be missing every
	// registered sessionless project, and presenting it as complete would be
	// a failed read shown as an empty result.
	degraded bool

	rect    layout.Rect
	compact bool
	focused bool

	// selected is the focused section's cursor over the project list (clamped on
	// every read); offset is the scroll window start so the cursor stays visible
	// in the few in-rail rows.
	selected int
	offset   int

	// zones is the shared mouse hit-test registry (#1024 R4); String() registers
	// the section plus its project rows every frame. Nil skips.
	zones *zones.Registry
}

// NewProjectsPane creates an empty Projects section; the app populates it with
// SetProjects.
func NewProjectsPane() *ProjectsPane {
	return &ProjectsPane{}
}

// SetProjects replaces the section's row list. The app pushes it from the same
// cross-repo discovery the ctrl+p picker uses, whenever the list can change
// (launch, project switch, and the background daemon-snapshot poll so the
// always-visible counts stay live). Reports whether the row list actually
// changed, so a background refresh only repaints on a real diff.
//
// The cursor tracks the selected project by IDENTITY (repo root), not by row
// index: the list is re-sorted by name on every rebuild, so a poll that inserts
// or removes a project ahead of the cursor would otherwise slide it onto a
// different project (and Enter would switch to the wrong one). After the swap we
// re-locate the previously-selected root; if it is gone (project removed), the
// cursor clamps to the row that took its place — the nearest sensible neighbor.
func (p *ProjectsPane) SetProjects(projects []SidebarProject) bool {
	changed := !reflect.DeepEqual(p.projects, projects)

	// Remember which project the cursor pointed at (by root), plus its old row,
	// before the swap.
	oldIndex := p.selected
	var selectedRoot string
	if oldIndex >= 0 && oldIndex < len(p.projects) {
		selectedRoot = p.projects[oldIndex].Root
	}

	p.projects = projects

	// Re-locate the same project by identity. If it is gone (removed), fall back
	// to the old row index clamped into the new list — the nearest name-neighbor
	// in the re-sorted list, not an arbitrary jump to the top.
	p.selected = -1
	if selectedRoot != "" {
		for i, proj := range projects {
			if proj.Root == selectedRoot {
				p.selected = i
				break
			}
		}
	}
	if p.selected < 0 {
		p.selected = oldIndex
	}
	if p.selected >= len(projects) {
		p.selected = len(projects) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
	return changed
}

// Projects returns the current row list (test/inspection helper).
func (p *ProjectsPane) Projects() []SidebarProject { return p.projects }

// SetDegraded records whether the project registry read failed (#3298), so
// the section says the list may be incomplete instead of presenting the
// remaining discovery sources as the whole world. Reports whether the flag
// changed, so a background refresh only repaints on a real diff.
func (p *ProjectsPane) SetDegraded(degraded bool) bool {
	changed := p.degraded != degraded
	p.degraded = degraded
	return changed
}

// SetRect implements layout.Pane.
func (p *ProjectsPane) SetRect(r layout.Rect) { p.rect = r }

// SetCompact selects the 1-line summary rendering (degradation ladder, mirrors
// the Automations section's compact mode).
func (p *ProjectsPane) SetCompact(compact bool) { p.compact = compact }

// Focused implements layout.Pane.
func (p *ProjectsPane) Focused() bool { return p.focused }

// Focus implements layout.Pane: the section shows a cursor over its rows.
func (p *ProjectsPane) Focus() { p.focused = true }

// Blur implements layout.Pane.
func (p *ProjectsPane) Blur() { p.focused = false }

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (p *ProjectsPane) SetZoneRegistry(reg *zones.Registry) { p.zones = reg }

// HandleKey implements layout.Pane: the focused section owns only its cursor
// movement; everything else (Enter → switch, Esc → focus ring) is root-routed
// so it stays in one place.
func (p *ProjectsPane) HandleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if !p.focused {
		return nil, false
	}
	switch msg.String() {
	case "up", "k":
		p.ScrollUp()
		return nil, true
	case "down", "j":
		p.ScrollDown()
		return nil, true
	}
	return nil, false
}

// SelectedProject returns the row under the cursor, or false when the section
// holds no projects — the row Enter switches to.
// HasProjects reports whether the section has any row to put a cursor on. With
// none, Enter is consumed as a no-op (SelectedProject reports false), so nothing
// may advertise Enter here and nothing should hand the section focus (#2830).
func (p *ProjectsPane) HasProjects() bool { return len(p.projects) > 0 }

func (p *ProjectsPane) SelectedProject() (SidebarProject, bool) {
	if len(p.projects) == 0 {
		return SidebarProject{}, false
	}
	sel := clampInt(p.selected, 0, len(p.projects)-1)
	return p.projects[sel], true
}

// SelectByRoot moves the cursor onto the project with the given repo root — the
// click action for a project row. Reports whether the project was found.
func (p *ProjectsPane) SelectByRoot(root string) bool {
	for i, proj := range p.projects {
		if proj.Root == root {
			p.selected = i
			return true
		}
	}
	return false
}

// ScrollUp moves the section cursor up (wheel/key routing).
func (p *ProjectsPane) ScrollUp() {
	if p.selected > 0 {
		p.selected--
	}
}

// ScrollDown moves the section cursor down.
func (p *ProjectsPane) ScrollDown() {
	if p.selected < len(p.projects)-1 {
		p.selected++
	}
}

// projectsSwitchHint is the affordance suffix on the section header: the key
// that switches to the cursor row, kept visible down to the rail minimum.
func projectsSwitchHint() string { return "enter switch" }

// titleLine renders the section header through the shared rail ladder
// (ui/rail_header.go). The switch affordance is hints[0], so it is the last
// thing cut — which is what this comment has always claimed and what the code
// did not do (#3642).
//
// The old ladder swapped in a count-free `" Projects "` short name at 25-28
// columns and then, below that, kept the whole name and dropped the hint
// instead. So the count was shown at 22-24, gone at 25-28, and back at 29+: a
// wider rail said LESS. The shared ladder shrinks the noun beside an intact
// count rather than swapping in a shorter name, which is what makes it
// monotonic.
func (p *ProjectsPane) titleLine(header railHeader, nameStyle lipgloss.Style) string {
	return railTitleLine(header, p.rect.W, nameStyle, projectsHintStyle,
		railHintSeparator+projectsSwitchHint(),
	)
}

// projectRow renders one project row: "● name (N)" for the active project (the
// "●" marker in the accent color), "  name (N)" for the rest. The focused
// cursor row paints on the selection background so the current pick stands out.
func (p *ProjectsPane) projectRow(proj SidebarProject, selected bool) string {
	marker := "  "
	if proj.Active {
		marker = projectRowActiveStyle.Render("●") + " "
	}
	name := proj.Name
	if name == "" {
		name = "(unnamed)"
	}
	label := fmt.Sprintf("%s (%d)", name, proj.SessionCount)
	// The marker occupies 2 cells; fit the label into the rest of the row.
	label = fitLine(label, p.rect.W-2)
	if selected {
		return marker + projectRowSelectedStyle.Render(label)
	}
	if proj.Active {
		return marker + projectRowActiveStyle.Render(label)
	}
	return marker + projectRowStyle.Render(label)
}

// View implements layout.Pane: exactly rect-sized.
func (p *ProjectsPane) View() string { return p.String() }

func (p *ProjectsPane) String() string {
	if p.rect.Empty() {
		return ""
	}
	// The section's base zone: any click inside it that lands on no project row
	// focuses the Projects region. Rows register on top (later wins).
	if p.zones != nil {
		p.zones.Register(zones.ProjectsBG, p.rect)
	}

	if p.selected >= len(p.projects) {
		p.selected = len(p.projects) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}

	// 1-line degraded summary (mirrors the Automations compact mode). A "?"
	// marks a count built over a failed registry read: it may be missing
	// registered projects (#3298).
	if p.compact || p.rect.H <= 1 {
		style := projectsTitleDimStyle
		if p.focused {
			style = projectsTitleStyle
		}
		count := fmt.Sprintf("%d", len(p.projects))
		if p.degraded {
			count += "?"
		}
		return layout.ClampToRect(
			p.titleLine(railHeader{noun: "Projects:", counts: count}, style), p.rect)
	}

	nameStyle := projectsTitleDimStyle
	if p.focused {
		nameStyle = projectsTitleStyle
	}
	title := p.titleLine(railHeader{
		noun:   "Projects",
		counts: fmt.Sprintf("(%d)", len(p.projects)),
	}, nameStyle)
	lines := []string{title}
	if p.degraded {
		// The registry read failed, so the rows below may be missing every
		// registered sessionless project — say so instead of rendering the
		// remaining sources as a complete list (#3298). This row also stands
		// in for the empty-state line: "No other projects yet" would be a
		// claim the failed read cannot support.
		lines = append(lines, projectsHintStyle.Render(fitLine("  registry unreadable · list may be incomplete", p.rect.W)))
	} else if len(p.projects) == 0 {
		lines = append(lines, projectRowStyle.Render(fitLine("  No other projects yet", p.rect.W)))
	}

	// Reserve the last rail row as a blank bottom margin so the workspace frame's
	// bottom border never abuts the section's last row (#1560) — the Projects
	// section is now the bottom-most rail region, so it carries the margin the
	// Automations section used to.
	contentH := p.rect.H
	if p.rect.H >= layout.ProjectsRows {
		contentH = p.rect.H - 1
	}

	// Window the rows around the cursor so a focused selection below the fold
	// scrolls into view instead of moving invisibly. The degraded notice above
	// occupies one of the content rows.
	visible := contentH - 1
	if p.degraded {
		visible--
	}
	if visible < 0 {
		visible = 0
	}
	if p.offset > p.selected {
		p.offset = p.selected
	}
	if visible > 0 && p.selected >= p.offset+visible {
		p.offset = p.selected - visible + 1
	}
	if max := len(p.projects) - visible; p.offset > max {
		p.offset = max
	}
	if p.offset < 0 {
		p.offset = 0
	}
	for i := p.offset; i < len(p.projects); i++ {
		if len(lines) >= contentH {
			break
		}
		selected := p.focused && i == p.selected
		rowStart := len(lines)
		lines = append(lines, p.projectRow(p.projects[i], selected))
		if p.zones != nil {
			p.zones.Register(zones.Project(p.projects[i].Root), layout.Rect{
				X: p.rect.X, Y: p.rect.Y + rowStart, W: p.rect.W, H: 1,
			})
		}
	}
	return layout.ClampToRect(strings.Join(lines, "\n"), p.rect)
}
