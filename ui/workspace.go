package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// EmptyWorkspace renders the workspace area when no panes are open (#1088):
// a receded frame with the open-pane affordance, exactly rect-sized. The
// N-pane model has no selection-driven pane — content appears when the user
// opens a tab as a pane (`s`), so the empty state must say exactly that.
func EmptyWorkspace(r layout.Rect) string {
	if r.Empty() {
		return ""
	}
	iw := r.W - blurredWindowStyle.GetHorizontalFrameSize()
	ih := r.H - blurredWindowStyle.GetVerticalFrameSize()
	if iw < 0 {
		iw = 0
	}
	if ih < 0 {
		ih = 0
	}
	hint := paneHeaderDimStyle.Render("no panes open — s opens the selected tab")
	inner := lipgloss.Place(iw, ih, lipgloss.Center, lipgloss.Center, hint)
	return layout.ClampToRect(blurredWindowStyle.Render(inner), r)
}
