package layout

import (
	"fmt"
	"strings"
)

// Region identifiers for the named layout regions. These double as focus
// ring ids (§2.3) and as the pane component of zone ids (§2.5). Workspace
// panes get dynamic ids from PaneRegion — the N-pane model (#1088) has no
// fixed pane regions.
const (
	RegionTree         = "tree"
	RegionWorkspace    = "workspace"
	RegionRailRule     = "railRule"
	RegionAutomations  = "automations"
	RegionProjectsRule = "projectsRule"
	RegionProjects     = "projects"
	RegionStatusBar    = "status"
)

// PaneRegion returns the region/focus-ring id for a workspace pane. The app
// keys ring entries on the store's stable pane ids (so the ring survives
// panes opening and closing); layout keys VisibleRegions on position.
func PaneRegion(id int) string { return fmt.Sprintf("pane:%d", id) }

// DividerRegion returns the region id for the 1-col divider right of pane i.
func DividerRegion(i int) string { return fmt.Sprintf("divider:%d", i) }

// IsPaneRegion reports whether a region/focus-ring id names a workspace pane.
func IsPaneRegion(id string) bool { return strings.HasPrefix(id, "pane:") }

// Sizing constants and degradation-ladder thresholds (RFC §2.6). A feature
// named by a *Min* threshold is available at sizes >= the threshold and
// degrades below it.
const (
	// TreeMinWidth / TreeMaxWidth clamp the left rail: clamp(22, 25%·W, 36).
	// Narrowed from clamp(24, 30%·W, 44) by #1090 to give the content panes
	// more columns.
	TreeMinWidth = 22
	TreeMaxWidth = 36

	// TreeMinHeight is the floor the instances tree keeps inside the rail once
	// any bottom sections render (#1590). The tree is in the focus ring, so it
	// must stay usable — enough rows for its leading blank chrome, the Instances
	// header, and a few rows. The fixed bottom sections (automations, projects)
	// yield space to hold this floor rather than squeezing the tree to nothing.
	TreeMinHeight = 6

	// StatusBarRows is the fixed status-bar height.
	StatusBarRows = 2

	// AlarmBarRows is the height of the delivery-failure alarm banner (#1238),
	// reserved at the very top of the screen only when Grid.Banner is set. One
	// loud full-width row above everything so a dead delivery pipeline can't be
	// missed.
	AlarmBarRows = 1

	// RailRuleRows is the horizontal rule separating the instances tree from
	// the automations section inside the left rail (#1087).
	RailRuleRows = 1

	// AutomationsRows is the full automations-section height (bottom of the
	// left rail, #1087); AutomationsCompactRows is its 1-line-summary height
	// when the terminal is tight. Both include automationsBottomMargin: the
	// section reserves its last row as a blank bottom margin so the workspace
	// frame's bottom border never abuts the rail's text (#1560) — the floor is
	// therefore 4 (title + one row + margin) and the compact summary is 2
	// (summary + margin).
	AutomationsRows        = 4
	AutomationsCompactRows = 2

	// automationsBottomMargin is the blank row the automations section keeps at
	// the very bottom of the rail. It mirrors the sidebar's leading blank row
	// (sidebar chromeLines): that keeps the workspace's TOP border off the
	// rail's first row, and this keeps the BOTTOM border off the rail's last
	// row, so the bottom-aligned section can never render text on the same line
	// as the workspace frame's `╰──╯` (#1560).
	automationsBottomMargin = 1

	// ProjectsRows / ProjectsCompactRows size the Projects section pinned at the
	// very bottom of the rail, below the automations section (#1588 follow-up).
	// The floor is 3 (title + one project row + the reserved bottom margin) and
	// the compact 1-line summary is 2 (summary + margin), mirroring the
	// automations floors one section up.
	ProjectsRows        = 3
	ProjectsCompactRows = 2

	// projectsBottomMargin is the Projects section's blank bottom row. The
	// Projects section is now the bottom-most rail region, so it — not the
	// automations section above it — carries the #1560 margin that keeps the
	// workspace frame's bottom border off the rail's last text row.
	projectsBottomMargin = 1

	// PaneMinWidth is the minimum usable width of one workspace pane (#1088,
	// §2.6). The pane-count fitting divides the workspace evenly with 1-col
	// dividers; MaxPanes is how many panes of at least this width fit. A
	// single pane is exempt — it takes whatever the workspace has, exactly
	// as the pre-split pane A did.
	PaneMinWidth = 40

	// MultiPaneMinWidth: below this total width the workspace collapses
	// toward a single pane regardless of PaneMinWidth math (§2.6 ladder). The
	// caller retains the hidden panes' bindings and restores them on grow.
	MultiPaneMinWidth = 110

	// AutomationsFullMinWidth / AutomationsFullMinHeight: below either the
	// automations section collapses to the 1-line summary.
	AutomationsFullMinWidth  = 80
	AutomationsFullMinHeight = 20

	// MinimalWidth / MinimalHeight: below either the layout drops to
	// minimal mode — tree + single pane + status bar only (no automations
	// section, never more than one pane).
	MinimalWidth  = 60
	MinimalHeight = 15

	// HardMinWidth / HardMinHeight: below either no layout is produced;
	// Layout.Fallback signals the caller to render the fallback banner
	// (ui/fallback.go).
	HardMinWidth  = 40
	HardMinHeight = 10
)

