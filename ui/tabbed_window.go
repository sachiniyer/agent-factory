package ui

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var windowStyle = lipgloss.NewStyle().
	BorderForeground(AccentColor).
	Border(lipgloss.RoundedBorder())

// blurredWindowStyle recedes the pane frame when focus is elsewhere in the
// workspace, so the focus ring is legible at a glance.
var blurredWindowStyle = windowStyle.
	BorderForeground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#555555"})

var paneHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var paneHeaderFocusedStyle = lipgloss.NewStyle().
	Bold(true).
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.Color("#1a1a1a"))

var paneHeaderDimStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

// paneHeaderRows is the height of the `title · tab` header line rendered
// inside the pane frame. With the tab bar gone (#1024 PR 4) the header is
// where the pane's tab is named; the full tab set lives in the left-rail tree.
const paneHeaderRows = 1

// TabbedWindow is one workspace content pane (RFC §2.1, #1088): a live
// capture-pane view of one open pane's (instance, tab) binding, framed with a
// `title · tab` header. The tab BAR is gone as of the layout cutover (#1024
// PR 4) — tabs live in the left-rail tree and the 1-9 jump keys, and the
// header names the one being shown. The window renders through one TabPane by
// capturing the bound tab's session (#930 PR 2).
//
// The binding is a store.OpenPane (#1024 PR 2/#1088): the (instance, tab)
// lives in the store's open-pane list — which the pane verbs move and
// ReplaceInstance re-points — so the window holds no instance pointer of its
// own. The pane's tab index is an atomic in the store because the background
// refreshPanesCmd goroutine reads it via UpdateContent while the bubbletea
// event loop writes it (#684); every other store read here happens on the
// event loop only.
//
// It implements layout.Pane: SetRect sizes the inner TabPane, and View()
// returns exactly Rect-sized output via layout.ClampToRect. The root model
// keeps one window per open pane, so scroll state stays per pane.
type TabbedWindow struct {
	pane    *store.OpenPane
	rect    layout.Rect
	focused bool

	tab *TabPane
}

// NewTabbedWindow creates a workspace window over the given open pane's
// binding. pane may be nil for an unbound window (rendered as the
// no-session placeholder); production windows are always bound.
func NewTabbedWindow(tab *TabPane, pane *store.OpenPane) *TabbedWindow {
	return &TabbedWindow{
		pane: pane,
		tab:  tab,
	}
}

// boundInstance returns the instance the window renders. Event-loop only.
func (w *TabbedWindow) boundInstance() *session.Instance {
	if w.pane == nil {
		return nil
	}
	return w.pane.Instance()
}

// activeTab returns the window's 0-based tab index through the pane's atomic
// (#684). Safe from the background capture goroutine.
func (w *TabbedWindow) activeTab() int {
	if w.pane == nil {
		return 0
	}
	return w.pane.Tab()
}

// setActiveTab writes the window's tab index to its pane binding.
func (w *TabbedWindow) setActiveTab(idx int) {
	if w.pane == nil {
		return
	}
	w.pane.SetTab(idx)
}

// ClampActiveTab bounds the pane's tab index into [0, len(tabLabels())-1].
// Called whenever the tab set may have shrunk (tab close, snapshot reconcile)
// so the index never dangles out of range: tab counts vary per instance (#930
// PR 4), and a dangling index would make isAgentSlot() lie and the number-jump
// math operate on a phantom slot.
func (w *TabbedWindow) ClampActiveTab() {
	n := len(w.tabLabels())
	if n <= 0 {
		w.setActiveTab(0)
		return
	}
	if cur := w.activeTab(); cur >= n {
		w.setActiveTab(n - 1)
	} else if cur < 0 {
		w.setActiveTab(0)
	}
}

// JumpToTab retargets the pane to the tab at the 0-based idx, returning true
// if it exists. Out-of-range indices are a no-op (false) so an unused number
// key does nothing (#930 PR 4).
func (w *TabbedWindow) JumpToTab(idx int) bool {
	if idx < 0 || idx >= len(w.tabLabels()) {
		return false
	}
	w.setActiveTab(idx)
	return true
}

// SelectTab sets the pane's tab to idx, clamped into range. Used after a tab
// close to land on a neighbor.
func (w *TabbedWindow) SelectTab(idx int) {
	w.setActiveTab(idx)
	w.ClampActiveTab()
}

// tabLabels returns the labels for the bound instance's tabs. The label
// derivation lives in tree.TabLabels (#1024 PR 3) — the single source of truth
// shared with the sidebar tree, so the pane header, the tree's child rows, and
// the 1-9 jump keys always agree on slot numbering. Never empty.
func (w *TabbedWindow) tabLabels() []string {
	return tree.TabLabels(w.boundInstance())
}

// isAgentSlot reports whether the pane's tab index is the agent tab (index 0).
// Index 0 is always the agent tab; every other slot is a shell/terminal tab.
func (w *TabbedWindow) isAgentSlot() bool {
	return w.activeTab() == 0
}

// SetRect implements layout.Pane: the pane renders exactly r. The inner
// TabPane gets the area inside the frame minus the header line; that inner
// size is also what the instances' tmux sessions are resized to (see
// GetPreviewSize), so the capture matches the visible area exactly — the full
// workspace width, with no AdjustPreviewWidth-style right buffer (#1024 PR 4).
func (w *TabbedWindow) SetRect(r layout.Rect) {
	w.rect = r
	iw, ih := w.innerSize()
	w.tab.SetSize(iw, ih)
}

