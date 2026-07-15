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
//     never coexist with isScrolling==true (String() checks isScrolling first).
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

	content     tabContentState
	isScrolling bool
	// scrollFillPending is set when ScrollUp/ScrollDown enter scroll mode and
	// cleared once the off-loop capture has populated the viewport. Scroll entry
	// no longer captures on the bubbletea event loop (#1637); this flag is the
	// deterministic "capture still owed" signal the updateAgent/updateShell
	// lazy-fill and NeedsScrollFill key off — a sized viewport's View() is never
	// the empty string (it renders padding), so viewport emptiness cannot serve.
	scrollFillPending bool
	// scrollFillGen / scrollFillDispatchedGen are the generation token that
	// replaces a plain in-flight bool (#1709). scrollFillGen stamps the current
	// scroll-fill lifecycle: it bumps on every scroll entry, every reset, and
	// every re-arm, so each owed fill is a distinct request. scrollFillDispatchedGen
	// records the generation panesRefresh has already dispatched a capture for.
	//
	// A generation, not a bool, because the in-flight state is shared across scroll
	// EXIT and RE-ENTRY on the same instance/tab (unchanged render seq — scroll
	// entry/exit does not bump ContentSeq). A capture stamps scrollFillGen at
	// dispatch and, on return, applies its result only if the generation still
	// matches; a slow capture from a previous scroll session finds the generation
	// moved on and is ignored, so it can neither satisfy nor clear the newer
	// entry's fill. Masking (no redundant dispatch) falls out of the same tokens:
	// a fill is owed-and-undispatched only while scrollFillDispatchedGen != the
	// current scrollFillGen.
	scrollFillGen           uint64
	scrollFillDispatchedGen uint64
	viewport                viewport.Model

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
	}
}

// IsScrolling reports whether the pane is in scroll mode. Locks p.mu to match
// the mutators (#579).
func (p *TabPane) IsScrolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isScrolling
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
	// Owed AND not yet dispatched for the current generation: a dispatched capture
	// (scrollFillDispatchedGen == scrollFillGen) masks the pane until it resolves
	// or a new generation supersedes it, so no redundant capture fires (#1709).
	return p.isScrolling && p.scrollFillPending &&
		p.scrollFillDispatchedGen != p.scrollFillGen && p.viewport.Height > 0
}

// BeginScrollFill records that panesRefresh has dispatched a capture for the
// current fill generation, so a refresh cycle in the dispatch→land window sees
// NeedsScrollFill go false and does not fire a redundant one (#1709). It is
// called synchronously on the event loop the instant the capture is dispatched.
// A later scroll entry bumps scrollFillGen past this dispatched generation, which
// both re-arms NeedsScrollFill and marks the in-flight capture stale.
func (p *TabPane) BeginScrollFill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scrollFillDispatchedGen = p.scrollFillGen
}

// resetScrollFill clears the fill-owed flag and starts a new generation. Every
// scroll-state reset and every completed fill funnels through it: bumping the
// generation invalidates any capture still in flight against the old one, so a
// stale completion can never satisfy or clear a later fill (#1709). Caller must
// hold p.mu.
func (p *TabPane) resetScrollFill() {
	p.scrollFillPending = false
	p.scrollFillGen++
}

// rearmScrollFill starts a new generation while leaving the fill owed, so a
// capture that resolved but could not publish (render binding moved on, transient
// error) re-dispatches instead of wedging the viewport blank, and its own stale
// return is ignored (#1709). Caller must hold p.mu.
func (p *TabPane) rearmScrollFill() {
	p.scrollFillGen++
}

func (p *TabPane) SetSize(width, maxHeight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width = width
	p.height = maxHeight
	p.viewport.Width = width
	p.viewport.Height = maxHeight
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
		if p.isScrolling {
			p.isScrolling = false
			p.resetScrollFill()
			p.viewport.SetContent("")
			p.viewport.GotoTop()
		}
		p.currentInstance = instance
		p.currentTab = activeTab
	}
}

