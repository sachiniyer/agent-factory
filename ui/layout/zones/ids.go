package zones

import (
	"strconv"
	"strings"
)

// Zone id constructors and parsers (#1024 PR 6). The panes build ids while
// registering rects during View(); the root mouse router parses them back to
// dispatch. Both sides live here so producer and consumer can't drift.
//
// The id grammar is colon-separated: "<region>:<kind>[:<payload>...]". Titles
// may themselves contain colons, so every parser that extracts a title keeps
// it as the FINAL variable-length segment (or, for tree tab rows, pairs it
// with a trailing numeric index parsed from the right).
const (
	// TreeHeader is the Instances section header row (click toggles the
	// section, like Enter on it).
	TreeHeader = "tree:header"
	// TreeBG is the tree pane's background: any click inside the left rail
	// that lands on no finer zone (focuses the tree).
	TreeBG = "tree:bg"
	// AutoStrip is the automations strip background (focuses the strip).
	AutoStrip = "auto:strip"
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

// PaneHeader is the zone id of a workspace pane's header line. region is the
// layout region id (layout.RegionPaneA / layout.RegionPaneB).
func PaneHeader(region string) string { return region + ":header" }

// PaneBody is the zone id of a workspace pane's full rect.
func PaneBody(region string) string { return region + ":body" }

// PaneRegion parses a PaneHeader/PaneBody id, returning the region and
// whether the zone was the header.
func PaneRegion(id string) (region string, header, ok bool) {
	if r, found := strings.CutSuffix(id, ":header"); found {
		return r, true, true
	}
	if r, found := strings.CutSuffix(id, ":body"); found {
		return r, false, true
	}
	return "", false, false
}

// AutoTask is the zone id of one task row in the automations strip (compact
// row or expanded manager row alike), keyed by the task's persistent id.
func AutoTask(taskID string) string { return "auto:task:" + taskID }

// AutoTaskID parses an AutoTask id back to the task id.
func AutoTaskID(id string) (taskID string, ok bool) {
	return strings.CutPrefix(id, "auto:task:")
}

// StatusHint is the zone id of one status-bar key hint; key is the binding's
// primary key string (e.g. "n", "enter", "shift+up") — clicking is equivalent
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
