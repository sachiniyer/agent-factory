package ui

import (
	"fmt"
	"sync/atomic"

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

// interactiveWindowStyle marks the pane that owns the keyboard in
// interactive mode (#1089, RFC §2.3): a green DOUBLE border — unmistakably
// distinct from the teal rounded nav-focus ring, and still distinct with
// colors unavailable — signals "keystrokes go INTO this terminal".
var interactiveWindowStyle = windowStyle.
	Border(lipgloss.DoubleBorder()).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"})

var paneHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var paneHeaderFocusedStyle = lipgloss.NewStyle().
	Bold(true).
	Background(lipgloss.Color("#dde4f0")).
	Foreground(lipgloss.Color("#1a1a1a"))

var paneHeaderDimStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

// paneHeaderInteractiveStyle matches the interactive frame: green header bar
// on the pane whose terminal owns the keyboard.
var paneHeaderInteractiveStyle = lipgloss.NewStyle().
	Bold(true).
	Background(lipgloss.AdaptiveColor{Light: "#D2F3DC", Dark: "#1A7F37"}).
	Foreground(lipgloss.AdaptiveColor{Light: "#0A3D1E", Dark: "#EAFBEF"})

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
	// interactive marks this pane as the one whose embedded terminal owns
	// the keyboard (#1089, RFC §2.3). Set by the root model alongside the
	// mode flag; drives the green frame/header cue and the cursor overlay.
	interactive bool

	tab *TabPane

	// region is the pane's layout/focus-ring region id
	// (layout.PaneRegion(paneID)), doubling as the pane component of its
	// mouse zone ids; zones is the shared hit-test registry (#1024 R4).
	// String() registers the body/header (and, while the live view renders,
	// the terminal grid) every frame. Either unset skips registration.
	region string
	zones  *zones.Registry

	// live is the pane's embedded-terminal render source (#1089 PR 1), set
	// by the root model while a termpane attachment is bound to this pane's
	// (instance, tab) and nil otherwise. While set (and the user is not in
	// scroll mode), String() renders the live grid instead of the TabPane's
	// capture — the capture polling for this pane is skipped by the root
	// model for the same duration. Event-loop only.
	live LiveView

	// selectionHint names the current tree/sidebar selection when it differs
	// from this pane's explicit binding. Open panes are intentionally not
	// selection-driven, so the header makes that divergence visible instead of
	// leaving the selected row and displayed content to appear contradictory
	// (#1289).
	selectionHint string

	// preview is a transient render binding for #1321. While set, the window
	// still owns its committed store.OpenPane binding, but capture/render uses
	// preview.instance + preview.tab and the header makes that explicit.
	preview *windowPreview
	// contentSeq invalidates off-loop captures whose binding was superseded
	// while they were in flight. The root model captures a sequence on the event
	// loop and TabPane writes only if the sequence is still current.
	contentSeq atomic.Uint64
}

type windowPreview struct {
	instance *session.Instance
	tab      int
	original string
}

