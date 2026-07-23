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
	// An overlay opened between the Enter that captured this pane and this
	// activation — most importantly, the user pressed `o` and the first-time
	// ATTACH help is now up. That makes the activation stale, so drop it and let
	// the later intent win.
	//
	// Load-bearing for #598, not just tidiness: showHelpScreen(helpTypeInstanceAttach)
	// has ALREADY closed the live panes to make room for the full-screen attach it
	// runs on dismiss, but attachTransitioning stays false until then — so this is
	// the one window where neither attach flag is set yet an attach is pending, and
	// binding here would reconnect an embedded client for the attach to fight over
	// the session size. Interactive mode under an overlay is incoherent anyway: the
	// overlay owns the keyboard. Every legitimate path arrives at stateDefault (the
	// help dismiss sets it before firing the deferred cmd), so nothing is lost.
	if m.state != stateDefault {
		return nil
	}
	// The session may have changed state while the help overlay was up
	// (e.g. gone Lost, #1108): re-run the Enter guard so the user gets the
	// same truthful error Enter would give now, not a generic bind failure.
	if err := interactiveGuard(p.Instance()); err != nil {
		return m.handleError(err)
	}

	// Focus (and, via the recency touch, un-auto-hide) the pane, then bind its live
	// attachment now (no waiting for the 100ms tick). Unlike the old tmux attach
	// client, the WS subscription is created immediately and self-heals via
	// reconnect if the session's pane isn't ready yet (#1526 is structurally gone),
	// so a single bind is enough — no retry loop.
	m.store.TouchOpenPane(p)
	m.focusRegion(layout.PaneRegion(p.ID()))

	// Bind through ensureLiveTermPaneFor, NOT a bare reconcile: activation is a
	// deliberate act on THIS pane and must install its attachment itself whenever
	// the pane can stream, instead of inheriting the reconcile's context gates and
	// failing to the `o` fallback when one of them happens to be closed (#1819).
	if !m.ensureLiveTermPaneFor(p) {
		return m.handleError(fmt.Errorf("couldn't open an embedded terminal for %s — press o to attach full-screen", paneErrorLabel(p)))
	}
	m.setInteractive(true)
	return nil
}

// isInteractiveExitKey reports whether msg is keys.KeyExitInteractive (Ctrl-]),
// the ONE host-reserved key inside interactive mode. Everything else forwards to
// the agent/shell.
//
// It is the single definition of that key for the app package, because two
// places need to agree on it and they are not adjacent: handleInteractiveKey
// acts on it, and the enterInteractiveMsg replay must refuse to feed it back in
// (#2413). A second literal tea.KeyCtrlCloseBracket comparison drifting from
// this one is exactly how that bug came back.
func isInteractiveExitKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyCtrlCloseBracket
}

// paneErrorLabel names pane p's session for a user-facing message, falling back
// to a generic phrase when the title is unknown. A pane whose instance vanished
// (or was never titled) used to render the name as an empty pair of quotes,
// which told the reader nothing and read as a formatting bug (#1819).
func paneErrorLabel(p *store.OpenPane) string {
	if p != nil {
		if inst := p.Instance(); inst != nil && inst.Title != "" {
			return "'" + inst.Title + "'"
		}
	}
	return "this pane"
}

// handleInteractiveKey is the whole keyboard while interactive: Ctrl-] pops
// back to nav; everything else forwards down the live attachment. A key the
// translation table cannot encode is IGNORED — never a guessed byte
// sequence (the #1089 input contract; the honest not-forwarded list lives on
// termpane.translateKey).
func (m *home) handleInteractiveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if isInteractiveExitKey(msg) {
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
	if lt, _ := m.focusedLiveTerm(); lt != nil {
		lt.SendKey(msg)
	}
	return m, nil
}
