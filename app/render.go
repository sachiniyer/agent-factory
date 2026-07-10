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
}

func (m *home) hooksOverlayContentRect() layout.Rect {
	return m.modalContentRect(hooksOverlayStyle, m.preferredOverlayWidth(50), m.preferredOverlayHeight())
}

func (m *home) tasksOverlayContentRect() layout.Rect {
	return m.modalContentRect(hooksOverlayStyle, m.preferredOverlayWidth(52), m.preferredOverlayHeight())
}

func (m *home) modalContentRect(style lipgloss.Style, preferredW, preferredH int) layout.Rect {
	return layout.FitContentRect(
		layout.Rect{W: preferredW, H: preferredH},
		layout.Rect{W: m.termWidth, H: m.termHeight},
		style.GetHorizontalBorderSize(),
		style.GetVerticalBorderSize(),
	)
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
