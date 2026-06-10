package ui

import (
	"github.com/sachiniyer/agent-factory/session"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ContentMode identifies what the right pane is displaying.
type ContentMode int

const (
	ContentModeInstance ContentMode = iota
	ContentModeTasks
	ContentModeHooks
	ContentModeEmpty
)

// ContentPane wraps the TabbedWindow and other pane types, switching
// what is displayed based on the current sidebar selection.
type ContentPane struct {
	mode         ContentMode
	tabbedWindow *TabbedWindow
	taskPane     *TaskPane
	hooksPane    *HooksPane

	width, height int
}

// NewContentPane creates a new content pane wrapping the given sub-panes.
func NewContentPane(tw *TabbedWindow) *ContentPane {
	return &ContentPane{
		mode:         ContentModeEmpty,
		tabbedWindow: tw,
		taskPane:     NewTaskPane(),
		hooksPane:    NewHooksPane(),
	}
}

// SetSize sets the display dimensions.
func (c *ContentPane) SetSize(width, height int) {
	c.width = width
	c.height = height
	c.tabbedWindow.SetSize(width, height)

	// Calculate content area for inline panes (matching window style)
	contentWidth := AdjustPreviewWidth(width) - windowStyle.GetHorizontalFrameSize()
	contentHeight := height - windowStyle.GetVerticalFrameSize() - 2
	c.taskPane.SetSize(contentWidth, contentHeight)
	c.hooksPane.SetSize(contentWidth, contentHeight)
}

// SetMode switches the content pane mode.
func (c *ContentPane) SetMode(mode ContentMode) {
	if c.mode == mode {
		return
	}
	// Unfocus panes when switching away
	c.taskPane.SetFocus(false)
	c.hooksPane.SetFocus(false)
	c.mode = mode
}

// GetMode returns the current content mode.
func (c *ContentPane) GetMode() ContentMode {
	return c.mode
}

// HasFocus returns true if the content pane has captured input focus.
func (c *ContentPane) HasFocus() bool {
	switch c.mode {
	case ContentModeTasks:
		return c.taskPane.HasFocus()
	case ContentModeHooks:
		return c.hooksPane.HasFocus()
	}
	return false
}

// HandleKeyPress routes key events to the focused sub-pane.
// Returns true if the key was consumed.
func (c *ContentPane) HandleKeyPress(msg tea.KeyMsg) bool {
	switch c.mode {
	case ContentModeTasks:
		if c.taskPane.HasFocus() {
			return c.taskPane.HandleKeyPress(msg)
		}
		if msg.String() == "enter" || msg.String() == "o" {
			c.taskPane.SetFocus(true)
			return true
		}
	case ContentModeHooks:
		if c.hooksPane.HasFocus() {
			return c.hooksPane.HandleKeyPress(msg)
		}
		if msg.String() == "enter" || msg.String() == "o" {
			c.hooksPane.SetFocus(true)
			return true
		}
	}
	return false
}

// TabbedWindow returns the underlying tabbed window.
func (c *ContentPane) TabbedWindow() *TabbedWindow {
	return c.tabbedWindow
}

// TaskPane returns the task pane.
func (c *ContentPane) TaskPane() *TaskPane {
	return c.taskPane
}

// HooksPane returns the hooks pane.
func (c *ContentPane) HooksPane() *HooksPane {
	return c.hooksPane
}

// ScrollUp scrolls the active pane up. Mirrors the Terminal/Preview routing
// pattern: Tasks and Hooks modes each delegate to their own pane's cursor
// movement so shift+up and mouse wheel work in those modes too (#524).
func (c *ContentPane) ScrollUp() {
	switch c.mode {
	case ContentModeInstance:
		c.tabbedWindow.ScrollUp()
	case ContentModeTasks:
		c.taskPane.ScrollUp()
	case ContentModeHooks:
		c.hooksPane.ScrollUp()
	}
}

// ScrollDown scrolls the active pane down.
func (c *ContentPane) ScrollDown() {
	switch c.mode {
	case ContentModeInstance:
		c.tabbedWindow.ScrollDown()
	case ContentModeTasks:
		c.taskPane.ScrollDown()
	case ContentModeHooks:
		c.hooksPane.ScrollDown()
	}
}

// UpdatePreview delegates to TabbedWindow.
func (c *ContentPane) UpdatePreview(instance *session.Instance) error {
	if c.mode != ContentModeInstance {
		return nil
	}
	return c.tabbedWindow.UpdatePreview(instance)
}

// UpdateTerminal delegates to TabbedWindow.
func (c *ContentPane) UpdateTerminal(instance *session.Instance) error {
	if c.mode != ContentModeInstance {
		return nil
	}
	return c.tabbedWindow.UpdateTerminal(instance)
}

// String renders the content pane.
func (c *ContentPane) String() string {
	switch c.mode {
	case ContentModeInstance:
		return c.tabbedWindow.String()
	case ContentModeTasks:
		return c.renderInlinePane(c.taskPane.String())
	case ContentModeHooks:
		return c.renderInlinePane(c.hooksPane.String())
	default:
		return c.renderEmptyPane()
	}
}

func (c *ContentPane) renderInlinePane(content string) string {
	w := AdjustPreviewWidth(c.width)
	if w <= 0 || c.height <= 0 {
		return ""
	}

	innerWidth := w - windowStyle.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}
	// The -2 budgets for the two spacer lines the JoinVertical("\n", ...)
	// below emits, mirroring TabbedWindow.String's height math so inline
	// panes line up with the tabbed window.
	innerHeight := c.height - windowStyle.GetVerticalFrameSize() - 2
	if innerHeight < 0 {
		innerHeight = 0
	}

	// lipgloss.Place pads short content but never truncates or wraps:
	// over-tall content pushes the menu and error box off-screen (#700) and
	// over-wide lines stretch the border box past the allocation. Wrap to the
	// frame width first, then clamp to the height budget.
	wrapped := strings.Split(lipgloss.NewStyle().Width(innerWidth).Render(content), "\n")
	if len(wrapped) > innerHeight {
		wrapped = wrapped[:innerHeight]
	}

	// windowStyle is bordered, and Style.Width sets the *inner* width — so
	// .Width(w) here rendered w+frame total columns, 2 wider than allocated
	// (#821). Match TabbedWindow.String: size the content via lipgloss.Place
	// and let the border add the frame on top.
	window := windowStyle.Render(
		lipgloss.Place(
			innerWidth, innerHeight,
			lipgloss.Left, lipgloss.Top,
			strings.Join(wrapped, "\n")))

	return lipgloss.JoinVertical(lipgloss.Left, "\n", window)
}

func (c *ContentPane) renderEmptyPane() string {
	emptyStyle := lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

	return c.renderInlinePane(
		emptyStyle.Render(strings.Repeat("\n", 3) + "  Select an item from the sidebar"))
}
