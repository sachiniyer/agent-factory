package ui

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var tabPaneStyle = lipgloss.NewStyle().
	Foreground(activeTheme.Foreground)

var tabPaneFooterStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

// tabContentState holds the rendered content of the tab pane.
//
// Invariant: fallback==true iff text is a centered fallback message
// (loading / error / inactive). Writers MUST replace the whole struct rather
// than mutate fields individually, so the two fields can never disagree about
// which rendering branch String() should take (#577).
type tabContentState struct {
	fallback bool
	text     string
}

// TabPane is the single right-hand pane introduced in PR 2 of #930. It renders
// the currently selected tab of an instance by capturing that tab's tmux
// session (detached) and attaches to it on Enter. It is the merge of the former
// PreviewPane (agent tab) and TerminalPane (shell tab): both surfaces are now
// one pane with one scroll/fallback state machine. The render *source* and
// fallback *copy* still depend on whether the selected tab is the agent tab
// (index 0) or a shell tab, so every hard-won per-tab edge fix is preserved.
//
// All hard-won race/edge fixes from both panes live here, in one place:
//   - #684 the active-tab index is an atomic on TabbedWindow (not here).
//   - #579 UpdateContent serialises state writes against String() reads via
//     p.mu so the renderer never observes a half-written state.
//   - #702/#746/#384 the mouse/keyboard scroll path can fire before the async
//     UpdateContent for a newly selected tab/instance — dropStaleView resets
//     scroll state when the (instance, slot) view key changes. Capture is keyed
//     off the passed instance + slot (not a title cache), so there is no
//     stale-session drift to attach/scroll the wrong thing.
//   - #669/#672/#940 setFallbackState clears scroll state so fallback==true can
//     never coexist with an active scroll controller (String() checks it first).
//   - #496/#920/#935 session-gone / Deleting / Dead fallbacks.
//   - #898/#649 trailing-newline strip + newest-lines truncation.
type TabPane struct {
	// mu serialises UpdateContent writes (called off the bubbletea Update
	// goroutine via refreshPanesCmd) against String() (called from the
	// renderer), plus the scroll-mode mutators. Without it the renderer can
	// observe partially written state while a capture is in flight (#579).
	mu sync.Mutex

	width  int
	height int

	content tabContentState
	// renderRevision advances whenever this pane publishes a completed render
	// state: captured content, a fallback, or a filled scroll viewport. Preview
	// retargeting snapshots it so a grace-period expiry can replace stale content
	// without racing and overwriting a capture that landed at the deadline.
	renderRevision uint64
	// scroll is the explicit scroll owner and transition controller (#2192).
	// Captured tabs use host-history ownership. It owns active/loading/
	// ready state, fill generations, and pending intent; TabPane owns only the
	// rendered viewport it asks the controller to manipulate.
	scroll   historyScrollController
	viewport viewport.Model

	// currentInstance + currentTab identify the (instance, tab-index) view
	// currently rendered. UpdateContent/ScrollUp/ScrollDown reset scroll-mode
	// state when either changes so switching instances OR tabs while scrolling
	// does not leave the viewport pinned on the previous view's content
	// (#470/#702/#746). currentTab is the 0-based tab index (0 is the agent tab);
	// it is also used to resize the active shell tab's detached session when the
	// pane is resized.
	currentInstance *session.Instance
	currentTab      int

	// previewSrc captures a tab's content. Since #1592 Phase 2 PR6 the TUI no
	// longer shells out to `tmux capture-pane` itself — the daemon is the sole
	// capturer — so this is injected by the app (backed by the daemon Preview RPC)
	// rather than calling instance.Preview*() directly. It returns
	// tmux.ErrSessionGone when the session vanished mid-capture so the pane shows
	// its session-gone fallback exactly as before.
	previewSrc PreviewSource
}

