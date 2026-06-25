package ui

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var tabPaneStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#1a1a1a", Dark: "#dddddd"})

var tabPaneFooterStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"})

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
//   - #579 UpdateContent serialises capture writes against String() reads via
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
	// mu serialises UpdateContent (called off the bubbletea Update goroutine via
	// refreshPanesCmd) against String() (called from the renderer), plus the
	// scroll-mode mutators. Without it the renderer can observe partially
	// written state while a capture is in flight (#579).
	mu sync.Mutex

	width  int
	height int

	content     tabContentState
	isScrolling bool
	viewport    viewport.Model

	// currentInstance + currentTab identify the (instance, tab-index) view
	// currently rendered. UpdateContent/ScrollUp/ScrollDown reset scroll-mode
	// state when either changes so switching instances OR tabs while scrolling
	// does not leave the viewport pinned on the previous view's content
	// (#470/#702/#746). currentTab is the 0-based tab index (0 is the agent tab);
	// it is also used to resize the active shell tab's detached session when the
	// pane is resized.
	currentInstance *session.Instance
	currentTab      int
}

func NewTabPane() *TabPane {
	return &TabPane{
		viewport: viewport.New(0, 0),
	}
}

// IsScrolling reports whether the pane is in scroll mode. Locks p.mu to match
// the mutators (#579).
func (p *TabPane) IsScrolling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isScrolling
}

func (p *TabPane) SetSize(width, maxHeight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.width = width
	p.height = maxHeight
	p.viewport.Width = width
	p.viewport.Height = maxHeight
	// Keep the active shell tab's detached session sized to the pane so its
	// capture matches what the agent preview shows (the old TerminalPane.SetSize
	// behavior, generalized onto the Instance's tab index).
	if p.currentInstance != nil && p.currentTab != 0 {
		_ = p.currentInstance.SetTabDetachedSize(p.currentTab, width, maxHeight)
	}
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
	p.viewport.SetContent("")
}

// UpdateContent captures the selected tab's content. Safe to call from a
// goroutine — it serialises against String() via p.mu (#579). activeTab is the
// 0-based selected tab index: index 0 is the agent tab (captured via the backend
// preview), any other index is a shell/process tab captured straight from that
// tab's tmux session. The slot selects the capture source and the fallback copy,
// so the merged pane reproduces the old PreviewPane and TerminalPane behaviors.
func (p *TabPane) UpdateContent(instance *session.Instance, activeTab int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropStaleView(instance, activeTab)
	if activeTab == 0 {
		return p.updateAgentLocked(instance)
	}
	return p.updateShellLocked(instance, activeTab)
}

