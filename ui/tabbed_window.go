package ui

import (
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/charmbracelet/lipgloss"
)

func tabBorderWithBottom(left, middle, right string) lipgloss.Border {
	border := lipgloss.RoundedBorder()
	border.BottomLeft = left
	border.Bottom = middle
	border.BottomRight = right
	return border
}

var (
	inactiveTabBorder = tabBorderWithBottom("┴", "─", "┴")
	activeTabBorder   = tabBorderWithBottom("┘", " ", "└")
	highlightColor    = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	inactiveTabStyle  = lipgloss.NewStyle().
				Border(inactiveTabBorder, true).
				BorderForeground(highlightColor).
				AlignHorizontal(lipgloss.Center)
	activeTabStyle = inactiveTabStyle.
			Border(activeTabBorder, true).
			AlignHorizontal(lipgloss.Center)
	windowStyle = lipgloss.NewStyle().
			BorderForeground(highlightColor).
			Border(lipgloss.NormalBorder(), false, true, true, true)
)

const (
	PreviewTab int = iota
	TerminalTab
)

// TabbedWindow has tabs at the top of a pane which can be selected. The tabs
// take up one rune of height.
type TabbedWindow struct {
	tabs []string

	// activeTab is read from the background refreshPanesCmd goroutine
	// (UpdatePreview/UpdateTerminal) while the bubbletea event loop writes it
	// via Toggle/ToggleBack, so it is an atomic to avoid a data race (#684).
	// The tab indices fit in an int32 (there are two tabs).
	activeTab atomic.Int32
	height    int
	width     int

	preview  *PreviewPane
	terminal *TerminalPane
	instance *session.Instance
}

func NewTabbedWindow(preview *PreviewPane, terminal *TerminalPane) *TabbedWindow {
	return &TabbedWindow{
		tabs:     []string{"Preview", "Terminal"},
		preview:  preview,
		terminal: terminal,
	}
}

func (w *TabbedWindow) SetInstance(instance *session.Instance) {
	w.instance = instance
}

// AdjustPreviewWidth adjusts the width of the preview pane to be 90% of the provided width.
func AdjustPreviewWidth(width int) int {
	return int(float64(width) * 0.9)
}

func (w *TabbedWindow) SetSize(width, height int) {
	w.width = AdjustPreviewWidth(width)
	w.height = height

	// Calculate the content height by subtracting:
	// 1. Tab height (including border and padding)
	// 2. Window style vertical frame size
	// 3. Additional padding/spacing (2 for the newline and spacing)
	tabHeight := activeTabStyle.GetVerticalFrameSize() + 1
	contentHeight := height - tabHeight - windowStyle.GetVerticalFrameSize() - 2
	contentWidth := w.width - windowStyle.GetHorizontalFrameSize()

	// Clamp to zero so tiny terminals don't produce negative dimensions,
	// which would otherwise overflow when later cast to uint16 (e.g. by
	// pty.Setsize for the tmux preview/terminal panes).
	if contentHeight < 0 {
		contentHeight = 0
	}
	if contentWidth < 0 {
		contentWidth = 0
	}

	w.preview.SetSize(contentWidth, contentHeight)
	w.terminal.SetSize(contentWidth, contentHeight)
}

func (w *TabbedWindow) GetPreviewSize() (width, height int) {
	return w.preview.width, w.preview.height
}

func (w *TabbedWindow) Toggle() {
	n := int32(len(w.tabs))
	w.activeTab.Store((w.activeTab.Load() + 1) % n)
}

func (w *TabbedWindow) ToggleBack() {
	n := int32(len(w.tabs))
	w.activeTab.Store((w.activeTab.Load() - 1 + n) % n)
}

// UpdatePreview updates the content of the preview pane. instance may be nil.
func (w *TabbedWindow) UpdatePreview(instance *session.Instance) error {
	if int(w.activeTab.Load()) != PreviewTab {
		return nil
	}
	return w.preview.UpdateContent(instance)
}

// UpdateTerminal updates the terminal pane content. Only updates when terminal tab is active.
func (w *TabbedWindow) UpdateTerminal(instance *session.Instance) error {
	if int(w.activeTab.Load()) != TerminalTab {
		return nil
	}
	return w.terminal.UpdateContent(instance)
}

// ResetPreviewToNormalMode resets the preview pane to normal mode
func (w *TabbedWindow) ResetPreviewToNormalMode(instance *session.Instance) error {
	return w.preview.ResetToNormalMode(instance)
}

