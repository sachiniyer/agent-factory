package overlay

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// Mouse zone registration for the modal overlays (#1024 R4): clicking a
// confirmation's y/n words or a selection/search row is equivalent to the
// key. Zones are derived by scanning the overlay's own rendered output — not
// by replicating the border/padding/wrap math — so a rendering change moves
// the zones with it instead of leaving them pointing at stale cells. origin
// is the overlay's top-left on screen (the root computes it from the same
// centering PlaceOverlay applies).

// cellColumn is the terminal-cell column of byte offset idx within the
// ANSI-stripped line (border glyphs are multibyte but one cell wide, so byte
// offsets are not columns).
func cellColumn(plain string, idx int) int {
	// layout.Cells, not runewidth: this is the horizontal offset a mouse zone is
	// registered at, so it has to count the same cells the compositor draws. A
	// message carrying clustered text before the needle measured narrower here than
	// it rendered, and the y/n zones landed left of the words (#3585).
	return layout.Cells(plain[:idx])
}

// RegisterZones registers the confirmation dialog's clickable y/n words. The
// instruction line is scanned from the bottom (the message could conceivably
// contain the same words); a wrapped instruction line registers nothing and
// the keyboard remains fully sufficient.
func (c *ConfirmationOverlay) RegisterZones(reg *zones.Registry, origin layout.Point) {
	if reg == nil {
		return
	}
	yesNeedle := c.ConfirmKey + " confirm"
	if c.enterConfirms() {
		// The full hint advertises enter as a confirm alias (#2405); the zone must
		// cover the same words the renderer wrote, or the click target goes stale.
		yesNeedle = c.ConfirmKey + "/enter confirm"
	}
	noNeedle := c.CancelKey + "/esc cancel"
	compactYesNeedle := c.ConfirmKey + " confirm"
	compactNoNeedle := c.CancelKey + "/esc cancel"
	lines := strings.Split(c.Render(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := xansi.Strip(lines[i])
		yi := strings.Index(plain, yesNeedle)
		ni := strings.Index(plain, noNeedle)
		if yi < 0 {
			yi = strings.Index(plain, compactYesNeedle)
			if yi >= 0 {
				yesNeedle = compactYesNeedle
			}
		}
		if ni < 0 {
			ni = strings.Index(plain, compactNoNeedle)
			if ni >= 0 {
				noNeedle = compactNoNeedle
			}
		}
		if yi < 0 && ni < 0 {
			continue
		}
		if yi >= 0 {
			reg.Register(zones.OverlayConfirmYes, layout.Rect{
				X: origin.X + cellColumn(plain, yi), Y: origin.Y + i,
				W: layout.Cells(yesNeedle), H: 1,
			})
		}
		if ni >= 0 {
			reg.Register(zones.OverlayConfirmNo, layout.Rect{
				X: origin.X + cellColumn(plain, ni), Y: origin.Y + i,
				W: layout.Cells(noNeedle), H: 1,
			})
		}
		return
	}
}

// RegisterZones registers one full-width clickable zone per selection row;
// clicking a row selects and submits it, like ↓/↑ + enter.
func (s *SelectionOverlay) RegisterZones(reg *zones.Registry, origin layout.Point) {
	if reg == nil {
		return
	}
	rendered := s.Render()
	width := renderedWidth(rendered)
	lines := strings.Split(rendered, "\n")
	style := selectionOverlayStyle()
	fit := fitOverlayContent(s.width, 0, s.maxWidth, s.maxHeight, style)
	textRect := overlayTextRect(fit, style)
	startIdx, endIdx, _, _, _ := s.windowForTextHeight(textRect.H)
	next := startIdx
	for i, line := range lines {
		if next >= endIdx {
			break
		}
		t := strings.Trim(xansi.Strip(line), "│ ")
		if t == "▸ "+s.items[next] || t == s.items[next] {
			reg.Register(zones.OverlaySelectRow(next), layout.Rect{
				X: origin.X, Y: origin.Y + i, W: width, H: 1,
			})
			next++
		}
	}
}

// RegisterZones registers one full-width clickable zone per visible search
// result, keyed by the result's index in the full result list; clicking a
// row selects and submits it. Geometry comes from the same render pass as the
// frame, so user titles cannot masquerade as chrome or shift pointer targets.
func (s *SearchOverlay) RegisterZones(reg *zones.Registry, origin layout.Point) {
	if reg == nil {
		return
	}
	rendered, firstRow, plan := s.renderFrame()
	width := renderedWidth(rendered)
	for idx := plan.startIdx; idx < plan.endIdx; idx++ {
		reg.Register(zones.OverlaySearchRow(idx), layout.Rect{
			X: origin.X, Y: origin.Y + firstRow + idx - plan.startIdx, W: width, H: 1,
		})
	}
}

// SetSelectedIndex moves the search selection onto the given result index
// (the click action for a result row). Out-of-range indices no-op.
func (s *SearchOverlay) SetSelectedIndex(idx int) {
	if idx >= 0 && idx < len(s.results) {
		s.selectedIdx = idx
	}
}

// renderedWidth is the widest row of a rendered overlay, measured with the same
// helper the compositor places it by. Named for what it does: it stopped being
// lipgloss.Width when the measures were unified (#3585).
func renderedWidth(rendered string) int {
	_, w := getLines(rendered)
	return w
}