// updateAgentLocked reproduces the former PreviewPane.UpdateContent. Caller
// must hold p.mu.
func (p *TabPane) updateAgentLocked(instance *session.Instance) error {
	switch {
	case instance == nil:
		p.setFallbackState("No agents running yet. Spin up a new instance with 'n' to get started!")
		return nil
	case instance.GetStatus() == session.Loading:
		p.setFallbackState("Setting up workspace...")
		return nil
	case instance.GetStatus() == session.Deleting:
		// Mirror the Loading case for the other transient status (#920): during
		// teardown Preview() returns ("", nil) and Started()==false, so without
		// this the generic name fallback below would claim throughout the delete.
		p.setFallbackState("Tearing down session...")
		return nil
	case instance.GetStatus() == session.Dead:
		// The metadata tick marks a session Dead once its backing session is
		// gone (#935); keying off the status makes the fallback synchronous with
		// the sidebar's dead-dot so the panes never disagree.
		p.setFallbackState("Session no longer running.")
		return nil
	}

	// If in scroll mode but the viewport hasn't been filled yet, capture the
	// full scrollback now (the agent slot fills lazily here; see ScrollUp).
	if p.isScrolling && p.viewport.Height > 0 && len(p.viewport.View()) == 0 {
		content, err := instance.PreviewFullHistory()
		if err != nil {
			if errors.Is(err, tmux.ErrSessionGone) {
				// setFallbackState clears scroll state and the stale viewport, so
				// the fallback renders even mid scroll-capture (#940).
				p.setFallbackState("Session no longer running.")
				return nil
			}
			return err
		}
		p.viewport.SetContent(lipgloss.JoinVertical(lipgloss.Left, content, scrollFooter()))
		return nil
	}

	if p.isScrolling {
		return nil
	}

	content, err := instance.Preview()
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

// updateShellLocked reproduces the former TerminalPane.UpdateContent for the
// shell/process tab at activeTab. Caller must hold p.mu.
func (p *TabPane) updateShellLocked(instance *session.Instance, activeTab int) error {
	if instance == nil {
		p.setFallbackState("Select an instance to open a terminal")
		return nil
	}
	// A Deleting instance reports Started()==false during teardown, so without
	// this it would fall through to the "not started yet" fallback — misleading
	// while the session is going away (#920).
	if instance.GetStatus() == session.Deleting {
		p.setFallbackState("Tearing down session...")
		return nil
	}
	if !instance.Started() {
		p.setFallbackState("Instance is not started yet.")
		return nil
	}

	// Remote instances have no local shell tab. When terminal_cmd is configured
	// the tab is an interactive-only surface (#843): prompt the user to attach.
	// Otherwise keep the "not available" fallback and name the config knob.
	if instance.IsRemote() {
		if instance.SupportsRemoteTerminal() {
			p.setFallbackState("Press Enter to open a terminal on the remote machine.")
		} else {
			p.setFallbackState("Terminal tab not available for remote sessions.\nConfigure remote_hooks.terminal_cmd to enable it.\nUse the Preview tab to see session output.")
		}
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
	// its scroll guard in updateAgentLocked.
	if !instance.TabAlive(activeTab) {
		p.setFallbackState("Terminal session not available.")
		return nil
	}

	// Skip content updates while scrolling (the shell slot fills its viewport
	// eagerly in enterScrollMode, not here).
	if p.isScrolling {
		return nil
	}

	content, err := instance.PreviewTab(activeTab)
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

// String renders the pane content.
func (p *TabPane) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	// In scroll/copy mode always use the viewport.
	if p.isScrolling {
		return p.viewport.View()
	}

	if p.content.fallback {
		// TabbedWindow.SetSize already subtracts borders/margins/padding from
		// p.height, so use it directly to match normal mode. Subtracting again
		// would double-count chrome and leave a trailing blank line (#616/#703).
		// renderCenteredFallback centers using the wrapped line count so narrow
		// panes don't miscenter (#699).
		return renderCenteredFallback(tabPaneStyle, p.content.text, p.width, p.height)
	}

	lines := strings.Split(p.content.text, "\n")
	// strings.Split produces a trailing empty element when text ends in "\n"
	// (common for capture-pane output). Drop it so the off-by-one does not
	// trigger truncation when content actually fits, and so the truncate branch
	// keeps the right slice of lines (#649/#898).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if p.height > 0 {
		if len(lines) > p.height {
			// Show the newest output, not the oldest (#649).
			lines = lines[len(lines)-p.height:]
		} else {
			lines = append(lines, make([]string, p.height-len(lines))...)
		}
	}

	return tabPaneStyle.Width(p.width).Render(strings.Join(lines, "\n"))
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

// enterScrollModeLocked captures the selected view's full scrollback and enters
// scroll mode. Caller must hold p.mu. Capture is keyed off the passed instance
// + tab index, never a cached title, so the wrong view can never be captured
// (#746/#384).
func (p *TabPane) enterScrollModeLocked(instance *session.Instance, activeTab int) error {
	var content string
	var err error
	if activeTab == 0 {
		content, err = instance.PreviewFullHistory()
	} else {
		if !instance.TabAlive(activeTab) {
			return nil
		}
		content, err = instance.PreviewTabFullHistory(activeTab)
	}
	if err != nil {
		if errors.Is(err, tmux.ErrSessionGone) {
			if activeTab == 0 {
				p.setFallbackState("Session no longer running.")
			} else {
				p.setFallbackState("Terminal session no longer running.")
			}
			return nil
		}
		return err
	}

	p.viewport.SetContent(lipgloss.JoinVertical(lipgloss.Left, content, scrollFooter()))
	p.viewport.GotoBottom()
	p.isScrolling = true
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
		p.viewport.SetContent("")
		p.viewport.GotoTop()
	}

	if instance == nil || !wasScrolling {
		return nil
	}

	// A shell/process slot simply returns to live capture on the next refresh —
	// no re-capture here (the former TerminalPane.ResetToNormalMode behavior).
	if activeTab != 0 {
		return nil
	}

	// Agent slot: immediately restore content instead of waiting for the next
	// UpdateContent, but keep transient/dead fallbacks rather than blanking the
	// pane on an empty/erroring Preview() (#823/#920/#935).
	switch instance.GetStatus() {
	case session.Loading:
		p.setFallbackState("Setting up workspace...")
		return nil
	case session.Deleting:
		p.setFallbackState("Tearing down session...")
		return nil
	case session.Dead:
		p.setFallbackState("Session no longer running.")
		return nil
	}
	content, err := instance.Preview()
	if err != nil {
		if errors.Is(err, tmux.ErrSessionGone) {
			p.setFallbackState("Session no longer running.")
			return nil
		}
		return err
	}
	p.content = tabContentState{fallback: false, text: content}
	return nil
}

func scrollFooter() string {
	return tabPaneFooterStyle.Render("ESC to exit scroll mode")
}