// Grid is the single sizing authority for the multi-pane TUI (§2.6): it
// turns a terminal size into the set of region rects, applying the
// degradation ladder as the terminal shrinks.
type Grid struct {
	// Panes is the number of open workspace panes requested (#1088). The
	// grid honors at most MaxPanes of them — the caller picks WHICH panes
	// stay visible (least-recently-focused hide first) and keeps the rest
	// bound for when the terminal grows back.
	Panes int

	// Automations is the number of automations (tasks) the rail's bottom
	// section holds. The section grows to show one row per automation (plus
	// its title) when the rail has the vertical room, and only collapses into
	// a scrollable strip when the tree + automations together can't fit
	// (#1126); zero keeps the section at its AutomationsRows floor.
	Automations int

	// Projects is the number of projects the rail's bottom-most section holds
	// (#1588 follow-up). Like Automations, the section grows to show one row per
	// project when the rail has the vertical room, capped so the tree keeps
	// priority; zero keeps it at the ProjectsRows floor.
	Projects int

	// Banner reserves the top-of-screen delivery-failure alarm row (#1238) when
	// set. It is cut before every other region so the alarm sits above the rail,
	// workspace, and status bar, and it survives the degradation ladder — an
	// active outage alarm shows even in minimal mode.
	Banner bool
}

// Layout is a solved arrangement: the named region rects plus which regions
// are visible at this size. Hidden regions have zero rects. Unless Fallback
// is set, the visible regions exactly tile the Width×Height screen.
type Layout struct {
	Width  int
	Height int

	// Fallback is set below the hard minimum size: no regions are laid
	// out and the caller should render the fallback banner instead.
	Fallback bool

	Tree Rect
	// Workspace is the full content area right of the rail (#1088): the
	// panes and dividers exactly tile it. It is what the caller renders
	// into when no panes are open.
	Workspace Rect
	// Panes are the visible workspace pane rects, left to right —
	// min(Grid.Panes, MaxPanes) of them, dividing Workspace evenly.
	Panes []Rect
	// Dividers are the 1-col rects between adjacent panes: Dividers[i] sits
	// between Panes[i] and Panes[i+1] (len = max(0, len(Panes)-1)).
	Dividers []Rect
	// RailRule is the 1-row horizontal rule inside the left rail separating
	// the tree from the bottom-aligned automations section (#1087). Visible
	// exactly when Automations is.
	RailRule    Rect
	Automations Rect
	// ProjectsRule is the 1-row horizontal rule separating the automations
	// section from the bottom-most Projects section (#1588 follow-up). Visible
	// exactly when Projects is.
	ProjectsRule Rect
	Projects     Rect
	StatusBar    Rect
	// Banner is the top-of-screen delivery-failure alarm row (#1238), non-empty
	// exactly when Grid.Banner was set and the layout is not a fallback.
	Banner Rect

	// MaxPanes is how many panes fit at this size (§2.6 pane-count fitting):
	// always at least 1 outside fallback. The caller auto-hides
	// least-recently-focused panes beyond it and restores them on grow.
	MaxPanes int
	// AutomationsVisible reports whether the automations section is shown at
	// all; AutomationsCompact whether it is the 1-line summary. (The full
	// task manager is a modal overlay, not a layout region — the in-rail
	// section is always the compact summary.)
	AutomationsVisible bool
	AutomationsCompact bool
	// ProjectsVisible reports whether the bottom Projects section is shown at
	// all (#1588 follow-up); ProjectsCompact whether it is the 1-line summary.
	// It shares the automations degradation thresholds, so the two bottom
	// sections appear and compact together.
	ProjectsVisible bool
	ProjectsCompact bool
}

