package layout

// Region identifiers for the named layout regions. These double as focus
// ring ids (§2.3) and as the pane component of zone ids (§2.5).
const (
	RegionTree        = "tree"
	RegionPaneA       = "paneA"
	RegionPaneB       = "paneB"
	RegionDivider     = "divider"
	RegionRailRule    = "railRule"
	RegionAutomations = "automations"
	RegionStatusBar   = "status"
)

// Sizing constants and degradation-ladder thresholds (RFC §2.6). A feature
// named by a *Min* threshold is available at sizes >= the threshold and
// degrades below it.
const (
	// TreeMinWidth / TreeMaxWidth clamp the left rail: clamp(22, 25%·W, 36).
	// Narrowed from clamp(24, 30%·W, 44) by #1090 to give the content panes
	// more columns.
	TreeMinWidth = 22
	TreeMaxWidth = 36

	// StatusBarRows is the fixed status-bar height.
	StatusBarRows = 2

	// RailRuleRows is the horizontal rule separating the instances tree from
	// the automations section inside the left rail (#1087).
	RailRuleRows = 1

	// AutomationsRows is the full automations-section height (bottom of the
	// left rail, #1087); AutomationsCompactRows is its 1-line-summary height
	// when the terminal is tight.
	AutomationsRows        = 3
	AutomationsCompactRows = 1

	// SplitMinWidth: below this a requested split collapses to pane A (the
	// caller retains pane B's binding and restores it on grow).
	SplitMinWidth = 110

	// AutomationsFullMinWidth / AutomationsFullMinHeight: below either the
	// automations section collapses to the 1-line summary.
	AutomationsFullMinWidth  = 80
	AutomationsFullMinHeight = 20

	// MinimalWidth / MinimalHeight: below either the layout drops to
	// minimal mode — tree + single pane + status bar only (no automations
	// section, split never honored).
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
	// Split requests a two-pane workspace. It is honored only when the
	// terminal is wide enough (>= SplitMinWidth) and not in minimal mode;
	// otherwise the workspace is pane A alone and the caller keeps pane B's
	// binding for when the terminal grows back.
	Split bool

	// AutomationsExpanded requests the automations section expanded in place —
	// focusing it swaps the compact task rows for the full task manager
	// (§2.1). Honored whenever the section is visible (i.e. outside minimal
	// mode): the expanded section takes half the left rail's rows (#1087 moved
	// it into the rail), so the tree above it stays usable. Expansion
	// overrides the compact 1-line degradation — an editor cannot run in one
	// line.
	AutomationsExpanded bool
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

	Tree    Rect
	PaneA   Rect
	Divider Rect
	PaneB   Rect
	// RailRule is the 1-row horizontal rule inside the left rail separating
	// the tree from the bottom-aligned automations section (#1087). Visible
	// exactly when Automations is.
	RailRule    Rect
	Automations Rect
	StatusBar   Rect

	// SplitActive reports whether the split was honored (PaneB and Divider
	// are visible).
	SplitActive bool
	// AutomationsVisible reports whether the automations section is shown at
	// all; AutomationsCompact whether it is the 1-line summary;
	// AutomationsExpanded whether the section got the expanded (full task
	// manager) allocation.
	AutomationsVisible  bool
	AutomationsCompact  bool
	AutomationsExpanded bool
}

// Solve lays out a width×height terminal.
func (g Grid) Solve(width, height int) Layout {
	l := Layout{Width: width, Height: height}
	if width < HardMinWidth || height < HardMinHeight {
		l.Fallback = true
		return l
	}

	minimal := width < MinimalWidth || height < MinimalHeight

	rem, statusBar := Rect{X: 0, Y: 0, W: width, H: height}.CutBottom(StatusBarRows)
	l.StatusBar = statusBar

	// The left rail and the workspace both run the full height above the
	// status bar (#1090): the rail hosts the tree plus — outside minimal
	// mode — the bottom-aligned automations section under a horizontal rule
	// (#1087), and the workspace is purely content panes.
	treeWidth := clampInt(width*25/100, TreeMinWidth, TreeMaxWidth)
	rail, workspace := rem.CutLeft(treeWidth)

	if !minimal {
		l.AutomationsVisible = true
		l.AutomationsCompact = width < AutomationsFullMinWidth || height < AutomationsFullMinHeight
		rows := AutomationsRows
		if l.AutomationsCompact {
			rows = AutomationsCompactRows
		}
		if g.AutomationsExpanded {
			// Expanded in place: half the rail's rows. Outside minimal mode
			// rail.H >= MinimalHeight - StatusBarRows = 13, so the expanded
			// section always gets >= 6 rows and the tree keeps at least as
			// much minus the rule.
			l.AutomationsExpanded = true
			l.AutomationsCompact = false
			rows = rail.H / 2
			if rows < AutomationsRows {
				rows = AutomationsRows
			}
		}
		rail, l.Automations = rail.CutBottom(rows)
		rail, l.RailRule = rail.CutBottom(RailRuleRows)
	}
	l.Tree = rail

	if g.Split && !minimal && width >= SplitMinWidth {
		l.SplitActive = true
		paneA, rest := workspace.CutLeft((workspace.W - 1) / 2)
		l.PaneA = paneA
		l.Divider, l.PaneB = rest.CutLeft(1)
	} else {
		l.PaneA = workspace
	}
	return l
}

// VisibleRegions returns the regions visible at this size, keyed by region
// id. Fallback layouts have none.
func (l Layout) VisibleRegions() map[string]Rect {
	regions := make(map[string]Rect)
	if l.Fallback {
		return regions
	}
	regions[RegionTree] = l.Tree
	regions[RegionPaneA] = l.PaneA
	regions[RegionStatusBar] = l.StatusBar
	if l.SplitActive {
		regions[RegionDivider] = l.Divider
		regions[RegionPaneB] = l.PaneB
	}
	if l.AutomationsVisible {
		regions[RegionRailRule] = l.RailRule
		regions[RegionAutomations] = l.Automations
	}
	return regions
}
