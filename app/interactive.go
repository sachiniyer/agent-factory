package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// This file hosts interactive mode's entry and key routing (#1089 PR 2, RFC
// §2.3). Enter on a live-eligible pane enters the pane: every keystroke —
// including Tab — forwards down the pane's termpane attachment to the
// agent/shell, in place, with the instances rail still visible. Ctrl-] (and
// ONLY Ctrl-]) returns to nav mode. Full-screen attach stays reachable on
// `o` (handleAttach); panes that cannot embed (remote instances, dead
// sessions) fall back to it from Enter too.
//
// The mode itself is the `interactive` flag on home (see its doc), flipped
// through setInteractive and policed by enforceInteractiveInvariant
// (live_termpane.go).

// enterInteractiveMsg asks the event loop to activate interactive mode on a
// pane. It exists because the first-time help screen defers the activation
// to its dismiss callback, which runs as a tea.Cmd — off the event loop,
// where model state must not be touched (#716 capture discipline: the pane
// is captured at Enter-press time, then re-validated on arrival).
type enterInteractiveMsg struct {
	pane      *store.OpenPane
	replayKey tea.KeyMsg
	replay    bool
}

// requestInteractive routes Enter-on-a-live-eligible-pane through the
// first-time interactive help screen (seen-bitmask, like the attach help)
// into activation. A non-nil replayKey is the keystroke that triggered the
// entry (an already-focused pane's Enter): it is forwarded into the pane on
// activation so the transition key is not swallowed (#1576) — the same
// contract the first-run help path already honors by replaying its dismiss
// key. The tree/nav select path and the mouse click pass nil: a navigation
// Enter opens the pane without also typing into the agent, and a click has no
// keystroke to forward. When the first-run help IS shown, the dismiss key
// replay (replayKeyAfterInteractiveHelpDismiss) overrides this key, so the
// entry key that only surfaced the overlay is never double-forwarded.
func (m *home) requestInteractive(p *store.OpenPane, replayKey *tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.showHelpScreen(helpTypeInteractive{}, func() tea.Cmd {
		return func() tea.Msg {
			msg := enterInteractiveMsg{pane: p}
			if replayKey != nil {
				msg.replay = true
				msg.replayKey = *replayKey
			}
			return msg
		}
	})
}

// activateInteractive focuses the pane, binds its live attachment
// immediately (no waiting for the 100ms tick), and flips the mode on. The
// pane pointer was captured at Enter-press time; it is re-validated against
// the store because the help overlay (or an async event) may have closed it
// in the meantime (#716 class).
func (m *home) activateInteractive(p *store.OpenPane) tea.Cmd {
	stillOpen := false
	for _, q := range m.store.OpenPanes() {
		if q == p {
			stillOpen = true
			break
		}
	}
	if !stillOpen {
		return nil
	}
	// The session may have changed state while the help overlay was up
	// (e.g. gone Lost, #1108): re-run the Enter guard so the user gets the
	// same truthful error Enter would give now, not a generic bind failure.
	if err := interactiveGuard(p.Instance()); err != nil {
		return m.handleError(err)
	}

	// Focus (and, via the recency touch, un-auto-hide) the pane, then force
	// the live bind for it now. The first bind after a preview opens can lose a
	// race with tmux finishing the pane, so retry the transient miss a bounded
	// number of times before surfacing the error (#1526) — the common
	// first-render race stays invisible; a genuine failure still surfaces the
	// "press o" guidance promptly.
	m.store.TouchOpenPane(p)
	m.focusRegion(layout.PaneRegion(p.ID()))

	if !m.bindLiveTermPaneWithRetry(p) {
		inst := p.Instance()
		title := ""
		if inst != nil {
			title = inst.Title
		}
		return m.handleError(fmt.Errorf("couldn't open an embedded terminal for '%s' — try again, or press o to attach full-screen", title))
	}
	m.setInteractive(true)
	return nil
}

// handleInteractiveKey is the whole keyboard while interactive: Ctrl-] pops
// back to nav; everything else forwards down the live attachment. A key the
// translation table cannot encode is IGNORED — never a guessed byte
// sequence (the #1089 input contract; the honest not-forwarded list lives on
// termpane.translateKey).
func (m *home) handleInteractiveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlCloseBracket {
		m.setInteractive(false)
		return m, nil
	}
	// The attachment may have died since the last tick (client killed, pane
	// pruned). Re-check before forwarding; a broken premise drops to nav and
	// swallows this one keystroke rather than typing into nothing.
	m.enforceInteractiveInvariant()
	if !m.interactive {
		return m, nil
	}
	m.liveTerm.SendKey(msg)
	return m, nil
}
