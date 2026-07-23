package ui

import (
	"fmt"
	"strings"
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
	BorderForeground(activeTheme.PaneBorderDefault).
	Border(lipgloss.RoundedBorder())

// blurredWindowStyle is the neutral pane frame for ordinary panes.
var blurredWindowStyle = windowStyle.
	BorderForeground(activeTheme.PaneBorderDefault)

// selectedWindowStyle marks the pane that matches the current sidebar
// highlight, while the focus ring is elsewhere.
var selectedWindowStyle = windowStyle.
	BorderForeground(activeTheme.PaneBorderSelected)

// interactiveWindowStyle marks the pane that owns the keyboard in
// interactive mode (#1089, RFC §2.3): a green DOUBLE border still signals
// "keystrokes go INTO this terminal" even when colors are unavailable.
var interactiveWindowStyle = windowStyle.
	Border(lipgloss.DoubleBorder()).
	BorderForeground(activeTheme.PaneBorderInteractive)

// previewWindowStyle marks a transient #1321 preview binding without
// mutating the pane's committed store binding.
var previewWindowStyle = windowStyle.
	BorderForeground(activeTheme.PaneBorderPreview)

// dropTargetWindowStyle marks the pane currently under an active tab drag.
var dropTargetWindowStyle = windowStyle.
	BorderForeground(activeTheme.Warning)

var paneHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.Foreground)

var paneHeaderFocusedStyle = lipgloss.NewStyle().
	Bold(true).
	Background(activeTheme.SelectionBackground).
	Foreground(activeTheme.SelectionForeground)

var paneHeaderDimStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