// Add these new methods for handling scroll events
func (w *TabbedWindow) ScrollUp() {
	switch int(w.activeTab.Load()) {
	case PreviewTab:
		err := w.preview.ScrollUp(w.instance)
		if err != nil {
			log.InfoLog.Printf("tabbed window failed to scroll up: %v", err)
		}
	case TerminalTab:
		if err := w.terminal.ScrollUp(w.instance); err != nil {
			log.InfoLog.Printf("tabbed window failed to scroll terminal up: %v", err)
		}
	}
}

func (w *TabbedWindow) ScrollDown() {
	switch int(w.activeTab.Load()) {
	case PreviewTab:
		err := w.preview.ScrollDown(w.instance)
		if err != nil {
			log.InfoLog.Printf("tabbed window failed to scroll down: %v", err)
		}
	case TerminalTab:
		if err := w.terminal.ScrollDown(w.instance); err != nil {
			log.InfoLog.Printf("tabbed window failed to scroll terminal down: %v", err)
		}
	}
}

// IsInPreviewTab returns true if the preview tab is currently active
func (w *TabbedWindow) IsInPreviewTab() bool {
	return int(w.activeTab.Load()) == PreviewTab
}

// IsInTerminalTab returns true if the terminal tab is currently active
func (w *TabbedWindow) IsInTerminalTab() bool {
	return int(w.activeTab.Load()) == TerminalTab
}

// GetActiveTab returns the currently active tab index
func (w *TabbedWindow) GetActiveTab() int {
	return int(w.activeTab.Load())
}

// AttachTerminalForInstance attaches to the terminal session of the given
// instance by binding the terminal pane to it first. Deferred attach flows
// must use this rather than re-reading the live selection: the terminal's
// bound instance (currentTitle) can be rebound by a background refresh while
// the help overlay is open (#716).
func (w *TabbedWindow) AttachTerminalForInstance(instance *session.Instance) (chan struct{}, error) {
	return w.terminal.AttachForInstance(instance)
}

// CleanupTerminal closes the terminal session
func (w *TabbedWindow) CleanupTerminal() {
	w.terminal.Close()
}

// CleanupTerminalForInstance closes the cached terminal session for the given instance title.
func (w *TabbedWindow) CleanupTerminalForInstance(title string) {
	w.terminal.CloseForInstance(title)
}

// IsPreviewInScrollMode returns true if the preview pane is in scroll mode
func (w *TabbedWindow) IsPreviewInScrollMode() bool {
	return w.preview.IsScrolling()
}

// IsTerminalInScrollMode returns true if the terminal pane is in scroll mode
func (w *TabbedWindow) IsTerminalInScrollMode() bool {
	return w.terminal.IsScrolling()
}

// ResetTerminalToNormalMode exits scroll mode on the terminal pane
func (w *TabbedWindow) ResetTerminalToNormalMode() {
	w.terminal.ResetToNormalMode()
}

func (w *TabbedWindow) String() string {
	if w.width == 0 || w.height == 0 {
		return ""
	}

	var renderedTabs []string

	activeTab := int(w.activeTab.Load())
	totalTabWidth := w.width
	tabWidth := totalTabWidth / len(w.tabs)
	lastTabWidth := totalTabWidth - tabWidth*(len(w.tabs)-1)
	tabHeight := activeTabStyle.GetVerticalFrameSize() + 1 // get padding border margin size + 1 for character height

	for i, t := range w.tabs {
		width := tabWidth
		if i == len(w.tabs)-1 {
			width = lastTabWidth
		}

		var style lipgloss.Style
		isFirst, isLast, isActive := i == 0, i == len(w.tabs)-1, i == activeTab
		if isActive {
			style = activeTabStyle
		} else {
			style = inactiveTabStyle
		}
		border, _, _, _, _ := style.GetBorder()
		if isFirst && isActive {
			border.BottomLeft = "│"
		} else if isFirst {
			border.BottomLeft = "├"
		} else if isLast && isActive {
			border.BottomRight = "│"
		} else if isLast {
			border.BottomRight = "┤"
		}
		style = style.Border(border)
		style = style.Width(width - style.GetHorizontalFrameSize())
		renderedTabs = append(renderedTabs, style.Render(t))
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	var content string
	switch activeTab {
	case PreviewTab:
		content = w.preview.String()
	case TerminalTab:
		content = w.terminal.String()
	}
	contentWidth := w.width - windowStyle.GetHorizontalFrameSize()
	if contentWidth < 0 {
		contentWidth = 0
	}
	window := windowStyle.Render(
		lipgloss.Place(
			contentWidth, w.height-2-windowStyle.GetVerticalFrameSize()-tabHeight,
			lipgloss.Left, lipgloss.Top, content))

	return lipgloss.JoinVertical(lipgloss.Left, "\n", row, window)
}