// innerSize is the TabPane content area: the rect minus the frame and header.
func (w *TabbedWindow) innerSize() (width, height int) {
	width = w.rect.W - windowStyle.GetHorizontalFrameSize()
	height = w.rect.H - windowStyle.GetVerticalFrameSize() - paneHeaderRows
	// Clamp to zero so tiny terminals don't produce negative dimensions, which
	// would otherwise overflow when later cast to uint16 (e.g. by pty.Setsize).
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	return width, height
}

// GetPreviewSize returns the size tmux sessions should render at — the
// TabPane's content area.
func (w *TabbedWindow) GetPreviewSize() (width, height int) {
	return w.tab.width, w.tab.height
}

// Focused implements layout.Pane.
func (w *TabbedWindow) Focused() bool { return w.focused }

// Focus implements layout.Pane.
func (w *TabbedWindow) Focus() { w.focused = true }

// Blur implements layout.Pane.
func (w *TabbedWindow) Blur() { w.focused = false }

// HandleKey implements layout.Pane. The root model routes all workspace keys
// globally in nav mode (scroll, attach, pane verbs), so the pane itself
// consumes nothing; interactive-mode key forwarding is #1089.
func (w *TabbedWindow) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Mouse support is #1024 PR 6.
func (w *TabbedWindow) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// UpdateContent updates the content of the pane's tab view. instance may be
// nil. It is called from the refreshPanesCmd goroutine with the instance
// captured on the event loop at dispatch time — deliberately a parameter, not
// a store read, so the capture is keyed to the binding the refresh was
// dispatched for; only the tab index is read here, through the pane's
// atomic (#684).
func (w *TabbedWindow) UpdateContent(instance *session.Instance) error {
	return w.tab.UpdateContent(instance, w.activeTab())
}

// ResetToNormalMode resets the pane's tab view to normal (non-scroll) mode.
func (w *TabbedWindow) ResetToNormalMode(instance *session.Instance) error {
	return w.tab.ResetToNormalMode(instance, w.activeTab())
}

func (w *TabbedWindow) ScrollUp() {
	if err := w.tab.ScrollUp(w.boundInstance(), w.activeTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll up: %v", err)
	}
}

func (w *TabbedWindow) ScrollDown() {
	if err := w.tab.ScrollDown(w.boundInstance(), w.activeTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll down: %v", err)
	}
}

// IsInPreviewTab returns true if the agent (Preview) tab is the pane's tab.
func (w *TabbedWindow) IsInPreviewTab() bool {
	return w.isAgentSlot()
}

// IsInTerminalTab returns true if a non-agent (terminal) tab is the pane's
// tab.
func (w *TabbedWindow) IsInTerminalTab() bool {
	return !w.isAgentSlot()
}

// GetActiveTab returns the pane's current tab index.
func (w *TabbedWindow) GetActiveTab() int {
	return w.activeTab()
}

// AttachTerminalTab attaches to a terminal (shell) tab of the given instance
// full-screen. Capturing the instance — rather than re-reading the live
// selection — keeps deferred attach flows safe from selection drift while the
// help overlay is open (#716): the shell session belongs to this instance, so
// there is no title-keyed cache to drift. Remote instances route to the
// terminal_cmd hook (#843).
func AttachTerminalTab(instance *session.Instance, tabIdx int) (chan struct{}, error) {
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

// IsInScrollMode returns true if the pane's tab view is in scroll mode.
func (w *TabbedWindow) IsInScrollMode() bool {
	return w.tab.IsScrolling()
}

// renderHeader renders the one-line `title · tab` header. The header is the
// only place the pane's tab is named inside the pane (the tab bar is gone);
// the highlight doubles as the pane's focus indicator.
func (w *TabbedWindow) renderHeader(width int) string {
	var text string
	if inst := w.boundInstance(); inst != nil {
		labels := w.tabLabels()
		idx := w.activeTab()
		label := ""
		if idx >= 0 && idx < len(labels) {
			label = labels[idx]
		}
		text = fmt.Sprintf(" %s · %s ", inst.Title, label)
	} else {
		text = " no session selected "
	}
	style := paneHeaderStyle
	switch {
	case w.focused:
		style = paneHeaderFocusedStyle
	case w.boundInstance() == nil:
		style = paneHeaderDimStyle
	}
	header := style.Render(text)
	return layout.ClampToRect(header, layout.Rect{W: width, H: paneHeaderRows})
}

// View implements layout.Pane: exactly rect-sized output — the framed header +
// live capture view.
func (w *TabbedWindow) View() string { return w.String() }

func (w *TabbedWindow) String() string {
	if w.rect.Empty() {
		return ""
	}
	iw, ih := w.innerSize()
	inner := lipgloss.JoinVertical(lipgloss.Left,
		w.renderHeader(iw),
		layout.ClampToRect(w.tab.String(), layout.Rect{W: iw, H: ih}),
	)
	frame := windowStyle
	if !w.focused {
		frame = blurredWindowStyle
	}
	return layout.ClampToRect(frame.Render(inner), w.rect)
}
