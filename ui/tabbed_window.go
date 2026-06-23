package ui

import (
	"fmt"
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

// defaultTabLabels are the labels shown before an instance is selected, or for
// one whose tabs haven't materialized yet. They preserve the exact pre-#930
// two-tab bar so the UX is identical: slot 0 is the agent ("Preview") tab, slot
// 1 the terminal tab.
var defaultTabLabels = []string{"Preview", "Terminal"}

// TabbedWindow has tabs at the top of a pane which can be selected. The tabs
// take up one rune of height. The tab list is sourced from the selected
// instance's Tabs (#930 PR 2): every tab renders through one shared TabPane by
// capturing the selected tab's session.
type TabbedWindow struct {
	// activeTab is read from the background refreshPanesCmd goroutine
	// (UpdateContent) while the bubbletea event loop writes it via
	// Toggle/ToggleBack, so it is an atomic to avoid a data race (#684).
	activeTab atomic.Int32
	height    int
	width     int

	tab      *TabPane
	instance *session.Instance
}

func NewTabbedWindow(tab *TabPane) *TabbedWindow {
	return &TabbedWindow{
		tab: tab,
	}
}

func (w *TabbedWindow) SetInstance(instance *session.Instance) {
	w.instance = instance
	// Tab counts vary per instance (#930 PR 4): switching to an instance with
	// fewer tabs must not leave activeTab pointing past the end, which would make
	// isAgentSlot() lie and the number/Toggle math operate on a phantom slot.
	w.clampActiveTab()
}

// clampActiveTab bounds activeTab into [0, len(tabLabels())-1]. Called whenever
// the tab set may have shrunk (instance switch, tab close) so the index never
// dangles out of range.
func (w *TabbedWindow) clampActiveTab() {
	n := int32(len(w.tabLabels()))
	if n <= 0 {
		w.activeTab.Store(0)
		return
	}
	if cur := w.activeTab.Load(); cur >= n {
		w.activeTab.Store(n - 1)
	} else if cur < 0 {
		w.activeTab.Store(0)
	}
}

// JumpToTab selects the tab at the 0-based idx, returning true if it exists.
// Out-of-range indices are a no-op (false) so an unused number key does nothing
// (#930 PR 4).
func (w *TabbedWindow) JumpToTab(idx int) bool {
	if idx < 0 || idx >= len(w.tabLabels()) {
		return false
	}
	w.activeTab.Store(int32(idx))
	return true
}

// SelectTab sets the active tab to idx, clamped into range. Used after a tab
// close to land on a neighbor.
func (w *TabbedWindow) SelectTab(idx int) {
	w.activeTab.Store(int32(idx))
	w.clampActiveTab()
}

// SelectLastTab selects the final tab. Used after a new tab is appended so the
// freshly created tab becomes active (#930 PR 4).
func (w *TabbedWindow) SelectLastTab() {
	w.SelectTab(len(w.tabLabels()) - 1)
}

// tabLabels returns the labels for the current instance's tabs. Agent tabs
// render as "Preview", shell tabs as "Terminal"; any Process tab renders under
// its own name.
//
// Remote instances are tab-driven too (#930 PR 6): their real tab set is the
// agent tab plus a terminal tab only when terminal_cmd is configured, so the
// bar reflects exactly those tabs — a terminal_cmd-less remote shows a single
// tab rather than the local two-tab default. Local instances keep the
// default-padded bar (always at least the two slots) so the header count never
// dips below two mid-start, identical to the pre-#930 UX.
func (w *TabbedWindow) tabLabels() []string {
	if w.instance != nil && w.instance.IsRemote() {
		if tabs := w.instance.GetTabs(); len(tabs) > 0 {
			labels := make([]string, len(tabs))
			for i, tab := range tabs {
				labels[i] = labelForTab(tab)
			}
			return labels
		}
		// Pre-start remote (no tabs yet): fall through to the default bar.
	}

	labels := append([]string(nil), defaultTabLabels...)
	if w.instance == nil {
		return labels
	}
	for idx, tab := range w.instance.GetTabs() {
		label := labelForTab(tab)
		if idx < len(labels) {
			labels[idx] = label
		} else {
			labels = append(labels, label)
		}
	}
	return labels
}

func labelForTab(tab *session.Tab) string {
	switch tab.Kind {
	case session.TabKindAgent:
		return "Preview"
	case session.TabKindShell:
		return "Terminal"
	default:
		if tab.Name != "" {
			return tab.Name
		}
		return "Tab"
	}
}

// isAgentSlot reports whether the active tab index is the agent tab (index 0).
// Index 0 is always the agent tab; every other slot is a shell/terminal tab.
func (w *TabbedWindow) isAgentSlot() bool {
	return int(w.activeTab.Load()) == 0
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
	n := int32(len(w.tabLabels()))
	if n <= 0 {
		return
	}
	w.activeTab.Store((w.activeTab.Load() + 1) % n)
}

func (w *TabbedWindow) ToggleBack() {
	n := int32(len(w.tabLabels()))
	if n <= 0 {
		return
	}
	w.activeTab.Store((w.activeTab.Load() - 1 + n) % n)
}

// UpdateContent updates the content of the active tab's pane. instance may be
// nil. Replaces the former UpdatePreview/UpdateTerminal split now that one pane
// renders whichever tab is selected.
func (w *TabbedWindow) UpdateContent(instance *session.Instance) error {
	return w.tab.UpdateContent(instance, int(w.activeTab.Load()))
}

// ResetToNormalMode resets the active tab's pane to normal (non-scroll) mode.
func (w *TabbedWindow) ResetToNormalMode(instance *session.Instance) error {
	return w.tab.ResetToNormalMode(instance, int(w.activeTab.Load()))
}

func (w *TabbedWindow) ScrollUp() {
	if err := w.tab.ScrollUp(w.instance, int(w.activeTab.Load())); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll up: %v", err)
	}
}

func (w *TabbedWindow) ScrollDown() {
	if err := w.tab.ScrollDown(w.instance, int(w.activeTab.Load())); err != nil {
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
	return int(w.activeTab.Load())
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
	if len(labels) == 0 {
		labels = defaultTabLabels
	}

	var renderedTabs []string

	activeTab := int(w.activeTab.Load())
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
