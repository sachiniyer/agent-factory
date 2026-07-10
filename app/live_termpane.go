package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/termpane"
)

// This file hosts the lifecycle of the ONE live embedded-terminal attachment
// (#1089 PR 1, read-only): the most-recently-relevant visible pane — the
// focused pane, or the pane that already holds the attachment while focus
// visits the tree/automations — renders a termpane projection of its tab's
// tmux session instead of polling capture-pane. This is deliberately a
// single-attachment proof path: binding every visible pane (and forwarding
// keystrokes) is the next #1089 PR.
//
// The attachment is a pure render client. It is closed — killing the attach
// CLIENT only, never the session — whenever its pane hides or its instance
// dies, before any full-screen attach (two clients on one session would
// fight over the session size, and #598 taught us to keep our own tmux
// traffic out of an interactive client's way), and on quit so no attach
// child outlives the TUI. Everything here runs on the bubbletea event loop.

// liveBindRetryInterval is the backoff after a failed bind (session gone,
// attach spawn error, or the client dying right after spawn). Without it a
// dead-but-still-open pane would respawn a doomed `tmux attach-session`
// child every 100ms tick. Capture rendering covers the pane in the meantime.
const liveBindRetryInterval = 5 * time.Second

// liveDeathLogInterval is how often the client-died warning repeats for the
// SAME binding: one line when the client first dies, then one per interval
// while a respawn-die loop persists — enough to diagnose a silent loop
// without a warning every 5s retry.
const liveDeathLogInterval = time.Minute

// liveTermAttachment is what the app needs from a termpane: ui.LiveView
// (render + resize) plus the lifecycle half (close, death signal) and the
// interactive-mode key sink (#1089 PR 2). It exists so tests can drive the
// bind/unbind/forward state machine with a fake instead of spawning real
// `tmux attach-session` clients.
type liveTermAttachment interface {
	Render(width, height int, showCursor bool) string
	Resize(width, height int)
	Close() error
	Done() <-chan struct{}
	// SendKey forwards one keystroke down the attachment's PTY, reporting
	// false when the key has no safe encoding (ignored, never guessed).
	SendKey(msg tea.KeyMsg) bool
	// SendMouse forwards one mouse event at grid cell (x, y) — the
	// interactive-mode in-pane mouse path (#1024 R4, RFC §2.5). The
	// emulator is mode-aware: the event reaches the inner app only if it
	// enabled mouse tracking, and is dropped otherwise.
	SendMouse(msg tea.MouseMsg, x, y int) bool
}

// newLiveTermPaneFn is the termpane creation seam. Production points it at
// termpane.New; tests swap in a fake factory. Read on the event loop only.
var newLiveTermPaneFn = func(sessionName string, width, height int) (liveTermAttachment, error) {
	return termpane.New(sessionName, width, height)
}

// syncLiveTermPane reconciles the live attachment with the current focus,
// pane visibility, and instance health, then re-checks the interactive-mode
// invariant against the outcome (#1089 PR 2: the mode cannot outlive its
// attachment). Called from the 100ms preview tick; steady-state cost is
// pointer compares plus one non-blocking channel read.
func (m *home) syncLiveTermPane() {
	m.reconcileLiveTermPane()
	m.enforceInteractiveInvariant()
}

// interactiveBindAttempts bounds how hard activateInteractive tries to bind the
// live attachment before surfacing the open error. The embedded terminal can
// miss on the FIRST attempt when the tmux pane isn't ready yet — a first-render
// race that self-heals on the next tick (#1526). A few short retries make that
// common transient miss invisible; the hard, non-retryable cases (remote, dead,
// lost, transitional instances) are already fenced off by interactiveGuard
// before we get here, so what remains at the bind is the transient class.
const interactiveBindAttempts = 4

// interactiveBindRetryDelay is the pause between bind attempts. attempts × delay
// (≈120ms) bounds the worst-case event-loop block below what reads as a stall,
// so a genuine failure still surfaces promptly and the loop never spins. A var
// so tests can zero it; event-loop only, never read/written concurrently.
var interactiveBindRetryDelay = 40 * time.Millisecond

