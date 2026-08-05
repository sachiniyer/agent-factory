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
// action is session creation, not opening a pane. One punchy line (#1993): the
// footer already carries `? help` and `n new`, so the old four-line block only
// repeated chrome and buried the one thing to do.
func FirstRunWorkspace(r layout.Rect) string {
	return emptyWorkspaceContent(r, []string{
		"No sessions yet — press n to create one.",
	})
}

// NoActiveProjectWorkspace renders the zero-session state in registry mode
// (#2477): af launched outside a repo, with no active project yet.
//
// It deliberately does NOT advertise `n`, which FirstRunWorkspace does and this
// state used to (#2830). In registry mode focus lands on the Projects section,
// and that section is a captive vim-style list that consumes the create verbs by
// design (#1620) — so the key did nothing at all, no form and no notice. Reached
// the long way round, by tabbing to the tree, `n` refuses anyway: a session needs
// a project to live in (#2764/#2815). The copy was promising an action no focused
// region could perform.
//
// pickHint is supplied by the caller rather than derived here so this line and
// the refusal notice name the same key in the same words; see the app package's
// switchProjectPickHint.
func NoActiveProjectWorkspace(r layout.Rect, pickHint string) string {
	line := "No project selected"
	if pickHint != "" {
		line += " — " + pickHint
	}
	return emptyWorkspaceContent(r, []string{line + "."})
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
