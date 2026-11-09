package ui

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
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
	// Hidden marks a repo the daemon skipped at startup (#4742): its sessions
	// are hidden, not absent, so the row's zero count is incomplete. The
	// section marks it "· hidden" rather than presenting it as a clean
	// "no sessions" — the same incompleteness channel the CLI and web surface
	// for a skipped repo.
	Hidden bool
	// SkippedHint is the reason-specific one-line repair hint for a Hidden
	// row (#4742 review): the Projects-section D refusal names it directly so
	// a skipped project the operator highlighted receives the applicable
	// remedy instead of a pointer at the status bar, which may hold no skip
	// notice for an inactive project. Empty when the row is not skipped.
	SkippedHint string
}

// projectsTitleStyle / projectsTitleDimStyle paint the section header — the
// underline when the section holds focus, plain ink when it does not —
// mirroring the Automations header so the two bottom sections read as peers.
var projectsTitleStyle = lipgloss.NewStyle().
	Bold(true).Underline(true).
	Foreground(activeTheme.Ink)

var projectsTitleDimStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.Ink)

var projectsHintStyle = lipgloss.NewStyle().
	Foreground(activeTheme.InkMuted)

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
	// before the swap. A synthesized skipped row (#4742) carries only a RepoID
	// and an empty Root, so the root match below would miss it and the cursor
	// would fall back to the numeric index — sliding onto an unrelated healthy
	// row when a poll inserts, removes, or reorders another project. Track
	// RepoID too so a synthesized row keeps a stable selection identity.
	oldIndex := p.selected
	var selectedRoot string
	var selectedRepoID string
	if oldIndex >= 0 && oldIndex < len(p.projects) {
		selectedRoot = p.projects[oldIndex].Root
		selectedRepoID = p.projects[oldIndex].RepoID
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
	if p.selected < 0 && selectedRepoID != "" {
		// The selected row had no Root (a synthesized skipped row): re-locate
		// it by RepoID so a refresh that reorders the list keeps the cursor on
		// the same skipped repo instead of sliding onto a neighbor (#4742
		// review).
		for i, proj := range projects {
			if proj.RepoID == selectedRepoID {
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

// Degraded reports an unavailable registry, not an empty one.
func (p *ProjectsPane) Degraded() bool { return p.degraded }

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
// A synthesized skipped row carries only a RepoID and an empty Root (#4742); its
// zone is keyed by RepoID so two such rows get distinct mouse identities. Select
// by Root for ordinary rows, then fall back to RepoID for synthesized rows whose
// Root is empty.
func (p *ProjectsPane) SelectByRoot(root string) bool {
	if root != "" {
		for i, proj := range p.projects {
			if proj.Root == root {
				p.selected = i
				return true
			}
		}
	}
	for i, proj := range p.projects {
		if proj.Root == "" && proj.RepoID != "" && proj.RepoID == root {
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
//
// It reads KeySwitchProjectRow — "the Projects-section display alias of Enter",
// which exists for exactly this — off the generated binding table rather than
// spelling the key out, so a [keys] rebind surfaces here as it does everywhere
// else (#1026). The literal "enter switch" it used to render disagreed with the
// bottom menu, which has always drawn the same action from the same binding as
// "↵ switch" (ui/menu.go projectsMenuOptions) — the header and the footer named
// one action two ways in one frame, which is the #2580 failure mode.
func projectsSwitchHint() string { return railActionHint(keys.KeySwitchProjectRow, "switch") }

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

// projectRow renders one project row: "▹ name (N)" for the active project (the
// "▹" marker in the accent color), "  name (N)" for the rest. The focused
// cursor row paints on the selection background so the current pick stands out.
func (p *ProjectsPane) projectRow(proj SidebarProject, selected bool) string {
	marker := "  "
	if proj.Active {
		marker = projectRowActiveStyle.Render("▹") + " "
	}
	name := proj.Name
	if name == "" {
		name = "(unnamed)"
	}
	label := fmt.Sprintf("%s (%d)", name, proj.SessionCount)
	// A skipped repo's zero count is hidden, not absent (#4742): mark the row
	// so the section does not present it as a clean "no sessions". Reserve
	// space for the marker BEFORE truncating so a long name or a narrow rail
	// truncates the name (with an ellipsis) instead of eating the only
	// proactive indication that the count is incomplete (#4742 review).
	hiddenMarker := ""
	if proj.Hidden {
		hiddenMarker = " · hidden"
	}
	avail := p.rect.W - 2 - layout.Cells(hiddenMarker)
	if avail < 0 {
		avail = 0
	}
	label = fitLine(label, avail) + hiddenMarker
	if selected {
		cursor := "▸ "
		if proj.Active {
			cursor = "▹ "
		}
		return SelectionMarker(cursor) + projectRowSelectedStyle.Render(label)
	}
	if proj.Active {
		return marker + projectRowStyle.Bold(true).Render(label)
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
	// registered projects (#3298). The summary has no room for project rows,
	// so every project it counts is hidden below the header — the strip says
	// so with the rail's overflow vocabulary rather than leaving the header
	// as the last visible row while its rows are clipped (#6883). The marker
	// takes the #1560 margin's slot at the 2-row strip; at any taller compact
	// rect the margin still pads the bottom.
	if p.compact || p.rect.H <= 1 {
		style := projectsTitleDimStyle
		if p.focused {
			style = projectsTitleStyle
		}
		count := fmt.Sprintf("%d", len(p.projects))
		if p.degraded {
			count += "?"
		}
		lines := []string{p.titleLine(railHeader{noun: "Projects:", counts: count}, style)}
		if n := len(p.projects); n > 0 {
			lines = append(lines, railWindowIndicator("▼", n, p.rect.W-2))
		}
		return layout.ClampToRect(strings.Join(lines, "\n"), p.rect)
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
		lines = append(lines, projectsHintStyle.Foreground(activeTheme.Dead).Render(fitLine("  registry unreadable · list may be incomplete", p.rect.W)))
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
	// scrolls into view instead of moving invisibly, and mark the rows the
	// window hides on either side with the rail's "▲/▼ N more" vocabulary —
	// the same fit as the sessions rail's, so a clipped header is never the
	// last visible row while its rows go uncounted (#6883). The degraded
	// notice (or the empty-state row) above occupies one of the content rows.
	avail := contentH - len(lines)
	if avail < 0 {
		avail = 0
	}
	heights := make([]int, len(p.projects))
	for i := range heights {
		heights[i] = 1
	}

	start, end, hiddenAbove, hiddenBelow := 0, len(p.projects), 0, 0
	if avail > 0 && len(p.projects) > avail {
		// Scroll minimally so the selected row lands inside the rendered
		// window, then fill from the bottom so a tail that fits one row
		// earlier scrolls up rather than leaving dead space below the last
		// project (the sidebar's own scrolling policy).
		if p.offset > p.selected {
			p.offset = p.selected
		}
		if p.offset < 0 {
			p.offset = 0
		}
		for p.offset < p.selected {
			if e, _, _ := fitWindow(heights, p.offset, avail); p.selected < e {
				break
			}
			p.offset++
		}
		for p.offset > 0 {
			if e, _, _ := fitWindow(heights, p.offset-1, avail); e < len(p.projects) {
				break
			}
			p.offset--
		}
		start = p.offset
		var topInd, botInd bool
		end, topInd, botInd = fitWindow(heights, start, avail)
		// A force-included row can push the window's lines past the rect: at
		// the section's floor height the degraded notice plus both markers
		// leave one budgeted row, so an interior scroll is one line too many
		// and the ▼ marker is what the clamp would cut. The notice is
		// expendable where the count is not — drop it and refit so hidden
		// rows stay counted (#6883).
		if p.degraded {
			used := 0
			for i := start; i < end; i++ {
				used += heights[i]
			}
			indLines := 0
			if topInd {
				indLines++
			}
			if botInd {
				indLines++
			}
			if len(lines)+indLines+used > p.rect.H {
				lines = lines[:1]
				avail++
				end, _, _ = fitWindow(heights, start, avail)
			}
		}
		hiddenAbove = start
		hiddenBelow = len(p.projects) - end
	} else {
		p.offset = 0
	}

	if hiddenAbove > 0 {
		lines = append(lines, railWindowIndicator("▲", hiddenAbove, p.rect.W-2))
	}
	for i := start; i < end; i++ {
		selected := p.focused && i == p.selected
		rowStart := len(lines)
		lines = append(lines, p.projectRow(p.projects[i], selected))
		if p.zones != nil {
			// A synthesized skipped row (#4742) has an empty Root; key its
			// zone by RepoID so two such rows register under distinct ids
			// and a click resolves to the row the user clicked instead of
			// collapsing onto zones.Project("") and the first empty-Root
			// row (#4742 review).
			rowKey := p.projects[i].Root
			if rowKey == "" {
				rowKey = p.projects[i].RepoID
			}
			p.zones.Register(zones.Project(rowKey), layout.Rect{
				X: p.rect.X, Y: p.rect.Y + rowStart, W: p.rect.W, H: 1,
			})
		}
	}
	if hiddenBelow > 0 {
		lines = append(lines, railWindowIndicator("▼", hiddenBelow, p.rect.W-2))
	}
	return layout.ClampToRect(strings.Join(lines, "\n"), p.rect)
}