// bindLiveTermPaneWithRetry forces the live attachment for pane p, retrying the
// transient "tmux pane not ready" miss (#1526) a bounded number of times before
// giving up. reconcileLiveTermPane records a failed bind with a 5s backoff meant
// for the passive preview tick; between attempts we clear that backoff so the
// next attempt actually re-tries the bind instead of short-circuiting on it. The
// final failure leaves the backoff intact so the passive tick keeps its
// anti-respawn behavior. Returns true once the pane is bound. Event-loop only.
func (m *home) bindLiveTermPaneWithRetry(p *store.OpenPane) bool {
	for attempt := 0; attempt < interactiveBindAttempts; attempt++ {
		m.syncLiveTermPane()
		if m.liveTerm != nil && m.livePane == p {
			return true
		}
		if attempt == interactiveBindAttempts-1 {
			break
		}
		m.liveBindKey = ""
		m.liveBindFailedAt = time.Time{}
		time.Sleep(interactiveBindRetryDelay)
	}
	return false
}

func (m *home) reconcileLiveTermPane() {
	// While the user is inside a full-screen attach our render client must
	// not hold the same session (size fight) or generate tmux traffic
	// (#598). The attach dispatch path already closed it; this covers any
	// path that flips the flag without going through showHelpScreen.
	if m.attached.Load() {
		m.closeLiveTermPane()
		return
	}

	// Reap an attachment whose client died on its own: session killed, tmux
	// server gone, or an external `tmux detach-client`. The pane falls back
	// to capture rendering; the backoff below stops a respawn-die loop when
	// the session is truly gone. This is the ONLY signal a spawned-then-died
	// client leaves, so it must log — rate-limited, or a persistent
	// respawn-die loop (e.g. the client attaching to the wrong tmux server)
	// would emit a warning every retry.
	if m.liveTerm != nil {
		select {
		case <-m.liveTerm.Done():
			if m.liveBindKey != m.liveDeathLogKey || time.Since(m.liveDeathLogAt) >= liveDeathLogInterval {
				m.liveDeathLogKey = m.liveBindKey
				m.liveDeathLogAt = time.Now()
				log.WarningLog.Printf("termpane: live client for %s died after %v (pane falls back to capture; rebind retries every %v)",
					m.liveBindKey, time.Since(m.liveBoundAt).Round(time.Millisecond), liveBindRetryInterval)
			}
			m.closeLiveTermPane()
			m.liveBindFailedAt = time.Now()
		default:
		}
	}

	// The binding target: the focused pane, else the pane already holding
	// the attachment as long as it stays visible — so Tab-cycling through
	// the tree doesn't churn attach clients.
	target := m.focusedOpenPane()
	if target == nil && m.livePane != nil {
		for _, p := range m.visiblePanes {
			if p == m.livePane {
				target = p
				break
			}
		}
	}

	key, sessionName := m.liveBindCandidate(target)
	if key == "" {
		m.closeLiveTermPane()
		m.liveBindKey = ""
		return
	}
	if key == m.liveBindKey {
		if m.liveTerm != nil {
			return // already bound and healthy
		}
		if time.Since(m.liveBindFailedAt) < liveBindRetryInterval {
			return
		}
	}

	// New binds only from stateDefault: an overlay can have a deferred
	// full-screen attach pending (the first-time attach help screen), and
	// rebinding under it would race the attach it just made room for. An
	// existing healthy binding is left alone while overlays are open.
	if m.state != stateDefault {
		return
	}

	m.closeLiveTermPane()
	m.liveBindKey = key

	w := m.paneWindows[target.ID()]
	if w == nil {
		// No window for this pane: record the failure so the passive 5s backoff
		// applies. liveBindKey stays set (not reset to "") so the backoff
		// key-match engages next tick instead of re-attempting every tick
		// (#1526 review).
		m.liveBindFailedAt = time.Now()
		return
	}
	width, height := w.GetPreviewSize()
	if width < 2 || height < 2 {
		// Not laid out yet (or degenerate): don't attach at a size that would
		// shrink the tmux session. Record the failure so the passive 5s backoff
		// applies too — without it, an interactive retry that clears the backoff
		// between attempts (bindLiveTermPaneWithRetry) would leave the tick
		// re-attempting this unavailable pane every 100ms. liveBindKey stays set
		// so the backoff key-match engages (#1526 review).
		m.liveBindFailedAt = time.Now()
		return
	}
	if !target.Instance().TabAlive(target.Tab()) {
		m.liveBindFailedAt = time.Now()
		return
	}
	tp, err := newLiveTermPaneFn(sessionName, width, height)
	if err != nil {
		log.WarningLog.Printf("termpane: attach to %s failed: %v (pane falls back to capture)", sessionName, err)
		m.liveBindFailedAt = time.Now()
		return
	}
	m.liveTerm = tp
	m.livePane = target
	m.liveBoundAt = time.Now()
	w.SetLive(tp)
}

