package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// #7175: the task manager, hooks editor and config editor are the pane-hosted
// overlays (renderFittedPaneOverlay). They answered the keyboard and swallowed
// every click — they registered no zones and handleModalClick had no case for
// their states, the #1819 class. They now register one overlay:select zone per
// list row, the zone every sibling picker registers, and a press on one routes
// through clickOverlayRow.

// placePaneOverlay composites a pane-hosted overlay over bg and registers its
// rows' click zones against the same origin the compositor places it at.
func (m *home) placePaneOverlay(fg, bg string, pane ui.OverlayRows) string {
	m.registerPaneOverlayRows(fg, overlayOrigin(fg, bg), pane.RowLines())
	return placeOverlay(fg, bg)
}

// registerPaneOverlayRows registers one full-width zone per row the pane
// reports, spanning every line it drew for that row. rows is per line of the
// pane's own String(), which renderFittedPaneOverlay frames at the dialog's
// top-left inset; a line the frame clipped off registers nothing.
func (m *home) registerPaneOverlayRows(fg string, origin layout.Point, rows []int) {
	top := origin.Y + hooksOverlayStyle.GetBorderTopSize() + hooksOverlayStyle.GetPaddingTop()
	bottom := origin.Y + lipgloss.Height(fg) - hooksOverlayStyle.GetBorderBottomSize() - hooksOverlayStyle.GetPaddingBottom()
	width := layout.BlockWidth(fg)
	for line := 0; line < len(rows); {
		idx, start := rows[line], line
		for line < len(rows) && rows[line] == idx {
			line++
		}
		y0, y1 := top+start, min(top+line, bottom)
		if idx < 0 || y1 <= y0 {
			continue
		}
		m.zones.Register(zones.OverlaySelectRow(idx), layout.Rect{X: origin.X, Y: y0, W: width, H: y1 - y0})
	}
}

// clickOverlayRow answers a left press on a pane-hosted overlay's row.
//
// These lists are editors, not pickers: Enter on a row opens its value editor,
// its task form, its hook's edit field — or, on an Accounts row, a login. So a
// press on a row selects it, as j/k would, and a press on the row already
// selected presses Enter on it through the overlay's own key handler, so every
// gate the keyboard passes (save on close, pending create, the quit-key rules)
// still applies. It is the §2.5 tree-row gesture, select then act; a modal
// press seeds no double-click tracker (#1731), so the selected row stands in
// for the second click. A press that names no row is swallowed, as before.
func (m *home) clickOverlayRow(id string, pane ui.OverlayRows, press func(tea.KeyMsg) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	idx, ok := zones.OverlaySelectIdx(id)
	if !ok {
		return m, nil
	}
	if idx == pane.SelectedRow() {
		return press(tea.KeyMsg{Type: tea.KeyEnter})
	}
	pane.SelectRow(idx)
	return m, nil
}
