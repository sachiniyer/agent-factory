package ui

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// SidebarProject is one row of the Projects section: a repo af has seen, with
// its display name (repo basename), absolute main-worktree root, tracked
// session count, and whether it is the project the rail is currently scoped to.
// The app derives these from the same discovery the ctrl+p picker uses
// (buildProjectList) and pushes them via ProjectsPane.SetProjects.
type SidebarProject struct {
	Name         string
	Root         string
	SessionCount int
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
// (launch, project switch). The cursor is clamped into the new range so a
// shorter list never leaves it dangling past the end.
func (p *ProjectsPane) SetProjects(projects []SidebarProject) {
	p.projects = projects
	if p.selected >= len(projects) {
		p.selected = len(projects) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
}

// Projects returns the current row list (test/inspection helper).
func (p *ProjectsPane) Projects() []SidebarProject { return p.projects }

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

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 R4): the section/project-row zones registered by String() resolve
// to focus/switch actions there, so the pane-local fallback consumes nothing.
func (p *ProjectsPane) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// SelectedProject returns the row under the cursor, or false when the section
// holds no projects — the row Enter switches to.
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
func projectsSwitchHint() string { return "· enter switch" }

// titleLine renders the section header width-aware: the switch affordance is the
// last thing cut, so the key to switch stays visible even at the 22-col rail
// minimum, exactly as the Automations header keeps its manage affordance.
func (p *ProjectsPane) titleLine(name string, nameStyle lipgloss.Style) string {
	w := p.rect.W
	const shortName = " Projects "
	hint := " " + projectsSwitchHint() + " "
	if runewidth.StringWidth(name+hint) <= w {
		return nameStyle.Render(name) + projectsHintStyle.Render(hint)
	}
	if runewidth.StringWidth(shortName+hint) <= w {
		return nameStyle.Render(shortName) + projectsHintStyle.Render(hint)
	}
	return nameStyle.Render(fitLine(name, w))
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

	// 1-line degraded summary (mirrors the Automations compact mode).
	if p.compact || p.rect.H <= 1 {
		style := projectsTitleDimStyle
		if p.focused {
			style = projectsTitleStyle
		}
		return layout.ClampToRect(
			p.titleLine(fmt.Sprintf(" Projects: %d ", len(p.projects)), style), p.rect)
	}

	nameStyle := projectsTitleDimStyle
	if p.focused {
		nameStyle = projectsTitleStyle
	}
	title := p.titleLine(fmt.Sprintf(" Projects (%d) ", len(p.projects)), nameStyle)
	lines := []string{title}
	if len(p.projects) == 0 {
		lines = append(lines, projectRowStyle.Render(fitLine("  no other projects yet", p.rect.W)))
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
	// scrolls into view instead of moving invisibly.
	visible := contentH - 1
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