// PaneCount returns how many panes this layout shows.
func (l Layout) PaneCount() int { return len(l.Panes) }

// Solve lays out a width×height terminal.
func (g Grid) Solve(width, height int) Layout {
	l := Layout{Width: width, Height: height}
	if width < HardMinWidth || height < HardMinHeight {
		l.Fallback = true
		return l
	}

	minimal := width < MinimalWidth || height < MinimalHeight

	// Reserve the alarm banner at the very top first, so every other region's
	// absolute Y is shifted down by CutTop and click zones stay accurate. It
	// is cut regardless of minimal mode — a delivery-failure alarm must survive
	// a cramped terminal (#1238).
	full := Rect{X: 0, Y: 0, W: width, H: height}
	if g.Banner {
		l.Banner, full = full.CutTop(AlarmBarRows)
	}

	rem, statusBar := full.CutBottom(StatusBarRows)
	l.StatusBar = statusBar

	// The left rail and the workspace both run the full height above the
	// status bar (#1090): the rail hosts the tree plus — outside minimal
	// mode — the bottom-aligned automations section under a horizontal rule
	// (#1087), and the workspace is purely content panes.
	treeWidth := clampInt(width*25/100, TreeMinWidth, TreeMaxWidth)
	rail, workspace := rem.CutLeft(treeWidth)

	if !minimal {
		railH := rail.H
		compact := width < AutomationsFullMinWidth || height < AutomationsFullMinHeight

		// Projects section: pinned at the VERY bottom of the rail, below the
		// automations section (#1588 follow-up). Cut bottom-up so it lands at the
		// rail's floor; automations and the tree stack above it.
		l.ProjectsVisible = true
		l.ProjectsCompact = compact
		projRows := ProjectsRows
		if !compact {
			// Grow to show every project (title + one row each + bottom margin);
			// the ProjectsRows floor keeps a recognizable strip. Cap the section
			// at a third of the rail so the tree + automations stay the priority —
			// beyond that projects scroll rather than crowd them out.
			if want := 2 + projectsBottomMargin + g.Projects; want > projRows {
				projRows = want
			}
			if third := (railH - RailRuleRows) / 3; third >= ProjectsRows && projRows > third {
				projRows = third
			}
		} else {
			projRows = ProjectsCompactRows
		}

		// Automations section, one section up. Its half-rail cap is measured
		// against the rail the Projects section leaves behind, preserving the
		// #1087/#1126 "tree keeps at least half" behavior around the new section.
		l.AutomationsVisible = true
		l.AutomationsCompact = compact
		autoRows := AutomationsRows
		if !compact {
			// Grow the section to show every automation when the rail has the
			// room: the title line, one row per automation, one line held for
			// the focused row's expansion detail (#1126), plus the reserved
			// bottom margin (#1560).
			if want := 2 + automationsBottomMargin + g.Automations; want > autoRows {
				autoRows = want
			}
			railAfterProjects := railH - (projRows + RailRuleRows)
			if half := (railAfterProjects - RailRuleRows) / 2; half >= AutomationsRows && autoRows > half {
				autoRows = half
			}
		} else {
			autoRows = AutomationsCompactRows
		}

		// Tree-priority guard (#1590): the instances tree is in the focus ring and
		// must never be squeezed to nothing by the fixed bottom sections. Enforce a
		// minimum tree height explicitly instead of leaning on the minimal-mode
		// cutoff: the Projects section yields first — shrinking toward its compact
		// summary, then hiding entirely (returning its rows AND its rule to the
		// tree) — and only then does automations give ground down to its own
		// compact floor. This keeps the tree usable at any rail height a
		// non-minimal layout can produce, the same "tree wins the squeeze" contract
		// #1560 gave the automations overlap.
		treeH := func() int {
			h := railH - autoRows - RailRuleRows
			if l.ProjectsVisible {
				h -= projRows + RailRuleRows
			}
			return h
		}
		for l.ProjectsVisible && treeH() < TreeMinHeight {
			if projRows > ProjectsCompactRows {
				projRows--
				continue
			}
			// Even the compact section won't fit: hide it so the tree reclaims the
			// section and its rule.
			l.ProjectsVisible = false
			l.ProjectsCompact = false
			projRows = 0
		}
		for treeH() < TreeMinHeight && autoRows > AutomationsCompactRows {
			autoRows--
		}
		// A guard-shrunk section renders its 1-line summary so it keeps the #1560
		// bottom margin (full mode only reserves the margin at its floor height).
		if l.ProjectsVisible && projRows < ProjectsRows {
			l.ProjectsCompact = true
		}
		if autoRows < AutomationsRows {
			l.AutomationsCompact = true
		}

		if l.ProjectsVisible {
			rail, l.Projects = rail.CutBottom(projRows)
			rail, l.ProjectsRule = rail.CutBottom(RailRuleRows)
		}
		rail, l.Automations = rail.CutBottom(autoRows)
		rail, l.RailRule = rail.CutBottom(RailRuleRows)
	}
	l.Tree = rail
	l.Workspace = workspace

	// Pane-count fitting (#1088, §2.6): N panes need N·PaneMinWidth plus the
	// N-1 divider columns. One pane always fits (it takes the workspace as
	// is); more panes only above the multi-pane threshold.
	l.MaxPanes = 1
	if !minimal && width >= MultiPaneMinWidth {
		if fit := (workspace.W + 1) / (PaneMinWidth + 1); fit > 1 {
			l.MaxPanes = fit
		}
	}

	n := g.Panes
	if n > l.MaxPanes {
		n = l.MaxPanes
	}
	if n > 0 {
		// Divide the workspace evenly: the leftmost panes absorb the
		// remainder columns so panes plus dividers tile it exactly.
		content := workspace.W - (n - 1)
		base, extra := content/n, content%n
		rest := workspace
		for i := 0; i < n; i++ {
			w := base
			if i < extra {
				w++
			}
			var pane Rect
			pane, rest = rest.CutLeft(w)
			l.Panes = append(l.Panes, pane)
			if i < n-1 {
				var div Rect
				div, rest = rest.CutLeft(1)
				l.Dividers = append(l.Dividers, div)
			}
		}
	}
	return l
}

// VisibleRegions returns the regions visible at this size, keyed by region
// id — panes and dividers positionally (PaneRegion(i)/DividerRegion(i)), the
// bare workspace when no panes are open. Fallback layouts have none.
func (l Layout) VisibleRegions() map[string]Rect {
	regions := make(map[string]Rect)
	if l.Fallback {
		return regions
	}
	regions[RegionTree] = l.Tree
	regions[RegionStatusBar] = l.StatusBar
	if len(l.Panes) == 0 {
		regions[RegionWorkspace] = l.Workspace
	}
	for i, r := range l.Panes {
		regions[PaneRegion(i)] = r
	}
	for i, r := range l.Dividers {
		regions[DividerRegion(i)] = r
	}
	if l.AutomationsVisible {
		regions[RegionRailRule] = l.RailRule
		regions[RegionAutomations] = l.Automations
	}
	if l.ProjectsVisible {
		regions[RegionProjectsRule] = l.ProjectsRule
		regions[RegionProjects] = l.Projects
	}
	return regions
}