// PreviewSource captures a session tab's content for a TabPane. tab 0 is the agent
// tab (formatted by the backend preview); tab>0 is a shell/process tab. full=true
// returns the entire scrollback history (the scroll-mode source). It returns
// tmux.ErrSessionGone when the session's tmux vanished mid-capture.
type PreviewSource func(instance *session.Instance, tab int, full bool) (string, error)

// NewTabPane creates a TabPane whose content is captured through src — the
// daemon-backed capture in production (#1592 Phase 2 PR6).
func NewTabPane(src PreviewSource) *TabPane {
	return &TabPane{
		viewport:   viewport.New(0, 0),
		previewSrc: src,
		scroll:     newHostHistoryScrollController(),
	}
}

// IsScrolling reports whether the pane is in scroll mode. Locks p.mu to match
// the mutators (#579).
func (p *TabPane) IsScrolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scroll.Active()
}

// ScrollOwner reports which subsystem is responsible for satisfying this
// pane's scroll requests. The owner boundary lets child-owned terminals route
// input without changing the host-history contract.
func (p *TabPane) ScrollOwner() ScrollOwner {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scroll.Owner()
}

// NeedsScrollFill reports whether the pane is in scroll mode with an unfilled
// viewport — ScrollUp/ScrollDown just entered scroll mode and the off-loop
// capture that populates the scrollback (updateAgent/updateShell lazy-fill) has
// not landed yet. panesRefresh uses it to bypass its capture throttle so the
// fill runs on the very next refresh instead of up to a tick later, keeping
// scroll-mode entry visually instant with no capture on the event loop (#1637).
func (p *TabPane) NeedsScrollFill() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scroll.NeedsFill(p.viewport.Height)
}

// BeginScrollFill records that panesRefresh has dispatched a capture for the
// current fill generation, so a refresh cycle in the dispatch→land window sees
// NeedsScrollFill go false and does not fire a redundant one (#1709). It is
// called synchronously on the event loop the instant the capture is dispatched.
// A later scroll lifecycle has a new controller generation, which both re-arms
// NeedsScrollFill and marks the prior in-flight capture stale.
func (p *TabPane) BeginScrollFill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scroll.BeginFill()
}

func (p *TabPane) SetSize(width, maxHeight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width = width
	p.height = maxHeight
	p.scroll.Resize(&p.viewport, width, maxHeight)
	// Local session sizing is the WS stream's job now (last-resize-wins
	// resize-window, #1592 Phase 2 PR6): the pane geometry rides the RESIZE frame,
	// so the TabPane no longer resizes a detached tmux session itself.
}

// dropStaleView clears scroll-mode viewport content captured from a previously
// selected (instance, slot) view and adopts the new view. Caller must hold
// p.mu.
//
// UpdateContent runs this on every refresh, but the scroll path (ScrollUp/
// ScrollDown) is driven straight off the bubbletea event loop and can fire
// before the async UpdateContent for the newly selected view has run. Without
// this guard a scroll would scroll the previous view's stale viewport instead
// of resetting scroll mode (#702/#746). Consolidating the reset here keeps the
// entry points consistent (the #669 motivation).
func (p *TabPane) dropStaleView(instance *session.Instance, activeTab int) {
	if instance != p.currentInstance || activeTab != p.currentTab {
		if p.scroll.Active() {
			p.scroll.Reset(&p.viewport)
		}
		p.currentInstance = instance
		p.currentTab = activeTab
	}
}

// publishContent replaces the normal/fallback render state and records that a
// completed state for the current binding is ready to paint. Caller must hold
// p.mu.
func (p *TabPane) publishContent(content tabContentState) {
	p.content = content
	p.renderRevision++
}

// setFallbackState sets the pane to display a centered fallback message. Caller
// must hold p.mu.
//
// Also resets scroll-mode state so fallback==true cannot coexist with an active
// controller. String() checks scroll before fallback, so leaving it active when
// entering a fallback (nil/Loading/Deleting/Dead/session-gone) would render the
// prior view's stale viewport instead of the fallback message (#669/#672/#940).
func (p *TabPane) setFallbackState(message string) {
	p.publishContent(tabContentState{
		fallback: true,
		text:     lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message),
	})
	p.scroll.Reset(&p.viewport)
}

