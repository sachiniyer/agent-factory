package layout

// Region identifiers for the named layout regions. These double as focus
// ring ids (§2.3) and as the pane component of zone ids (§2.5).
const (
	RegionTree        = "tree"
	RegionPaneA       = "paneA"
	RegionPaneB       = "paneB"
	RegionDivider     = "divider"
	RegionAutomations = "automations"
	RegionStatusBar   = "status"
)

// Sizing constants and degradation-ladder thresholds (RFC §2.6). A feature
// named by a *Min* threshold is available at sizes >= the threshold and
// degrades below it.
const (
	// TreeMinWidth / TreeMaxWidth clamp the left rail: clamp(24, 30%·W, 44).
	TreeMinWidth = 24
	TreeMaxWidth = 44

	// StatusBarRows is the fixed status-bar height.
	StatusBarRows = 2

	// AutomationsRows is the full automations-strip height;
	// AutomationsCompactRows is its 1-line-summary height when the terminal
	// is tight.
	AutomationsRows        = 3
	AutomationsCompactRows = 1

	// SplitMinWidth: below this a requested split collapses to pane A (the
	// caller retains pane B's binding and restores it on grow).
	SplitMinWidth = 110

	// AutomationsFullMinWidth / AutomationsFullMinHeight: below either the
	// automations strip collapses to the 1-line summary.
	AutomationsFullMinWidth  = 80
	AutomationsFullMinHeight = 20

	// MinimalWidth / MinimalHeight: below either the layout drops to
	// minimal mode — tree + single pane + status bar only (no automations
	// strip, split never honored).
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

	Tree        Rect
	PaneA       Rect
	Divider     Rect
	PaneB       Rect
	Automations Rect
	StatusBar   Rect

	// SplitActive reports whether the split was honored (PaneB and Divider
	// are visible).
	SplitActive bool
	// AutomationsVisible reports whether the automations strip is shown at
	// all; AutomationsCompact whether it is the 1-line summary.
	AutomationsVisible bool
	AutomationsCompact bool
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

	if !minimal {
		l.AutomationsVisible = true
		l.AutomationsCompact = width < AutomationsFullMinWidth || height < AutomationsFullMinHeight
		rows := AutomationsRows
		if l.AutomationsCompact {
			rows = AutomationsCompactRows
		}
		rem, l.Automations = rem.CutBottom(rows)
	}

	treeWidth := clampInt(width*30/100, TreeMinWidth, TreeMaxWidth)
	tree, workspace := rem.CutLeft(treeWidth)
	l.Tree = tree

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
		regions[RegionAutomations] = l.Automations
	}
	return regions
}