// LiveView is a live embedded-terminal render source (#1089): the termpane
// behind an open pane. It is an interface so ui tests can substitute a fake
// without a PTY. Both methods are event-loop only.
type LiveView interface {
	// Render returns the live grid as exactly height ANSI lines of exactly
	// width cells. showCursor overlays the terminal cursor — the
	// interactive-mode typing cue; nav-mode renders pass false.
	Render(width, height int, showCursor bool) string
	// Resize propagates a pane-geometry change to the underlying PTY and
	// emulator grid.
	Resize(width, height int)
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

// boundInstance returns the committed pane instance. Event-loop only.
func (w *TabbedWindow) boundInstance() *session.Instance {
	if w.pane == nil {
		return nil
	}
	return w.pane.Instance()
}

func (w *TabbedWindow) effectiveInstance() *session.Instance {
	if w.preview != nil {
		return w.preview.instance
	}
	return w.boundInstance()
}

// activeTab returns the window's 0-based tab index through the pane's atomic
// (#684). Safe from the background capture goroutine.
func (w *TabbedWindow) activeTab() int {
	if w.pane == nil {
		return 0
	}
	return w.pane.Tab()
}

func (w *TabbedWindow) effectiveTab() int {
	if w.preview != nil {
		return w.preview.tab
	}
	return w.activeTab()
}

// setActiveTab writes the window's tab index to its pane binding.
func (w *TabbedWindow) setActiveTab(idx int) {
	if w.pane == nil {
		return
	}
	if w.pane.Tab() == idx {
		return
	}
	w.pane.SetTab(idx)
	w.bumpContentSeq()
}

func (w *TabbedWindow) bumpContentSeq() uint64 {
	return w.contentSeq.Add(1)
}

// ContentSeq returns the current render-binding generation.
func (w *TabbedWindow) ContentSeq() uint64 {
	return w.contentSeq.Load()
}

// SetPreview applies a transient render binding without mutating the committed
// pane binding. original is rendered in the PREVIEW header so the reversible
// state is visible to the user.
func (w *TabbedWindow) SetPreview(instance *session.Instance, tab int, original string) uint64 {
	if w.preview != nil &&
		w.preview.instance == instance &&
		w.preview.tab == tab &&
		w.preview.original == original {
		return w.ContentSeq()
	}
	w.preview = &windowPreview{instance: instance, tab: tab, original: original}
	return w.bumpContentSeq()
}

// ClearPreview drops any transient render binding and returns the generation
// after the clear. It is intentionally a no-op when there is no preview so
// refresh throttling does not churn sequences.
func (w *TabbedWindow) ClearPreview() uint64 {
	if w.preview == nil {
		return w.ContentSeq()
	}
	w.preview = nil
	return w.bumpContentSeq()
}

// Previewing reports whether the window is currently rendering a transient
// binding.
func (w *TabbedWindow) Previewing() bool {
	return w.preview != nil
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

func tabLabelFor(inst *session.Instance, idx int) string {
	labels := tree.TabLabels(inst)
	if idx >= 0 && idx < len(labels) {
		return labels[idx]
	}
	return ""
}

// isAgentSlot reports whether the effective tab index is the agent tab (index 0).
// Index 0 is always the agent tab; every other slot is a shell/terminal tab.
func (w *TabbedWindow) isAgentSlot() bool {
	return w.effectiveTab() == 0
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
	// A zero rect means the pane is auto-hidden (§2.6): don't push a
	// degenerate winsize at the live attachment — the root model closes it
	// on its next sync, and shrinking the tmux session to ~1x1 in the
	// meantime would visibly disrupt the session for no reason.
	if w.live != nil && iw > 0 && ih > 0 {
		w.live.Resize(iw, ih)
	}
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

// SetLive binds (or, with nil, unbinds) the pane's embedded-terminal render
// source and sizes it to the current content area. The root model owns the
// termpane's lifecycle; the window only renders through it.
func (w *TabbedWindow) SetLive(v LiveView) {
	if w.live == v {
		return
	}
	w.live = v
	if v == nil {
		return
	}
	if iw, ih := w.innerSize(); iw > 0 && ih > 0 {
		v.Resize(iw, ih)
	}
}

// HasLive reports whether an embedded-terminal render source is bound. The
// root model skips capture-pane polling for the pane while true.
func (w *TabbedWindow) HasLive() bool { return w.live != nil }

// Focused implements layout.Pane.
func (w *TabbedWindow) Focused() bool { return w.focused }

// Focus implements layout.Pane.
func (w *TabbedWindow) Focus() { w.focused = true }

// Blur implements layout.Pane.
func (w *TabbedWindow) Blur() { w.focused = false }

// SetInteractive flags this pane's terminal as the keyboard owner
// (interactive mode, #1089). The root model keeps it true on at most one
// window at a time.
func (w *TabbedWindow) SetInteractive(on bool) { w.interactive = on }

// Interactive reports the interactive-mode flag.
func (w *TabbedWindow) Interactive() bool { return w.interactive }

// HandleKey implements layout.Pane. The root model routes all workspace keys
// globally in nav mode (scroll, attach, pane verbs), so the pane itself
// consumes nothing; interactive-mode key forwarding is #1089.
func (w *TabbedWindow) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 R4): the body/header/term zones registered by String() resolve
// to focus/interact/scroll/forward actions there, so the pane-local fallback
// consumes nothing.
func (w *TabbedWindow) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (w *TabbedWindow) SetZoneRegistry(reg *zones.Registry) {
	w.zones = reg
}

// SetRegion records the pane's layout region id (layout.PaneRegion(paneID))
// for zone registration. The root model sets it when the pane window is
// created; the pane id — and therefore the region — is stable for the
// window's whole life.
func (w *TabbedWindow) SetRegion(region string) {
	w.region = region
}

// SetSelectionHint annotates the pane header when the tree selection differs
// from the pane's explicit binding. Empty clears the annotation.
func (w *TabbedWindow) SetSelectionHint(title string) {
	w.selectionHint = title
}

// registerZones records this frame's hit-test rects: the whole pane as the
// body (click focuses; click focused interacts; wheel scrolls), the one-line
// `title · tab` header inside the frame on top of it, and — exactly while the
// live embedded terminal is what renders — the terminal content grid, whose
// zone-local coordinates ARE emulator grid cells (the interactive-mode
// forwarding target, RFC §2.5). liveShowing must equal the render branch
// String() takes so a scroll-mode pane never advertises a term zone.
func (w *TabbedWindow) registerZones(liveShowing bool) {
	if w.zones == nil || w.region == "" || w.rect.Empty() {
		return
	}
	w.zones.Register(zones.PaneBody(w.region), w.rect)
	if w.rect.W > 2 && w.rect.H > 2 {
		w.zones.Register(zones.PaneHeader(w.region), layout.Rect{
			X: w.rect.X + 1, Y: w.rect.Y + 1, W: w.rect.W - 2, H: paneHeaderRows,
		})
		if liveShowing {
			iw, ih := w.innerSize()
			w.zones.Register(zones.PaneTerm(w.region), layout.Rect{
				X: w.rect.X + 1, Y: w.rect.Y + 1 + paneHeaderRows, W: iw, H: ih,
			})
		}
	}
}

// UpdateContent updates the content of the pane's tab view. instance may be
// nil. It is called from the refreshPanesCmd goroutine with the instance
// captured on the event loop at dispatch time — deliberately a parameter, not
// a store read, so the capture is keyed to the binding the refresh was
// dispatched for.
func (w *TabbedWindow) UpdateContent(instance *session.Instance) error {
	return w.UpdateContentAt(instance, w.activeTab(), w.ContentSeq())
}

// UpdateContentAt captures a specific render binding if seq is still current.
// It is used by #1321 previews so a fast sidebar scroll cannot let an older
// off-loop capture overwrite the newest preview target.
func (w *TabbedWindow) UpdateContentAt(instance *session.Instance, tab int, seq uint64) error {
	return w.tab.UpdateContentGuarded(instance, tab, func() bool {
		return w.ContentSeq() == seq
	})
}

// InvalidateContent synchronously adopts a new view key and fallback message so
// the next frame cannot show a preview header over the previous pane content.
func (w *TabbedWindow) InvalidateContent(instance *session.Instance, tab int, message string) {
	w.tab.InvalidateContent(instance, tab, message)
}

// ResetToNormalMode resets the pane's tab view to normal (non-scroll) mode.
func (w *TabbedWindow) ResetToNormalMode(instance *session.Instance) error {
	return w.tab.ResetToNormalMode(instance, w.activeTab())
}

func (w *TabbedWindow) ScrollUp() {
	if err := w.tab.ScrollUp(w.effectiveInstance(), w.effectiveTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll up: %v", err)
	}
}

func (w *TabbedWindow) ScrollDown() {
	if err := w.tab.ScrollDown(w.effectiveInstance(), w.effectiveTab()); err != nil {
		log.InfoLog.Printf("tabbed window failed to scroll down: %v", err)
	}
}

// IsInPreviewTab returns true if the Agent tab is the pane's tab.
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
	if w.preview != nil && w.preview.instance != nil {
		inst := w.preview.instance
		label := tabLabelFor(inst, w.preview.tab)
		text = fmt.Sprintf(" PREVIEW %s · %s (original %s) ", inst.Title, label, w.preview.original)
	} else if inst := w.boundInstance(); inst != nil {
		label := tabLabelFor(inst, w.activeTab())
		text = fmt.Sprintf(" %s · %s ", inst.Title, label)
		if w.selectionHint != "" {
			text = fmt.Sprintf(" %s · %s · selected: %s ", inst.Title, label, w.selectionHint)
		}
	} else {
		text = " no session selected "
	}
	style := paneHeaderStyle
	switch {
	case w.interactive:
		style = paneHeaderInteractiveStyle
	case w.focused:
		style = paneHeaderFocusedStyle
	case w.boundInstance() == nil:
		style = paneHeaderDimStyle
	}
	// Ellipsize before styling: ClampToRect's hard cut would render a narrow
	// pane's header as `alpha · Termina` with no mark of the cut (#1098).
	header := style.Render(fitLine(text, width))
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
	liveShowing := w.live != nil && !w.tab.IsScrolling()
	w.registerZones(liveShowing)
	// The live embedded terminal renders instead of the capture when bound
	// (#1089 PR 1). Scroll mode still renders the TabPane's viewport: the
	// host-side scrollback UX stays capture-based until the interactive-mode
	// PR decides who owns scrolling (RFC §2.4).
	content := ""
	if liveShowing {
		// The cursor overlays only while this pane's terminal owns the
		// keyboard — the interactive typing cue (#1089 PR 2).
		content = w.live.Render(iw, ih, w.interactive)
	} else {
		content = w.tab.String()
	}
	inner := lipgloss.JoinVertical(lipgloss.Left,
		w.renderHeader(iw),
		layout.ClampToRect(content, layout.Rect{W: iw, H: ih}),
	)
	frame := windowStyle
	switch {
	case w.interactive:
		frame = interactiveWindowStyle
	case !w.focused:
		frame = blurredWindowStyle
	}
	return layout.ClampToRect(frame.Render(inner), w.rect)
}
