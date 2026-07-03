package ui

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

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
	inactiveTabStyle  = lipgloss.NewStyle().
				Border(inactiveTabBorder, true).
				BorderForeground(AccentColor).
				AlignHorizontal(lipgloss.Center)
	activeTabStyle = inactiveTabStyle.
			Border(activeTabBorder, true).
			AlignHorizontal(lipgloss.Center)
	windowStyle = lipgloss.NewStyle().
			BorderForeground(AccentColor).
			Border(lipgloss.NormalBorder(), false, true, true, true)
)

// TabbedWindow has tabs at the top of a pane which can be selected. The tabs
// take up one rune of height. The tab list is sourced from the selected
// instance's Tabs (#930 PR 2): every tab renders through one shared TabPane by
// capturing the selected tab's session.
//
// The window is a VIEW over the store.Projection (#1024 PR 2): both the
// selected instance and the active tab index live in the store, so the window
// no longer holds an instance pointer of its own. The active tab is an atomic
// in the store because the background refreshPanesCmd goroutine reads it via
// UpdateContent while the bubbletea event loop writes it via Toggle/ToggleBack
// (#684); every other store read here happens on the event loop only.
type TabbedWindow struct {
	proj   *store.Projection
	height int
	width  int

	tab *TabPane
}

func NewTabbedWindow(tab *TabPane, proj *store.Projection) *TabbedWindow {
	return &TabbedWindow{
		proj: proj,
		tab:  tab,
	}
}

// selectedInstance returns the instance the window renders — the store's
// (sticky) display selection. Event-loop only.
func (w *TabbedWindow) selectedInstance() *session.Instance {
	return w.proj.GetSelectedInstance()
}

// ClampActiveTab bounds the active tab index into [0, len(tabLabels())-1].
// Called whenever the tab set may have shrunk (instance switch, tab close) so
// the index never dangles out of range: tab counts vary per instance (#930
// PR 4), and switching to an instance with fewer tabs must not leave the
// index pointing past the end, which would make isAgentSlot() lie and the
// number/Toggle math operate on a phantom slot.
func (w *TabbedWindow) ClampActiveTab() {
	n := len(w.tabLabels())
	if n <= 0 {
		w.proj.SetActiveTab(0)
		return
	}
	if cur := w.proj.ActiveTab(); cur >= n {
		w.proj.SetActiveTab(n - 1)
	} else if cur < 0 {
		w.proj.SetActiveTab(0)
	}
}

// JumpToTab selects the tab at the 0-based idx, returning true if it exists.
// Out-of-range indices are a no-op (false) so an unused number key does nothing
// (#930 PR 4).
func (w *TabbedWindow) JumpToTab(idx int) bool {
	if idx < 0 || idx >= len(w.tabLabels()) {
		return false
	}
	w.proj.SetActiveTab(idx)
	return true
}

// SelectTab sets the active tab to idx, clamped into range. Used after a tab
// close to land on a neighbor.
func (w *TabbedWindow) SelectTab(idx int) {
	w.proj.SetActiveTab(idx)
	w.ClampActiveTab()
}

// SelectLastTab selects the final tab. Used after a new tab is appended so the
// freshly created tab becomes active (#930 PR 4).
func (w *TabbedWindow) SelectLastTab() {
	w.SelectTab(len(w.tabLabels()) - 1)
}

// tabLabels returns the labels for the selected instance's tabs. The label
// derivation lives in tree.TabLabels (#1024 PR 3) — the single source of truth
// shared with the sidebar tree, so the bar, the tree's child rows, and the 1-9
// jump keys always agree on slot numbering. Never empty.
func (w *TabbedWindow) tabLabels() []string {
	return tree.TabLabels(w.selectedInstance())
}

// isAgentSlot reports whether the active tab index is the agent tab (index 0).
// Index 0 is always the agent tab; every other slot is a shell/terminal tab.
func (w *TabbedWindow) isAgentSlot() bool {
	return w.proj.ActiveTab() == 0
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

	// Clamp to zero so tiny terminals don't produce negative dimensions, which
	// would otherwise overflow when later cast to uint16 (e.g. by pty.Setsize).
	if contentHeight < 0 {
		contentHeight = 0
	}
	if contentWidth < 0 {
		contentWidth = 0
	}

	w.tab.SetSize(contentWidth, contentHeight)
}

func (w *TabbedWindow) GetPreviewSize() (width, height int) {
	return w.tab.width, w.tab.height
}

func (w *TabbedWindow) Toggle() {
	n := len(w.tabLabels())
	if n <= 0 {
		return
	}
	w.proj.SetActiveTab((w.proj.ActiveTab() + 1) % n)
}