// setFallbackState sets the pane to display a centered fallback message. Caller
// must hold p.mu.
//
// Also resets scroll-mode state so fallback==true cannot coexist with
// isScrolling==true. String() checks isScrolling before fallback, so leaving
// scroll state intact when entering a fallback (nil/Loading/Deleting/Dead/
// session-gone) would render the prior view's stale viewport instead of the
// fallback message (#669/#672/#940).
func (p *TabPane) setFallbackState(message string) {
	p.content = tabContentState{
		fallback: true,
		text:     lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message),
	}
	p.isScrolling = false
	p.resetScrollFill()
	p.viewport.SetContent("")
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
		p.setFallbackState("Setting up workspace...")
		p.mu.Unlock()
		return nil
	case instance.IsTearingDown():
		// Mirror the creating case for a teardown op (#920/#1195): during
		// teardown Preview() returns ("", nil) and Started()==false, so without
		// this the generic name fallback below would claim throughout the delete.
		p.setFallbackState("Tearing down session...")
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

	// If scroll mode was entered but the scrollback hasn't been captured yet,
	// capture the full history now. ScrollUp/ScrollDown enter scroll mode WITHOUT
	// any capture (that used to block the bubbletea event loop on a slow tmux
	// capture / daemon RPC — #1637); the fill happens here instead, on the
	// off-loop refresh goroutine, the first time UpdateContent runs with a
	// pending scroll fill.
	if p.isScrolling && p.scrollFillPending {
		gen := p.scrollFillGen
		p.mu.Unlock()
		content, err := p.previewSrc(instance, 0, true)
		p.mu.Lock()
		defer p.mu.Unlock()
		// A scroll exit+re-entry (or any reset) during the capture bumps the
		// generation, handing the fill to a newer dispatch. Ignore this stale
		// completion entirely: it must neither satisfy nor clear the newer entry's
		// fill, and it must not publish its stale content (#1709 review). Every
		// path that clears pending or exits scroll mode bumps the generation, so a
		// matching generation here guarantees the fill is still owed and current.
		if p.scrollFillGen != gen {
			return nil
		}
		if !guardOK(guard) || !p.isCurrentViewLocked(instance, 0) {
			// Could not publish for the live render binding: re-arm so the owed
			// fill re-dispatches rather than wedging the viewport blank (#1709).
			p.rearmScrollFill()
			return nil
		}
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				// setFallbackState clears scroll state and the stale viewport, so
				// the fallback renders even mid scroll-capture (#940).
				p.setFallbackState("Session no longer running.")
				return nil
			}
			p.rearmScrollFill()
			return err
		}
		p.viewport.SetContent(lipgloss.JoinVertical(lipgloss.Left, content, scrollFooter()))
		// First fill lands at the bottom (newest output), matching the live view
		// the user was looking at when they entered scroll mode. This is the only
		// place that pins the offset: subsequent LineUp/LineDown move it and the
		// fill no longer runs (pending cleared), so the scroll position is
		// preserved (#1637).
		p.viewport.GotoBottom()
		p.resetScrollFill()
		return nil
	}

	if p.isScrolling {
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
		p.content = tabContentState{fallback: false, text: content}
	}
	return nil
}

// webTabPlaceholder is the TUI content for a web/iframe tab, which the terminal
// cannot render: the target URL plus a pointer to where it can be viewed. Shared
// by updateShell and enterScrollModeLocked so the two never diverge.
func webTabPlaceholder(url string) string {
	return fmt.Sprintf("%s\n\nweb tab — view in the web UI or open in a browser", url)
}

// vscodeTabPlaceholder is the TUI content for a VS Code tab. Unlike a web tab
// there is no URL to show: the editor is a daemon-managed per-session
// code-server on an ephemeral loopback port, reachable only through the daemon's
// proxy, so the only meaningful pointer is the web UI itself.
func vscodeTabPlaceholder() string {
	return "VS Code tab — view in the web UI\n\nThe editor opens this session's worktree. A terminal can't render it."
}

// tabPlaceholder returns the TUI placeholder for a tab kind the terminal cannot
// render, and ok=false for kinds it can (agent/shell/process, which have a PTY).
// Shared by updateShell and enterScrollModeLocked so the two never diverge.
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
		p.setFallbackState("Tearing down session...")
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
	// clears scroll state, so String() (which checks isScrolling first) renders the
	// fallback message. This mirrors the agent slot, whose Dead check also precedes
	// its scroll guard in updateAgent.
	if !instance.TabAlive(activeTab) {
		p.setFallbackState("Terminal session not available.")
		p.mu.Unlock()
		return nil
	}

	// Scroll mode with a pending fill: capture the tab's full scrollback here —
	// off the event loop, exactly like the agent slot. ScrollUp/ScrollDown no
	// longer capture inline (that blocked the bubbletea event loop on a slow tmux
	// capture / daemon RPC — #1637); the shell slot fills lazily on this off-loop
	// refresh goroutine, the first time UpdateContent runs with a pending fill.
	if p.isScrolling && p.scrollFillPending {
		gen := p.scrollFillGen
		p.mu.Unlock()
		content, err := p.previewSrc(instance, activeTab, true)
		p.mu.Lock()
		defer p.mu.Unlock()
		// Stale-generation completion (scroll exit+re-entry during the capture):
		// ignore it entirely, so this old capture can't satisfy or clear the newer
		// scroll entry's fill nor publish its stale content (#1709 review). A
		// matching generation guarantees the fill is still owed and current.
		if p.scrollFillGen != gen {
			return nil
		}
		if !guardOK(guard) || !p.isCurrentViewLocked(instance, activeTab) {
			// Could not publish for the live render binding: re-arm so the owed
			// fill re-dispatches rather than wedging the viewport blank (#1709).
			p.rearmScrollFill()
			return nil
		}
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				// setFallbackState clears scroll state and the stale viewport, so
				// the fallback renders even mid scroll-capture (#940/#977).
				p.setFallbackState("Terminal session no longer running.")
				return nil
			}
			p.rearmScrollFill()
			return err
		}
		p.viewport.SetContent(lipgloss.JoinVertical(lipgloss.Left, content, scrollFooter()))
		p.viewport.GotoBottom()
		p.resetScrollFill()
		return nil
	}

	// Already-filled scroll viewport: leave it (and p.content) untouched so
	// LineUp/LineDown keep their position.
	if p.isScrolling {
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
	p.content = tabContentState{fallback: false, text: content}
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
	if p.isScrolling {
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
	// strings.Split produces a trailing empty element when text ends in "\n"
	// (common for capture-pane output). Drop it so the off-by-one does not
	// trigger truncation when content actually fits, and so the truncate branch
	// keeps the right slice of lines (#649/#898).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if instance == nil {
		return nil
	}
	// Reset scroll mode if the view changed out from under us, so we capture the
	// newly selected view's content rather than scrolling stale content (#702).
	p.dropStaleView(instance, activeTab)
	if !p.isScrolling {
		if err := p.enterScrollModeLocked(instance, activeTab); err != nil {
			return err
		}
		return nil
	}
	p.viewport.LineUp(1)
	return nil
}

