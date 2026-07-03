package overlay

import (
	"strings"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// Mouse zone registration for the modal overlays (#1024 PR 6): clicking a
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
	return runewidth.StringWidth(plain[:idx])
}

// RegisterZones registers the confirmation dialog's clickable y/n words. The
// instruction line is scanned from the bottom (the message could conceivably
// contain the same words); a wrapped instruction line registers nothing and
// the keyboard remains fully sufficient.
func (c *ConfirmationOverlay) RegisterZones(reg *zones.Registry, origin layout.Point) {
	if reg == nil {
		return
	}
	yesNeedle := c.ConfirmKey + " to confirm"
	noNeedle := c.CancelKey + " or esc to cancel"
	lines := strings.Split(c.Render(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := xansi.Strip(lines[i])
		yi := strings.Index(plain, yesNeedle)
		ni := strings.Index(plain, noNeedle)
		if yi < 0 && ni < 0 {
			continue
		}
		if yi >= 0 {
			reg.Register(zones.OverlayConfirmYes, layout.Rect{
				X: origin.X + cellColumn(plain, yi), Y: origin.Y + i,
				W: runewidth.StringWidth(yesNeedle), H: 1,
			})
		}
		if ni >= 0 {
			reg.Register(zones.OverlayConfirmNo, layout.Rect{
				X: origin.X + cellColumn(plain, ni), Y: origin.Y + i,
				W: runewidth.StringWidth(noNeedle), H: 1,
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
	width := lipglossWidth(rendered)
	lines := strings.Split(rendered, "\n")
	next := 0
	for i, line := range lines {
		if next >= len(s.items) {
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
// result, keyed by the result's index in the full result list; clicking a row
// selects and submits it.
func (s *SearchOverlay) RegisterZones(reg *zones.Registry, origin layout.Point) {
	if reg == nil {
		return
	}
	rendered := s.Render()
	width := lipglossWidth(rendered)
	lines := strings.Split(rendered, "\n")
	next := 0
	for i, line := range lines {
		if next >= len(s.results) {
			break
		}
		title, ok := searchRowTitle(line)
		if !ok {
			continue
		}
		want := s.results[next].Instance.Title
		if title == want || strings.HasPrefix(title, want+" (") {
			reg.Register(zones.OverlaySearchRow(next), layout.Rect{
				X: origin.X, Y: origin.Y + i, W: width, H: 1,
			})
			next++
		}
	}
}

// searchRowTitle parses a rendered search-result line down to its title text
// (plus the optional " (branch)" suffix the caller strips by prefix match).
// Result rows are the only lines that begin with a status glyph after the
// border/padding, which is what distinguishes them from the query and hint
// lines.
func searchRowTitle(line string) (string, bool) {
	t := strings.Trim(xansi.Strip(line), "│ ")
	r, size := utf8.DecodeRuneInString(t)
	if r != '●' && r != '○' {
		return "", false
	}
	t = strings.TrimLeft(t[size:], " ")
	t = strings.TrimPrefix(t, "▸ ")
	return t, true
}

// SetSelectedIndex moves the search selection onto the given result index
// (the click action for a result row). Out-of-range indices no-op.
func (s *SearchOverlay) SetSelectedIndex(idx int) {
	if idx >= 0 && idx < len(s.results) {
		s.selectedIdx = idx
	}
}

// lipglossWidth is the widest printable line width of a rendered block.
func lipglossWidth(rendered string) int {
	_, w := getLines(rendered)
	return w
}