// RenderRevision returns the completed-render generation. It is safe to read
// from the Bubble Tea event loop while an off-loop capture is publishing.
func (p *TabPane) RenderRevision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.renderRevision
}

type contentGuard func() bool

func guardOK(guard contentGuard) bool {
	return guard == nil || guard()
}

func (p *TabPane) isCurrentViewLocked(instance *session.Instance, activeTab int) bool {
	return p.currentInstance == instance && p.currentTab == activeTab
}

// InvalidateContent synchronously adopts a new view key and fallback state.
// This is used by #1321 preview retargeting so the next render frame cannot
// pair a new PREVIEW header with stale content from the previous binding.
func (p *TabPane) InvalidateContent(instance *session.Instance, activeTab int, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropStaleView(instance, activeTab)
	p.setFallbackState(message)
}

// InvalidateContentIfRevision publishes a fallback only if no capture or other
// completed state has landed since expected was sampled. The check and write
// share p.mu so a capture completing at the grace-period boundary wins instead
// of being overwritten by a late loading fallback.
func (p *TabPane) InvalidateContentIfRevision(
	instance *session.Instance,
	activeTab int,
	expected uint64,
	message string,
) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.renderRevision != expected {
		return false
	}
	p.dropStaleView(instance, activeTab)
	p.setFallbackState(message)
	return true
}

// UpdateContent captures the selected tab's content. Safe to call from a
// goroutine — it serialises state writes against String() via p.mu (#579).
// activeTab is the 0-based selected tab index: index 0 is the agent tab
// (captured via the backend preview), any other index is a shell/process tab
// captured straight from that tab's tmux session. The slot selects the capture
// source and the fallback copy, so the merged pane reproduces the old
// PreviewPane and TerminalPane behaviors.
func (p *TabPane) UpdateContent(instance *session.Instance, activeTab int) error {
	return p.UpdateContentGuarded(instance, activeTab, nil)
}

// UpdateContentGuarded captures the selected tab's content and publishes it
// only while guard still reports that the caller's render binding is current.
// Capture itself runs outside p.mu so event-loop invalidation can immediately
// clear stale content even if an older capture is still in flight.
func (p *TabPane) UpdateContentGuarded(instance *session.Instance, activeTab int, guard contentGuard) error {
	if activeTab == 0 {
		return p.updateAgent(instance, guard)
	}
	return p.updateShell(instance, activeTab, guard)
}

// fillHostHistoryLocked performs the one asynchronous transition shared by
// agent and shell previews. Caller holds p.mu; this method releases it around
// capture and returns with it held. The controller keeps both the generation
// token and every pending ScrollIntent, so a stale capture cannot publish and a
// slow current capture cannot erase input (#1637/#1709/#2192).
func (p *TabPane) fillHostHistoryLocked(
	instance *session.Instance,
	activeTab int,
	guard contentGuard,
	goneMessage string,
) error {
	gen := p.scroll.FillGeneration()
	p.mu.Unlock()
	content, err := p.previewSrc(instance, activeTab, true)
	p.mu.Lock()

	if !p.scroll.FillIsCurrent(gen) {
		return nil
	}
	if !guardOK(guard) || !p.isCurrentViewLocked(instance, activeTab) {
		p.scroll.RearmFill()
		return nil
	}
	if err != nil {
		if errors.Is(err, tmux.ErrSessionGone) {
			p.setFallbackState(goneMessage)
			return nil
		}
		p.scroll.RearmFill()
		return err
	}
	content = lipgloss.JoinVertical(lipgloss.Left, content, scrollFooter())
	if p.scroll.CompleteFill(gen, &p.viewport, content) {
		p.renderRevision++
	}
	return nil
}

