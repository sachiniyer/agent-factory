package zones

import (
	"strconv"
	"strings"
)

// Zone id constructors and parsers (#1024 R4, RFC §2.5). The panes build ids
// while registering rects during View(); the root mouse router parses them
// back to dispatch. Both sides live here so producer and consumer can't
// drift.
//
// The id grammar is colon-separated: "<region>:<kind>[:<payload>...]". Titles
// may themselves contain colons, so every parser that extracts a title keeps
// it as the FINAL variable-length segment (or, for tree tab rows, pairs it
// with a trailing numeric index parsed from the right).
const (
	// TreeHeader is the Instances section header row (click toggles the
	// section, like Enter on it).
	TreeHeader = "tree:header"
	// TreeHeaderArchived is the Archived folder header row (#1028) — a DISTINCT
	// zone from TreeHeader so clicking each toggles its own section instead of
	// colliding on one id.
	TreeHeaderArchived = "tree:header:archived"
	// TreeBG is the tree pane's background: any click inside the rail's tree
	// region that lands on no finer zone (focuses the tree).
	TreeBG = "tree:bg"
	// AutoBG is the rail's automations-section background (focuses the
	// section).
	AutoBG = "auto:bg"
	// OverlayConfirmYes / OverlayConfirmNo are the confirmation dialog's
	// clickable y/n words.
	OverlayConfirmYes = "overlay:confirm:yes"
	OverlayConfirmNo  = "overlay:confirm:no"
)

// TreeInstance is the zone id of an instance row in the left-rail tree.
func TreeInstance(title string) string { return "tree:instance:" + title }

// TreeInstanceTitle parses a TreeInstance id back to its title.
func TreeInstanceTitle(id string) (title string, ok bool) {
	return strings.CutPrefix(id, "tree:instance:")
}

// TreeArrow is the zone id of an instance row's expand/collapse arrow glyph.
func TreeArrow(title string) string { return "tree:arrow:" + title }

// TreeArrowTitle parses a TreeArrow id back to its title.
func TreeArrowTitle(id string) (title string, ok bool) {
	return strings.CutPrefix(id, "tree:arrow:")
}

// TreeTab is the zone id of a tab child row: the parent instance's title plus
// the 0-based tab slot.
func TreeTab(title string, idx int) string {
	return "tree:tab:" + title + ":" + strconv.Itoa(idx)
}

// TreeTabParts parses a TreeTab id. The index is the final segment (parsed
// from the right) so titles containing colons survive the round-trip.
func TreeTabParts(id string) (title string, idx int, ok bool) {
	rest, found := strings.CutPrefix(id, "tree:tab:")
	if !found {
		return "", 0, false
	}
	cut := strings.LastIndex(rest, ":")
	if cut < 0 {
		return "", 0, false
	}
	idx, err := strconv.Atoi(rest[cut+1:])
	if err != nil {
		return "", 0, false
	}
	return rest[:cut], idx, true
}

// Workspace pane zone kinds (#1088 N-pane model): region is the pane's
// layout region id (layout.PaneRegion(paneID), e.g. "pane:3").
const (
	// PaneKindHeader is the one-line `title · tab` header inside the frame.
	PaneKindHeader = "header"
	// PaneKindBody is the pane's whole rect (frame included).
	PaneKindBody = "body"
	// PaneKindTerm is the embedded terminal's content grid — the rect whose
	// zone-local coordinates ARE emulator grid cells, registered only while
	// the live view is what the pane renders (#1089). Interactive-mode mouse
	// forwarding targets exactly this zone.
	PaneKindTerm = "term"
)

// PaneHeader is the zone id of a workspace pane's header line.
func PaneHeader(region string) string { return region + ":" + PaneKindHeader }

// PaneBody is the zone id of a workspace pane's full rect.
func PaneBody(region string) string { return region + ":" + PaneKindBody }

// PaneTerm is the zone id of a workspace pane's live terminal grid.
func PaneTerm(region string) string { return region + ":" + PaneKindTerm }

// PaneZone parses a PaneHeader/PaneBody/PaneTerm id, returning the layout
// region and the kind (PaneKind*).
func PaneZone(id string) (region, kind string, ok bool) {
	for _, k := range []string{PaneKindHeader, PaneKindBody, PaneKindTerm} {
		if r, found := strings.CutSuffix(id, ":"+k); found {
			return r, k, true
		}
	}
	return "", "", false
}

// AutoTask is the zone id of one task row in the rail's automations section,
// keyed by the task's persistent id.
func AutoTask(taskID string) string { return "auto:task:" + taskID }

// AutoTaskID parses an AutoTask id back to the task id.
func AutoTaskID(id string) (taskID string, ok bool) {
	return strings.CutPrefix(id, "auto:task:")
}

// StatusHint is the zone id of one status-bar key hint; key is the binding's
// primary key string (e.g. "n", "enter", "ctrl+]") — clicking is equivalent
// to pressing it.
func StatusHint(key string) string { return "status:hint:" + key }

// StatusHintKey parses a StatusHint id back to its key string.
func StatusHintKey(id string) (key string, ok bool) {
	return strings.CutPrefix(id, "status:hint:")
}

// OverlaySelectRow is the zone id of row idx in the selection overlay's list.
func OverlaySelectRow(idx int) string { return "overlay:select:" + strconv.Itoa(idx) }

// OverlaySelectIdx parses an OverlaySelectRow id back to its row index.
func OverlaySelectIdx(id string) (idx int, ok bool) {
	return overlayRowIdx(id, "overlay:select:")
}

// OverlaySearchRow is the zone id of result idx in the search overlay's list
// (the index is into the full result list, not the visible window).
func OverlaySearchRow(idx int) string { return "overlay:search:" + strconv.Itoa(idx) }

// OverlaySearchIdx parses an OverlaySearchRow id back to its result index.
func OverlaySearchIdx(id string) (idx int, ok bool) {
	return overlayRowIdx(id, "overlay:search:")
}

func overlayRowIdx(id, prefix string) (int, bool) {
	rest, found := strings.CutPrefix(id, prefix)
	if !found {
		return 0, false
	}
	idx, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return idx, true
}
