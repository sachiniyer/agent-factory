package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
)

// View render helpers extracted from app.go (#1145): the overlay framing style
// and the rail/divider rules the composed View draws. Behavior is unchanged —
// this is a pure relocation to keep app.go under its length ceiling.

// hooksOverlayStyle frames the hooks editor when it is hosted as an overlay
// (#1024 PR 4: hooks lost their persistent sidebar slot).
var hooksOverlayStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(ui.AccentColor).
	Padding(1, 2)

func (m *home) renderHooksOverlay() string {
	return hooksOverlayStyle.Render(m.hooksPane.String())
}

// renderTasksOverlay frames the task manager (list + create/edit form) as the
// centered modal it lives in (#1087 play-test): the manager needs real
// width/height for its form, which the narrow left rail cannot provide.
func (m *home) renderTasksOverlay() string {
	return hooksOverlayStyle.Render(m.automations.TaskPane().String())
}

// splitDividerStyle recedes the 1-col dividers between panes so the focused
// pane's frame stays the strongest line on screen.
var splitDividerStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#DDDADA", Dark: "#3C3C3C"})

// renderDivider renders the 1-col divider right of pane i (§2.6: "N panes
// divide the workspace width evenly with 1-col dividers").
func (m *home) renderDivider(i int) string {
	if i < 0 || i >= len(m.lastLayout.Dividers) {
		return ""
	}
	r := m.lastLayout.Dividers[i]
	if r.Empty() {
		return ""
	}
	col := strings.TrimSuffix(strings.Repeat("│\n", r.H), "\n")
	return splitDividerStyle.Render(col)
}

// renderRailRule renders the full-rail-width horizontal rule separating the
// instances tree from the bottom-aligned automations section (#1087), in the
// same receded style as the split divider.
func (m *home) renderRailRule() string {
	r := m.lastLayout.RailRule
	if r.Empty() {
		return ""
	}
	return splitDividerStyle.Render(strings.Repeat("─", r.W))
}