// updateAgent reproduces the former PreviewPane.UpdateContent.
func (p *TabPane) updateAgent(instance *session.Instance, guard contentGuard) error {
	p.mu.Lock()
	if !guardOK(guard) {
		p.mu.Unlock()
		return nil
	}
	p.dropStaleView(instance, 0)
	switch {
	case instance == nil:
		p.setFallbackState("No agents running yet. Spin up a new instance with 'n' to get started!")
		p.mu.Unlock()
		return nil
	case instance.IsCreating():
		p.setFallbackState("Setting up workspace…")
		p.mu.Unlock()
		return nil
	case instance.IsTearingDown():
		// Mirror the creating case for a teardown op (#920/#1195): during
		// teardown Preview() returns ("", nil) and Started()==false, so without
		// this the generic name fallback below would claim throughout the delete.
		p.setFallbackState("Tearing down session…")
		p.mu.Unlock()
		return nil
	case instance.GetLiveness() == session.LiveDead:
		// The daemon poll marks a session Dead once its backing session is
		// gone (#935); keying off the liveness makes the fallback synchronous with
		// the sidebar's dead-dot so the panes never disagree.
		p.setFallbackState("Session no longer running.")
		p.mu.Unlock()
		return nil
	case instance.GetLiveness() == session.LiveLost:
		// Lost (#1108): the tmux session vanished with no kill on record —
		// same synchronous-with-the-sidebar treatment as Dead, but the message
		// says what happened rather than implying a plain corpse.
		p.setFallbackState("Session lost — its tmux session is gone.")
		p.mu.Unlock()
		return nil
	}
	// A LimitReached agent (#1146) is deliberately NOT a fallback: its tmux is
	// alive and its screen shows the limit message, so it falls through to the
	// live Preview() below.

	// Scroll entry is I/O-free; the controller preserves pending intent while
	// this off-loop full-history capture runs (#1637/#2192).
	if p.scroll.AwaitingHistory() {
		err := p.fillHostHistoryLocked(instance, 0, guard, "Session no longer running.")
		p.mu.Unlock()
		return err
	}

	if p.scroll.Active() {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	content, err := p.previewSrc(instance, 0, false)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !guardOK(guard) || !p.isCurrentViewLocked(instance, 0) {
		return nil
	}
	if err != nil {
		// Tmux session vanished out from under us (#496): render an inactive
		// fallback instead of logging at ERROR every preview tick.
		if errors.Is(err, tmux.ErrSessionGone) {
			p.setFallbackState("Session no longer running.")
			return nil
		}
		return err
	}
	// Always update with content, even if empty, so a newly created instance
	// displays immediately.
	if len(content) == 0 && !instance.Started() {
		p.setFallbackState("Please enter a name for the instance.")
	} else {
		p.publishContent(tabContentState{fallback: false, text: content})
	}
	return nil
}

// webTabPlaceholder is the TUI content for a web/iframe tab, which the terminal
// cannot render: the target URL plus a pointer to where it can be viewed. Shared
// by updateShell and canEnterScrollModeLocked so the two never diverge.
func webTabPlaceholder(url string) string {
	return fmt.Sprintf("%s\n\nWeb tab — view in the web UI or open in a browser", url)
}

// vscodeTabPlaceholder is the TUI content for a VS Code tab. Unlike a web tab
// there is no URL to show: the editor is a daemon-managed per-session
// code-server on a 0600 unix socket, reachable only through the daemon's proxy
// (#1873), so the only meaningful pointer is the web UI itself.
func vscodeTabPlaceholder() string {
	return "VS Code tab — view in the web UI\n\nThe editor opens this session's worktree. A terminal can't render it."
}

// tabPlaceholder returns the TUI placeholder for a tab kind the terminal cannot
// render, and ok=false for kinds it can (agent/shell/process, which have a PTY).
// Shared by updateShell and canEnterScrollModeLocked so the two never diverge.
func tabPlaceholder(tab *session.Tab) (string, bool) {
	switch tab.Kind {
	case session.TabKindWeb:
		return webTabPlaceholder(tab.URL), true
	case session.TabKindVSCode:
		return vscodeTabPlaceholder(), true
	default:
		return "", false
	}
}

// updateShell reproduces the former TerminalPane.UpdateContent for the
// shell/process tab at activeTab.
func (p *TabPane) updateShell(instance *session.Instance, activeTab int, guard contentGuard) error {
	p.mu.Lock()
	if !guardOK(guard) {
		p.mu.Unlock()
		return nil
	}
	p.dropStaleView(instance, activeTab)
	if instance == nil {
		p.setFallbackState("Select an instance to open a terminal")
		p.mu.Unlock()
		return nil
	}
	// A tearing-down instance reports Started()==false during teardown, so without
	// this it would fall through to the "not started yet" fallback — misleading
	// while the session is going away (#920/#1195).
	if instance.IsTearingDown() {
		p.setFallbackState("Tearing down session…")
		p.mu.Unlock()
		return nil
	}
	if !instance.Started() {
		p.setFallbackState("Instance is not started yet.")
		p.mu.Unlock()
		return nil
	}

	// A web/vscode tab has no PTY to capture — one is a URL the web UI iframes,
	// the other a browser-served editor. The TUI cannot render either, so show a
	// clean placeholder instead of falling through to the misleading "Terminal
	// session not available" branch below (neither kind is ever TabAlive).
	if tabs := instance.GetTabs(); activeTab >= 0 && activeTab < len(tabs) {
		if placeholder, ok := tabPlaceholder(tabs[activeTab]); ok {
			p.setFallbackState(placeholder)
			p.mu.Unlock()
			return nil
		}
	}

	// Remote instances have no local shell tab. When terminal_cmd is configured
	// the tab is an interactive-only surface (#843): prompt the user to attach.
	// Otherwise keep the "not available" fallback and name the config knob.
	if caps := instance.Capabilities(); caps.Workspace == session.WorkspaceRemote {
		if caps.TerminalTab {
			p.setFallbackState("Press Enter to open a terminal on the remote machine.")
		} else {
			p.setFallbackState("Terminal tab not available for remote sessions.\nConfigure remote_hooks.terminal_cmd to enable it.\nUse the Agent tab to see session output.")
		}
		p.mu.Unlock()
		return nil
	}

	// The tab's session is owned by the Instance and created at start (or by the
	// new-tab hotkey). If it is not alive, show a fallback rather than leaving the
	// previous view's content on screen (#747, generalized to the persisted tab).
	//
	// This runs BEFORE the scroll-mode guard below so a shell session killed
	// externally while the user is scrolling transitions to the fallback instead
	// of leaving stale scrollback pinned on screen forever (#977). setFallbackState
	// clears scroll state, so String() (which checks the controller first) renders the
	// fallback message. This mirrors the agent slot, whose Dead check also precedes
	// its scroll guard in updateAgent.
	if !instance.TabAlive(activeTab) {
		p.setFallbackState("Terminal session not available.")
		p.mu.Unlock()
		return nil
	}

	// The shell slot uses the same controller and off-loop fill transition as the
	// agent slot; input queued during capture is applied when history publishes.
	if p.scroll.AwaitingHistory() {
		err := p.fillHostHistoryLocked(instance, activeTab, guard, "Terminal session no longer running.")
		p.mu.Unlock()
		return err
	}

	// Already-filled scroll viewport: leave it (and p.content) untouched so
	// LineUp/LineDown keep their position.
	if p.scroll.Active() {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	content, err := p.previewSrc(instance, activeTab, false)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !guardOK(guard) || !p.isCurrentViewLocked(instance, activeTab) {
		return nil
	}
	if err != nil {
		// The alive pre-check above can race an external kill: fall through to a
		// fallback instead of propagating an error logged at ERROR (#496).
		if errors.Is(err, tmux.ErrSessionGone) {
			p.setFallbackState("Terminal session no longer running.")
			return nil
		}
		return fmt.Errorf("tab pane: failed to capture terminal content: %w", err)
	}
	p.publishContent(tabContentState{fallback: false, text: content})
	return nil
}

// String renders the pane content, exactly width×height cells. Every branch
// funnels through a final layout.ClampToRect so no capture, viewport, or
// fallback content can ever exceed the allocation: wide capture-pane lines —
// a process tab whose program emits lines wider than the pane (#1082) — are
// truncated per line rather than wrapped (the pre-cutover Style.Width wrap
// re-flowed them onto extra rows, overflowing the pane and pushing chrome off
// screen).
func (p *TabPane) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.width <= 0 || p.height <= 0 {
		return ""
	}
	rect := layout.Rect{W: p.width, H: p.height}

	// In scroll/copy mode always use the viewport.
	if p.scroll.Active() {
		return layout.ClampToRect(p.viewport.View(), rect)
	}

	if p.content.fallback {
		// TabbedWindow.SetRect already subtracts borders/margins/padding from
		// p.height, so use it directly to match normal mode. Subtracting again
		// would double-count chrome and leave a trailing blank line (#616/#703).
		// renderCenteredFallback centers using the wrapped line count so narrow
		// panes don't miscenter (#699).
		return layout.ClampToRect(
			renderCenteredFallback(tabPaneStyle, p.content.text, p.width, p.height), rect)
	}

	lines := strings.Split(p.content.text, "\n")
	// Drop ALL trailing blank lines before the keep-newest truncation below. A
	// shell/process tab is captured with `tmux capture-pane`, which returns the
	// session's FULL screen height: the visible content (e.g. a prompt at the top)
	// followed by blank rows padding out to the window height, which it does NOT
	// strip. When that captured window is taller than the preview pane — the common
	// case, since a non-streamed tab keeps whatever taller size a prior stream pinned
	// its window to — keeping only the newest p.height lines dropped the real content
	// off the top and left the body rendering the trailing blanks, i.e. empty
	// (#1958). Stripping them first makes the truncation act on real content only:
	// genuine overflow (an agent that scrolled past the pane) has content on every
	// row and is unaffected, while a short capture padded with blanks renders
	// top-aligned — matching what the live pane shows for the same tab. This subsumes
	// the earlier single-trailing-"\n" strip (the empty element strings.Split leaves
	// when text ends in "\n" — #649/#898). A row is blank when it has no printable
	// content: lipgloss.Width is ANSI-aware (a trailing style reset is zero-width) and
	// TrimRight handles capture's trailing-space padding.
	for len(lines) > 0 && lipgloss.Width(strings.TrimRight(lines[len(lines)-1], " ")) == 0 {
		lines = lines[:len(lines)-1]
	}

	if len(lines) > p.height {
		// Show the newest output, not the oldest (#649). Height is trimmed
		// here — before the clamp — because ClampToRect keeps the FIRST
		// height lines, and a capture must keep the newest.
		lines = lines[len(lines)-p.height:]
	}

	return layout.ClampToRect(tabPaneStyle.Render(strings.Join(lines, "\n")), rect)
}

// ScrollUp enters scroll mode (if not already) and scrolls up.
func (p *TabPane) ScrollUp(instance *session.Instance, activeTab int) error {
	return p.scrollBy(instance, activeTab, scrollOneLineUp)
}

// ScrollDown enters scroll mode (if not already) and scrolls down.
func (p *TabPane) ScrollDown(instance *session.Instance, activeTab int) error {
	return p.scrollBy(instance, activeTab, scrollOneLineDown)
}

// scrollBy is the single keyboard/wheel-independent input path. It validates a
// new host-history session, then hands the semantic intent to the controller;
// the controller either applies it now or preserves it across the pending fill.
func (p *TabPane) scrollBy(instance *session.Instance, activeTab int, intent ScrollIntent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if instance == nil {
		return nil
	}
	// Reset scroll mode if the view changed out from under us, so we capture the
	// newly selected view's content rather than scrolling stale content (#702).
	p.dropStaleView(instance, activeTab)
	if !p.scroll.Active() && !p.canEnterScrollModeLocked(instance, activeTab) {
		return nil
	}
	p.scroll.Scroll(&p.viewport, intent)
	return nil
}

// canEnterScrollModeLocked validates a new scroll session WITHOUT capturing.
// The full-scrollback capture used to run inline here, on the bubbletea event
// loop — an unbounded tmux capture / daemon Preview RPC that froze the whole TUI
// while entering scroll mode if that capture was slow or hung (#1637). The
// controller starts the lazy fill only after this in-memory validation succeeds.
// Caller must hold p.mu.
func (p *TabPane) canEnterScrollModeLocked(instance *session.Instance, activeTab int) bool {
	if instance.IsTearingDown() {
		p.setFallbackState("Tearing down session…")
		return false
	}
	// A web/vscode tab has no scrollback (no PTY): keep the placeholder rather than
	// entering scroll mode over an empty capture. Mirrors updateShell's branch.
	if tabs := instance.GetTabs(); activeTab >= 0 && activeTab < len(tabs) {
		if placeholder, ok := tabPlaceholder(tabs[activeTab]); ok {
			p.setFallbackState(placeholder)
			return false
		}
	}
	// An already-dead shell tab transitions to the fallback rather than entering
	// scroll mode over stale terminal output: leaving normal content intact would
	// render the last capture instead of the dead-session
	// message. Mirrors updateShell's !TabAlive branch (#998, sibling of #977/#984).
	if activeTab != 0 && !instance.TabAlive(activeTab) {
		p.setFallbackState("Terminal session not available.")
		return false
	}
	return true
}

// ResetToNormalMode exits scroll mode and returns to normal content display.
func (p *TabPane) ResetToNormalMode(instance *session.Instance, activeTab int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Always clear scroll state first so pressing ESC while no instance is
	// selected (e.g. the sidebar header) does not leave the pane stuck on stale
	// viewport content.
	wasScrolling := p.scroll.Active()
	if wasScrolling {
		p.scroll.Reset(&p.viewport)
	}

	if instance == nil || !wasScrolling {
		return nil
	}
	p.dropStaleView(instance, activeTab)

	// A shell/process slot simply returns to live capture on the next refresh —
	// no re-capture here (the former TerminalPane.ResetToNormalMode behavior).
	if activeTab != 0 {
		return nil
	}

	// Agent slot: surface a transient/dead fallback synchronously (these are
	// in-memory checks, no I/O) so ESC on a gone/creating session shows the right
	// message at once rather than the pre-scroll capture (#823/#920/#935). Live
	// content is restored off the event loop by the immediate refresh the app
	// dispatches after ESC — there is no inline Preview() here, which used to
	// block the bubbletea event loop on a slow tmux capture / daemon RPC (#1637).
	// A live session keeps p.content (the last non-scroll capture, still valid)
	// until that refresh lands; a LimitReached agent (#1146) likewise falls
	// through to its live screen rather than a fallback.
	switch {
	case instance.IsCreating():
		p.setFallbackState("Setting up workspace…")
	case instance.IsTearingDown():
		p.setFallbackState("Tearing down session…")
	case instance.GetLiveness() == session.LiveDead:
		p.setFallbackState("Session no longer running.")
	case instance.GetLiveness() == session.LiveLost:
		p.setFallbackState("Session lost — its tmux session is gone.")
	}
	return nil
}

func scrollFooter() string {
	return tabPaneFooterStyle.Render("ESC to exit scroll mode")
}
