package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// View render helpers extracted from app.go (#1145): overlay frame sizing and
// the rail/divider rules the composed View draws.

// hooksOverlayStyle frames the hooks editor when it is hosted as an overlay
// (#1024 PR 4: hooks lost their persistent sidebar slot).
var hooksOverlayStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(ui.AccentColor).
	Padding(1, 2)

func (m *home) renderHooksOverlay() string {
	return m.renderFittedPaneOverlay(
		m.hooksOverlayContentRect(),
		func(w, h int) { m.hooksPane.SetSize(w, h) },
		m.hooksPane.String,
	)
}

// renderConfigOverlay frames the global config editor, using the same
// pane-hosted modal framing as the hooks and tasks overlays.
func (m *home) renderConfigOverlay() string {
	return m.renderFittedPaneOverlay(
		m.configOverlayContentRect(),
		func(w, h int) { m.configPane.SetSize(w, h) },
		m.configPane.String,
	)
}

// renderTasksOverlay frames the task manager (list + create/edit form) as the
// centered modal it lives in (#1087 play-test): the manager needs real
// width/height for its form, which the narrow left rail cannot provide.
func (m *home) renderTasksOverlay() string {
	return m.renderFittedPaneOverlay(
		m.tasksOverlayContentRect(),
		func(w, h int) { m.automations.TaskPane().SetSize(w, h) },
		m.automations.TaskPane().String,
	)
}

func (m *home) renderFittedPaneOverlay(r layout.Rect, setSize func(int, int), renderContent func() string) string {
	if r.W < 1 {
		r.W = 1
	}
	if r.H < 1 {
		r.H = 1
	}
	for {
		contentRect := paneOverlayContentRect(r)
		setSize(contentRect.W, contentRect.H)
		fg := sizedOverlayStyle(hooksOverlayStyle, r).Render(renderContent())
		fgW, fgH := lipgloss.Width(fg), lipgloss.Height(fg)
		tooWide := m.termWidth > 0 && fgW > m.termWidth
		tooTall := m.termHeight > 0 && fgH > m.termHeight
		if (!tooWide && !tooTall) || (r.W == 1 && r.H == 1) {
			if tooWide || tooTall {
				return layout.ClampToRect(fg, layout.Rect{W: m.termWidth, H: m.termHeight})
			}
			return fg
		}
		if tooWide && r.W > 1 {
			r.W -= fgW - m.termWidth
			if r.W < 1 {
				r.W = 1
			}
		}
		if tooTall && r.H > 1 {
			r.H -= fgH - m.termHeight
			if r.H < 1 {
				r.H = 1
			}
		}
	}
}

func (m *home) layoutModalOverlays() {
	m.layoutTextOverlay()
	m.layoutSelectionOverlay()
	m.layoutConfirmationOverlay()
	m.layoutSearchOverlay()
	m.layoutProjectPickerOverlay()
	m.layoutPaneOverlays()
}

func (m *home) layoutSelectionOverlay() {
	if m.selectionOverlay == nil {
		return
	}
	m.selectionOverlay.SetWidth(int(float32(m.termWidth) * 0.6))
	m.selectionOverlay.SetMaxSize(m.termWidth, m.termHeight)
}

func (m *home) layoutConfirmationOverlay() {
	if m.confirmationOverlay != nil {
		m.confirmationOverlay.SetMaxSize(m.termWidth, m.termHeight)
	}
}

func (m *home) layoutSearchOverlay() {
	if m.searchOverlay != nil {
		m.searchOverlay.SetMaxSize(m.termWidth, m.termHeight)
	}
}

func (m *home) layoutProjectPickerOverlay() {
	if m.projectPickerOverlay != nil {
		m.projectPickerOverlay.SetMaxSize(m.termWidth, m.termHeight)
	}
}

func (m *home) layoutPaneOverlays() {
	hooksRect := m.hooksOverlayContentRect()
	hooksContent := paneOverlayContentRect(hooksRect)
	m.hooksPane.SetSize(hooksContent.W, hooksContent.H)

	taskRect := m.tasksOverlayContentRect()
	taskContent := paneOverlayContentRect(taskRect)
	m.automations.TaskPane().SetSize(taskContent.W, taskContent.H)

	configRect := m.configOverlayContentRect()
	configContent := paneOverlayContentRect(configRect)
	m.configPane.SetSize(configContent.W, configContent.H)
}

// configOverlayContentRect sizes the config editor. It asks for more width than
// the hooks and tasks modals (64 vs 50/52) because a config row carries a key, a
// value, and a one-line purpose — a narrower box wraps the purpose into noise.
// The same #1821 full-screen fallback applies on a terminal too narrow for that.
func (m *home) configOverlayContentRect() layout.Rect {
	return m.modalContentRect(hooksOverlayStyle, m.preferredOverlayWidth(64), m.preferredOverlayHeight())
}

func (m *home) hooksOverlayContentRect() layout.Rect {
	return m.modalContentRect(hooksOverlayStyle, m.preferredOverlayWidth(50), m.preferredOverlayHeight())
}

func (m *home) tasksOverlayContentRect() layout.Rect {
	return m.modalContentRect(hooksOverlayStyle, m.preferredOverlayWidth(52), m.preferredOverlayHeight())
}

