package ui

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
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
// where the active tab is named; the full tab set lives in the left-rail tree.
const paneHeaderRows = 1

// TabbedWindow is the workspace content pane (pane A of RFC §2.1): a live
// capture-pane view of the selected instance's active tab, framed with a
// `title · tab` header. The tab BAR is gone as of the layout cutover (#1024
// PR 4) — tabs live in the left-rail tree and the 1-9 jump keys, and the
// header names the one being shown. Every tab renders through one shared
// TabPane by capturing the selected tab's session (#930 PR 2).
//
// The window is a VIEW over the store.Projection (#1024 PR 2): both the
// selected instance and the active tab index live in the store, so the window
// no longer holds an instance pointer of its own. The active tab is an atomic
// in the store because the background refreshPanesCmd goroutine reads it via
// UpdateContent while the bubbletea event loop writes it via the tab-jump
// handlers (#684); every other store read here happens on the event loop only.
//
// It implements layout.Pane: SetRect sizes the inner TabPane, and View()
// returns exactly Rect-sized output via layout.ClampToRect.
//
// The split view (#1024 PR 5) adds a second window, pane B, distinguished by
// the pinned flag: pane A renders the store's selection-driven binding
// (selected instance + active tab), pane B the store's PINNED pane-B binding,
// which only split verbs move. Everything else — frame, header, capture,
// scroll, clamp discipline — is one implementation, so the two panes can
// never drift apart in behavior.
type TabbedWindow struct {
	proj    *store.Projection
	rect    layout.Rect
	focused bool
	pinned  bool

	// zones is the shared mouse hit-test registry (#1024 PR 6); String()
	// registers the pane's body + header rects every frame. Nil skips.
	zones *zones.Registry

	tab *TabPane
}

// NewTabbedWindow creates the selection-driven workspace window (pane A).
func NewTabbedWindow(tab *TabPane, proj *store.Projection) *TabbedWindow {
	return &TabbedWindow{
		proj: proj,
		tab:  tab,
	}
}

// NewPinnedTabbedWindow creates the pinned split window (pane B): it renders
// the store's pane-B binding instead of the live tree selection (#1024 PR 5).
func NewPinnedTabbedWindow(tab *TabPane, proj *store.Projection) *TabbedWindow {
	return &TabbedWindow{
		proj:   proj,
		tab:    tab,
		pinned: true,
	}
}

// boundInstance returns the instance the window renders: the store's (sticky)
// display selection for pane A, the pinned pane-B binding for pane B.
// Event-loop only.
func (w *TabbedWindow) boundInstance() *session.Instance {
	if w.pinned {
		return w.proj.PaneBInstance()
	}
	return w.proj.GetSelectedInstance()
}

// activeTab returns the window's 0-based tab index through the store's
// role-matching atomic (#684): pane A follows the shared active tab, pane B
// its pinned tab. Safe from the background capture goroutine.
func (w *TabbedWindow) activeTab() int {
	if w.pinned {
		return w.proj.PaneBTab()
	}
	return w.proj.ActiveTab()
}

// setActiveTab writes the window's tab index to the role-matching store slot.
func (w *TabbedWindow) setActiveTab(idx int) {
	if w.pinned {
		w.proj.SetPaneBTab(idx)
		return
	}
	w.proj.SetActiveTab(idx)
}

// ClampActiveTab bounds the active tab index into [0, len(tabLabels())-1].
// Called whenever the tab set may have shrunk (instance switch, tab close) so
// the index never dangles out of range: tab counts vary per instance (#930
// PR 4), and switching to an instance with fewer tabs must not leave the
// index pointing past the end, which would make isAgentSlot() lie and the
// number-jump math operate on a phantom slot.
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

// JumpToTab selects the tab at the 0-based idx, returning true if it exists.
// Out-of-range indices are a no-op (false) so an unused number key does nothing
// (#930 PR 4).
func (w *TabbedWindow) JumpToTab(idx int) bool {
	if idx < 0 || idx >= len(w.tabLabels()) {
		return false
	}
	w.setActiveTab(idx)
	return true
}

// SelectTab sets the active tab to idx, clamped into range. Used after a tab
// close to land on a neighbor.
func (w *TabbedWindow) SelectTab(idx int) {
	w.setActiveTab(idx)
	w.ClampActiveTab()
}

// SelectLastTab selects the final tab. Used after a new tab is appended so the
// freshly created tab becomes active (#930 PR 4).
func (w *TabbedWindow) SelectLastTab() {
	w.SelectTab(len(w.tabLabels()) - 1)
}

// tabLabels returns the labels for the selected instance's tabs. The label
// derivation lives in tree.TabLabels (#1024 PR 3) — the single source of truth
// shared with the sidebar tree, so the pane header, the tree's child rows, and
// the 1-9 jump keys always agree on slot numbering. Never empty.
func (w *TabbedWindow) tabLabels() []string {
	return tree.TabLabels(w.boundInstance())
}

// isAgentSlot reports whether the active tab index is the agent tab (index 0).
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
// globally in the single-pane layout (scroll, attach, tab jumps), so the pane
// itself consumes nothing yet; per-pane key handling arrives with the split
// (#1024 PR 5).
func (w *TabbedWindow) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 PR 6): the body/header zones registered by String() resolve to
// focus/attach/scroll actions there, so the pane-local fallback consumes
// nothing.
func (w *TabbedWindow) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// UpdateContent updates the content of the active tab's pane. instance may be
// nil. It is called from the refreshPanesCmd goroutine with the instance
// captured on the event loop at dispatch time — deliberately a parameter, not
// a store read, so the capture is keyed to the selection the refresh was
// dispatched for; only the active tab index is read here, through the store's
// atomic (#684).
func (w *TabbedWindow) UpdateContent(instance *session.Instance) error {
	return w.tab.UpdateContent(instance, w.activeTab())
}

// ResetToNormalMode resets the active tab's pane to normal (non-scroll) mode.
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
	return w.activeTab()
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

// renderHeader renders the one-line `title · tab` header. The header is the
// only place the active tab is named inside the pane (the tab bar is gone);
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

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 PR 6).
func (w *TabbedWindow) SetZoneRegistry(reg *zones.Registry) {
	w.zones = reg
}

// Region returns the layout region id this window renders as: pane B when
// pinned, pane A otherwise. Doubles as the pane component of its zone ids.
func (w *TabbedWindow) Region() string {
	if w.pinned {
		return layout.RegionPaneB
	}
	return layout.RegionPaneA
}

// registerZones records this frame's hit-test rects: the whole pane as the
// body (click focuses; click focused attaches; wheel scrolls), with the
// one-line `title · tab` header registered on top of it (click focuses the
// pane). The header sits inside the frame border, so it needs at least a
// 3×3 rect to exist.
func (w *TabbedWindow) registerZones() {
	if w.zones == nil || w.rect.Empty() {
		return
	}
	region := w.Region()
	w.zones.Register(zones.PaneBody(region), w.rect)
	if w.rect.W > 2 && w.rect.H > 2 {
		w.zones.Register(zones.PaneHeader(region), layout.Rect{
			X: w.rect.X + 1, Y: w.rect.Y + 1, W: w.rect.W - 2, H: paneHeaderRows,
		})
	}
}

func (w *TabbedWindow) String() string {
	if w.rect.Empty() {
		return ""
	}
	w.registerZones()
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