func (w *TabbedWindow) ToggleBack() {
	n := len(w.tabLabels())
	if n <= 0 {
		return
	}
	w.proj.SetActiveTab((w.proj.ActiveTab() - 1 + n) % n)
}

// UpdateContent updates the content of the active tab's pane. instance may be
// nil. It is called from the refreshPanesCmd goroutine with the instance
// captured on the event loop at dispatch time — deliberately a parameter, not
// a store read, so the capture is keyed to the selection the refresh was
// dispatched for; only the active tab index is read here, through the store's
// atomic (#684).
func (w *TabbedWindow) UpdateContent(instance *session.Instance) error {
	return w.tab.UpdateContent(instance, w.proj.ActiveTab())
}

// ResetToNormalMode resets the active tab's pane to normal (non-scroll) mode.
func (w *TabbedWindow) ResetToNormalMode(instance *session.Instance) error {
	return w.tab.ResetToNormalMode(instance, w.proj.ActiveTab())
}

func (w *TabbedWindow) ScrollUp() {
	if err := w.tab.ScrollUp(w.selectedInstance(), w.proj.ActiveTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll up: %v", err)
	}
}

func (w *TabbedWindow) ScrollDown() {
	if err := w.tab.ScrollDown(w.selectedInstance(), w.proj.ActiveTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll down: %v", err)
	}
}

// IsInPreviewTab returns true if the agent (Preview) tab is currently active.
func (w *TabbedWindow) IsInPreviewTab() bool {
	return w.isAgentSlot()
}

// IsInTerminalTab returns true if a non-agent (terminal) tab is currently
// active.
func (w *TabbedWindow) IsInTerminalTab() bool {
	return !w.isAgentSlot()
}

// GetActiveTab returns the currently active tab index.
func (w *TabbedWindow) GetActiveTab() int {
	return w.proj.ActiveTab()
}

// AttachTerminalForInstance attaches to the terminal (shell) tab of the given
// instance. Capturing the instance — rather than re-reading the live selection
// — keeps deferred attach flows safe from selection drift while the help
// overlay is open (#716): the shell session belongs to this instance, so there
// is no title-keyed cache to drift. Remote instances route to the terminal_cmd
// hook (#843).
func (w *TabbedWindow) AttachTerminalForInstance(instance *session.Instance, tabIdx int) (chan struct{}, error) {
	if instance == nil {
		return nil, fmt.Errorf("no terminal session to attach to")
	}
	if instance.IsRemote() {
		if !instance.SupportsRemoteTerminal() {
			return nil, fmt.Errorf("remote terminal is not configured: add a terminal_cmd to remote_hooks to enable the Terminal tab for remote sessions")
		}
		return instance.AttachRemoteTerminal()
	}
	return instance.AttachTab(tabIdx)
}

// IsInScrollMode returns true if the active tab's pane is in scroll mode.
func (w *TabbedWindow) IsInScrollMode() bool {
	return w.tab.IsScrolling()
}

func (w *TabbedWindow) String() string {
	if w.width == 0 || w.height == 0 {
		return ""
	}

	labels := w.tabLabels()

	var renderedTabs []string

	activeTab := w.proj.ActiveTab()
	totalTabWidth := w.width
	tabWidth := totalTabWidth / len(labels)
	lastTabWidth := totalTabWidth - tabWidth*(len(labels)-1)
	tabHeight := activeTabStyle.GetVerticalFrameSize() + 1 // padding/border/margin + 1 for char height

	for i, t := range labels {
		width := tabWidth
		if i == len(labels)-1 {
			width = lastTabWidth
		}

		var style lipgloss.Style
		isFirst, isLast, isActive := i == 0, i == len(labels)-1, i == activeTab
		if isActive {
			style = activeTabStyle
		} else {
			style = inactiveTabStyle
		}
		// A single tab is both first and last, so the bottom-left and
		// bottom-right corners must be decided independently — a mutually
		// exclusive if/else-if chain would set only BottomLeft and leave
		// BottomRight at its wrong default (#972).
		border, _, _, _, _ := style.GetBorder()
		if isFirst && isActive {
			border.BottomLeft = "│"
		} else if isFirst {
			border.BottomLeft = "├"
		}
		if isLast && isActive {
			border.BottomRight = "│"
		} else if isLast {
			border.BottomRight = "┤"
		}
		style = style.Border(border)
		style = style.Width(width - style.GetHorizontalFrameSize())
		renderedTabs = append(renderedTabs, style.Render(t))
	}

	row := lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
	content := w.tab.String()
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