// modalContentRect sizes a pane-hosted modal — the tasks manager and the hooks
// editor, the two overlays that frame an existing pane instead of being a
// ui/overlay type.
//
// Presentation is deliberately two-mode (#1821):
//
//   - Beside the rail (default): the modal keeps its preferred size and
//     PlaceOverlay centers it over the frame, exactly like every other modal.
//   - Full-screen: when the modal can no longer fit beside the rail, it takes
//     the whole terminal instead.
//
// The width floor is what forces the switch. preferredOverlayWidth never
// yields below its minimum, because a task form narrower than that stops being
// usable — but that floor ignores the terminal, so on a narrow one the box
// ends up wider than the workspace, the centered modal lands ON the rail, and
// the only rail left showing is an unreadable sliver down the side. At 60x20 a
// 54-column box centers at x=3 over a 22-column rail: 19 of its 22 columns are
// painted over and the survivors are stubs like `▸`, `Au`, `Pr` (#1821).
// Full-screen is the deliberate presentation the issue asks for instead — the
// rail is covered cleanly and completely, no fragments, and the form gets
// every column the terminal has.
//
// Rail-AWARE placement (keeping the modal strictly right of the rail at every
// size) is deliberately NOT what this does. No overlay here is rail-aware:
// PlaceOverlay centers over the whole frame, so every modal overlaps the rail
// once it is wide enough — the narrow ones just don't get wide enough to show
// it. Making this one modal dodge the rail would single it out for no gain the
// issue asked for.
func (m *home) modalContentRect(style lipgloss.Style, preferredW, preferredH int) layout.Rect {
	if m.modalGoesFullScreen(style, preferredW) {
		// A non-positive preferred dimension means "use all available space",
		// so the frame lands on exactly termWidth x termHeight.
		preferredW, preferredH = 0, 0
	}
	return layout.FitContentRect(
		layout.Rect{W: preferredW, H: preferredH},
		layout.Rect{W: m.termWidth, H: m.termHeight},
		style.GetHorizontalBorderSize(),
		style.GetVerticalBorderSize(),
	)
}

// modalGoesFullScreen reports whether the terminal is too narrow for this modal
// and the rail to COEXIST: whether the modal's floored outer box (content +
// border) still fits in the workspace the rail leaves behind.
//
// This is a narrowness test, not a placement promise. Placement stays centered
// over the whole frame — a modal that passes this test is NOT moved to sit
// beside the rail, and at mid widths it still clips the rail's right edge
// exactly as every other modal does. What the test buys is the point where that
// stops being a clipped rail and becomes a useless sliver: once the box cannot
// fit in the workspace at all, centering it can only ever leave fragments, so
// the modal takes the whole terminal instead.
//
// It reads the workspace from the solved layout rather than recomputing
// termWidth - railWidth, so the rail's own sizing rule (clamped 25%, see
// Grid.Solve) stays in exactly one place. relayout assigns m.lastLayout before
// it sizes the overlays, and returns early in Fallback mode (where View()
// renders the too-small banner and no overlay at all), so the workspace here is
// always the live one.
func (m *home) modalGoesFullScreen(style lipgloss.Style, preferredW int) bool {
	if m.termWidth <= 0 || m.termHeight <= 0 {
		return false
	}
	workspaceW := m.lastLayout.Workspace.W
	if workspaceW <= 0 {
		return false
	}
	return preferredW+style.GetHorizontalBorderSize() > workspaceW
}

func (m *home) preferredOverlayWidth(minWidth int) int {
	width := int(float32(m.termWidth) * 0.6)
	if width < minWidth {
		width = minWidth
	}
	return width
}

func (m *home) preferredOverlayHeight() int {
	return int(float32(m.termHeight) * 0.6)
}

func sizedOverlayStyle(style lipgloss.Style, r layout.Rect) lipgloss.Style {
	if r.W > 0 {
		style = style.Width(r.W)
	}
	if r.H > 0 {
		style = style.Height(r.H)
	}
	return style
}

func paneOverlayContentRect(styleRect layout.Rect) layout.Rect {
	r := layout.Rect{
		W: styleRect.W - hooksOverlayStyle.GetHorizontalPadding(),
		H: styleRect.H - hooksOverlayStyle.GetVerticalPadding(),
	}
	if styleRect.W > 0 && r.W < 1 {
		r.W = 1
	}
	if styleRect.H > 0 && r.H < 1 {
		r.H = 1
	}
	return r
}

// splitDividerStyle recedes the 1-col dividers between panes so the focused
// pane's frame stays the strongest line on screen.
var splitDividerStyle = lipgloss.NewStyle().
	Foreground(ui.CurrentTheme().BackgroundSubtle)

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

// renderProjectsRule renders the full-rail-width horizontal rule separating the
// automations section from the bottom-most Projects section (#1588 follow-up),
// in the same receded style as the rail rule above it.
func (m *home) renderProjectsRule() string {
	r := m.lastLayout.ProjectsRule
	if r.Empty() {
		return ""
	}
	return splitDividerStyle.Render(strings.Repeat("─", r.W))
}