// enforceInteractiveInvariant drops back to nav mode whenever interactive
// mode's premise breaks: the mode means "keystrokes forward into the FOCUSED
// pane's live attachment", so a dead client, a closed/hidden pane, or focus
// moved by a relayout (auto-hide on shrink) each end it. Every keystroke and
// every sync tick funnels through this, so the stale-mode window is at most
// one 100ms tick. Idempotent.
func (m *home) enforceInteractiveInvariant() {
	if !m.interactive {
		return
	}
	if m.liveTerm == nil || m.livePane == nil || m.livePane != m.focusedOpenPane() {
		m.setInteractive(false)
	}
}

// setInteractive flips interactive mode (#1089, RFC §2.3) and keeps every
// dependent surface coherent: the status bar collapses to (or restores from)
// the Ctrl-] escape hatch, and the pane windows' green interactive cue
// follows the live pane. Idempotent; event-loop only.
func (m *home) setInteractive(on bool) {
	m.interactive = on
	m.menu.SetInteractive(on)
	for id, w := range m.paneWindows {
		w.SetInteractive(on && m.livePane != nil && id == m.livePane.ID())
	}
}

// liveBindCandidate resolves the pane to a bind key + tmux session name, or
// ("", "") when the pane is not eligible for a live attachment: remote
// instances (no local session), not-started/transitional/dead instances, and
// tabs with no session all keep rendering through capture-pane. The key
// changes whenever the pane, its tab index, or the underlying session name
// changes, which is exactly when a rebind is needed.
func (m *home) liveBindCandidate(target *store.OpenPane) (key, sessionName string) {
	if target == nil {
		return "", ""
	}
	tab := target.Tab()
	name := liveSessionName(target.Instance(), tab)
	if name == "" {
		return "", ""
	}
	return fmt.Sprintf("%d/%d/%s", target.ID(), tab, name), name
}

// liveSessionName resolves an (instance, tab) to the tmux session a live
// attachment would target, or "" when embedding is not possible: remote
// instances (no local session), not-started/transitional/dead/lost instances,
// and tabs with no session. Of the "" cases, remote is the one where Enter
// falls back to the full-screen attach path (#1089 PR 2);
// dead/lost/transitional instances are fenced off earlier by
// interactiveGuard's explicit errors.
func liveSessionName(inst *session.Instance, tab int) string {
	if inst == nil || inst.IsRemote() || !inst.Started() {
		return ""
	}
	// No live session name for a row with an in-flight op (create/kill/archive)
	// or a vanished session (Dead/Lost) (#1195, was the Loading/Deleting/Dead/Lost
	// status check). A LimitReached agent is still alive (throttled), so it keeps
	// its name and stays attachable.
	if inst.HasInFlightOp() {
		return ""
	}
	switch inst.GetLiveness() {
	case session.LiveDead, session.LiveLost:
		return ""
	}
	return inst.TabTmuxName(tab)
}

// closeLiveTermPane releases the live attachment: unbind the window's render
// source, then kill the attach client (bounded drain; the tmux session keeps
// running server-side). Idempotent. Deliberately does NOT clear liveBindKey —
// the caller decides whether the binding is gone (key reset) or should back
// off before retrying (liveBindFailedAt).
func (m *home) closeLiveTermPane() {
	if m.liveTerm == nil {
		return
	}
	if m.livePane != nil {
		if w := m.paneWindows[m.livePane.ID()]; w != nil {
			w.SetLive(nil)
		}
	}
	if err := m.liveTerm.Close(); err != nil {
		log.WarningLog.Printf("termpane: close: %v", err)
	}
	m.liveTerm = nil
	m.livePane = nil
}