// ScrollDown enters scroll mode (if not already) and scrolls down.
func (p *TabPane) ScrollDown(instance *session.Instance, activeTab int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if instance == nil {
		return nil
	}
	p.dropStaleView(instance, activeTab)
	if !p.isScrolling {
		if err := p.enterScrollModeLocked(instance, activeTab); err != nil {
			return err
		}
		return nil
	}
	p.viewport.LineDown(1)
	return nil
}

// enterScrollModeLocked switches the pane into scroll mode WITHOUT capturing.
// The full-scrollback capture used to run inline here, on the bubbletea event
// loop — an unbounded tmux capture / daemon Preview RPC that froze the whole TUI
// while entering scroll mode if that capture was slow or hung (#1637). It now
// only flips scroll state and empties the viewport; the capture happens off the
// event loop on the next UpdateContent (the updateAgent/updateShell lazy-fill,
// keyed on an empty scroll viewport), which the app dispatches immediately on
// scroll entry (panesRefresh bypasses its throttle while NeedsScrollFill). Caller
// must hold p.mu. Validation uses in-memory state only, never I/O.
func (p *TabPane) enterScrollModeLocked(instance *session.Instance, activeTab int) error {
	if instance.IsTearingDown() {
		p.setFallbackState("Tearing down session...")
		return nil
	}
	// A web/vscode tab has no scrollback (no PTY): keep the placeholder rather than
	// entering scroll mode over an empty capture. Mirrors updateShell's branch.
	if tabs := instance.GetTabs(); activeTab >= 0 && activeTab < len(tabs) {
		if placeholder, ok := tabPlaceholder(tabs[activeTab]); ok {
			p.setFallbackState(placeholder)
			return nil
		}
	}
	// An already-dead shell tab transitions to the fallback rather than entering
	// scroll mode over stale terminal output: leaving p.content intact with
	// isScrolling==false would render the last capture instead of the dead-session
	// message. Mirrors updateShell's !TabAlive branch (#998, sibling of #977/#984).
	if activeTab != 0 && !instance.TabAlive(activeTab) {
		p.setFallbackState("Terminal session not available.")
		return nil
	}
	// Empty the viewport and mark a fill pending so the off-loop lazy-fill
	// captures the full scrollback and pins it to the bottom. Capture is keyed off
	// the passed instance + tab index there, never a cached title, so the wrong
	// view can never be captured (#746/#384).
	p.viewport.SetContent("")
	p.isScrolling = true
	p.scrollFillPending = true
	// Start a new generation: this entry's fill is owed and undispatched
	// (scrollFillDispatchedGen now trails scrollFillGen), and any capture still in
	// flight from a previous scroll session is stamped an older generation, so it
	// can't satisfy this entry (#1709).
	p.scrollFillGen++
	return nil
}

// ResetToNormalMode exits scroll mode and returns to normal content display.
func (p *TabPane) ResetToNormalMode(instance *session.Instance, activeTab int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Always clear scroll state first so pressing ESC while no instance is
	// selected (e.g. the sidebar header) does not leave the pane stuck on stale
	// viewport content.
	wasScrolling := p.isScrolling
	if wasScrolling {
		p.isScrolling = false
		p.resetScrollFill()
		p.viewport.SetContent("")
		p.viewport.GotoTop()
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
		p.setFallbackState("Setting up workspace...")
	case instance.IsTearingDown():
		p.setFallbackState("Tearing down session...")
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
