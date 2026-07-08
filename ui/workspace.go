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
	return emptyWorkspaceContent(r, []string{"no panes open — s opens the selected tab"})
}

// FirstRunWorkspace renders the zero-session onboarding state. It is distinct
// from EmptyWorkspace because there is no selected tab yet, so the useful next
// action is session creation, not opening a pane.
func FirstRunWorkspace(r layout.Rect) string {
	return emptyWorkspaceContent(r, []string{
		"No sessions yet",
		"Press n to create a local session",
		"Press ? for all keys",
		"Setup check: run af doctor --setup",
	})
}

func emptyWorkspaceContent(r layout.Rect, lines []string) string {
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
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered = append(rendered, paneHeaderDimStyle.Render(fitLine(line, iw)))
	}
	content := lipgloss.JoinVertical(lipgloss.Center, rendered...)
	inner := lipgloss.Place(iw, ih, lipgloss.Center, lipgloss.Center, content)
	return layout.ClampToRect(blurredWindowStyle.Render(inner), r)
}