// paneHeaderInteractiveStyle matches the interactive frame: green header bar
// on the pane whose terminal owns the keyboard.
var paneHeaderInteractiveStyle = lipgloss.NewStyle().
	Bold(true).
	Background(activeTheme.Success).
	Foreground(activeTheme.Background)

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
	// sidebarSelected marks whether the pane's committed binding matches the
	// current sidebar selection. It colors selected-but-not-focused panes blue
	// without moving focus or mutating the pane binding.
	sidebarSelected bool
	// dropTarget marks this pane as the current drop target while a sidebar
	// tab drag is active. It is render-only and never changes pane focus.
	dropTarget bool

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
	if w.preview == nil {
		// Ownership belongs to the rendered tab, not the window. The next live or
		// detached snapshot will establish the new tab's owner. A transient preview
		// still renders its own target and therefore keeps its current owner.
		w.SetScrollOwnerResolving()
	}
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
// pane binding. original is rendered in the preview header so the reversible
// state is visible to the user.
func (w *TabbedWindow) SetPreview(instance *session.Instance, tab int, original string) uint64 {
	if w.preview != nil &&
		w.preview.instance == instance &&
		w.preview.tab == tab &&
		w.preview.original == original {
		return w.ContentSeq()
	}
	w.preview = &windowPreview{instance: instance, tab: tab, original: original}
	// A transient preview has no live stream and therefore no authoritative
	// terminal-mode snapshot. Unknown must not inherit the committed pane's owner
	// and accidentally capture a fullscreen target's background primary history.
	w.SetScrollOwnerResolving()
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
	// The app will replace this with the freshly rebound live stream's owner (or
	// HostHistory for a capture-only pane). Until then there is no truthful owner.
	w.SetScrollOwnerResolving()
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
// derivation lives in tree.TabLabels (#1024 PR 3), shared with the sidebar tree
// and the 1-9 jump keys. Never empty.
func (w *TabbedWindow) tabLabels() []string {
	return tree.TabLabels(w.boundInstance())
}

func tabLabelFor(inst *session.Instance, idx int) string {
	if label, ok := tree.TabLabelAt(inst, idx); ok {
		return label
	}
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

// SetSidebarSelected marks whether this pane corresponds to the current
// sidebar highlight. The root model updates it immediately before rendering.
func (w *TabbedWindow) SetSidebarSelected(on bool) {
	w.sidebarSelected = on
}

// SetDropTarget marks whether this pane is the current tab-drag drop target.
func (w *TabbedWindow) SetDropTarget(on bool) {
	w.dropTarget = on
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

// RenderRevision returns the generation of the last completed pane render.
func (w *TabbedWindow) RenderRevision() uint64 {
	return w.tab.RenderRevision()
}

// InvalidateContentIfUnchanged replaces stale content only while both the
// preview binding and completed render are unchanged from the caller's sample.
func (w *TabbedWindow) InvalidateContentIfUnchanged(
	instance *session.Instance,
	tab int,
	seq uint64,
	renderRevision uint64,
	message string,
) bool {
	if w.ContentSeq() != seq {
		return false
	}
	return w.tab.InvalidateContentIfRevision(instance, tab, renderRevision, message)
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

// IsInScrollMode returns true if the pane's tab view is in scroll mode.
func (w *TabbedWindow) IsInScrollMode() bool {
	return w.tab.IsScrolling()
}

// ScrollOwner reports which subsystem can satisfy scroll intent for this pane.
// Exposing the owner at the window boundary is the routing seam for captured
// host history and child-owned terminal history.
func (w *TabbedWindow) ScrollOwner() ScrollOwner {
	return w.tab.ScrollOwner()
}

// SetScrollOwner switches the pane's controller to the terminal's current owner.
// It is event-loop safe; TabPane serializes against an off-loop history fill.
func (w *TabbedWindow) SetScrollOwner(owner ScrollOwner) {
	w.tab.SetScrollOwnerFor(w.effectiveInstance(), w.effectiveTab(), owner)
}

// ObserveScrollOwnerUnknown records a transient stream-authority gap. An active
// AF history transaction remains usable; every idle pane becomes unavailable.
func (w *TabbedWindow) ObserveScrollOwnerUnknown() ScrollOwner {
	return w.tab.ObserveScrollOwnerUnknownFor(w.effectiveInstance(), w.effectiveTab())
}

// SetScrollOwnerResolving makes the effective capture target own the pending
// ownership probe without inheriting the committed/previous target's decision.
func (w *TabbedWindow) SetScrollOwnerResolving() {
	w.tab.SetScrollOwnerResolvingFor(w.effectiveInstance(), w.effectiveTab())
}

// CanResolveScrollOwner distinguishes a capture-backed unknown target from a
// live stream still waiting for its authoritative repaint.
func (w *TabbedWindow) CanResolveScrollOwner() bool {
	return w.tab.CanResolveScrollOwner()
}

// NeedsScrollFill reports whether the pane just entered scroll mode and is still
// waiting for its off-loop scrollback capture — panesRefresh bypasses its
// throttle for such a pane so the fill is immediate (#1637).
func (w *TabbedWindow) NeedsScrollFill() bool {
	return w.tab.NeedsScrollFill()
}

// BeginScrollFill claims the pending scroll fill for the capture panesRefresh is
// about to dispatch, so a refresh cycle that fires before it lands does not
// dispatch a redundant one (#1709).
func (w *TabbedWindow) BeginScrollFill() {
	w.tab.BeginScrollFill()
}

// renderHeader renders the one-line `title · tab` header. The header is the
// only place the pane's tab is named inside the pane (the tab bar is gone), so
// duplicate labels include their 1-based jump slot (#2150); the highlight
// doubles as the pane's focus indicator.
func (w *TabbedWindow) renderHeader(width int) string {
	var text string
	if w.preview != nil && w.preview.instance != nil {
		inst := w.preview.instance
		label := tabLabelFor(inst, w.preview.tab)
		text = fmt.Sprintf(" Preview %s · %s (original %s) ", inst.Title, label, w.preview.original)
	} else if inst := w.boundInstance(); inst != nil {
		label := tabLabelFor(inst, w.activeTab())
		text = fmt.Sprintf(" %s · %s ", inst.Title, label)
		if w.selectionHint != "" {
			text = fmt.Sprintf(" %s · %s · selected: %s ", inst.Title, label, w.selectionHint)
		}
	} else {
		text = " no session selected "
	}
	if w.IsInScrollMode() {
		// Scroll mode is pane chrome, not terminal history. Keeping this cue in
		// the header prevents an AF-owned footer row from consuming the first
		// scroll gesture or changing the child's history coordinates (#2192).
		text = strings.TrimSuffix(text, " ") + " · Scroll · Esc exits "
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

func (w *TabbedWindow) frameStyle() lipgloss.Style {
	switch {
	case w.preview != nil:
		return previewWindowStyle
	case w.interactive:
		return interactiveWindowStyle
	case w.dropTarget:
		return dropTargetWindowStyle
	case w.sidebarSelected && !w.focused:
		return selectedWindowStyle
	default:
		return blurredWindowStyle
	}
}

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
	return layout.ClampToRect(w.frameStyle().Render(inner), w.rect)
}
