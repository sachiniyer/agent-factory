package app

import (
	"fmt"
	"time"

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

// liveTermAttachment is what the sync needs from a termpane: ui.LiveView
// (render + resize) plus the lifecycle half (close, death signal). It exists
// so tests can drive the bind/unbind state machine with a fake instead of
// spawning real `tmux attach-session` clients.
type liveTermAttachment interface {
	Render(width, height int) string
	Resize(width, height int)
	Close() error
	Done() <-chan struct{}
}

// newLiveTermPaneFn is the termpane creation seam. Production points it at
// termpane.New; tests swap in a fake factory. Read on the event loop only.
var newLiveTermPaneFn = func(sessionName string, width, height int) (liveTermAttachment, error) {
	return termpane.New(sessionName, width, height)
}

// syncLiveTermPane reconciles the live attachment with the current focus,
// pane visibility, and instance health. Called from the 100ms preview tick;
// steady-state cost is pointer compares plus one non-blocking channel read.
func (m *home) syncLiveTermPane() {
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
	// the session is truly gone.
	if m.liveTerm != nil {
		select {
		case <-m.liveTerm.Done():
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
		m.liveBindKey = ""
		return
	}
	width, height := w.GetPreviewSize()
	if width < 2 || height < 2 {
		// Not laid out yet (or degenerate): retry next tick rather than
		// attach a client at a size that would shrink the tmux session.
		m.liveBindKey = ""
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
	w.SetLive(tp)
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
	inst := target.Instance()
	if inst == nil || inst.IsRemote() || !inst.Started() {
		return "", ""
	}
	switch inst.GetStatus() {
	case session.Loading, session.Deleting, session.Dead:
		return "", ""
	}
	tab := target.Tab()
	name := inst.TabTmuxName(tab)
	if name == "" {
		return "", ""
	}
	return fmt.Sprintf("%d/%d/%s", target.ID(), tab, name), name
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
