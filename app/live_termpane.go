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

// This file hosts the lifecycle of the live embedded-terminal attachments
// (#1592 Phase 2 PR6): every visible, eligible pane binds its OWN reconnecting
// WebSocket subscription to that pane's (session, tab) PTY stream — fanned from
// the daemon's clientless capture — and renders the streamed grid instead of a
// capture. The FOCUSED pane routes keystrokes (interactive mode) and mouse events
// to its subscription's INPUT; resize sends RESIZE (last-resize-wins, server-side).
//
// The reliability payoff over the old #1089 tmux attach client: a dropped WS
// subscriber reconnects and replays the gap it missed with ?since — there is NO
// capture-pane fallback, no 5s rebind loop, and no client-death signal to poll.
// The termpane's run loop owns resilience; this file just binds/unbinds by pane
// eligibility and focus. Attachments are closed — never killing the session —
// whenever a pane hides, its instance dies, before any full-screen attach (two
// clients on one session would fight over size, #598), and on quit. Everything
// here runs on the bubbletea event loop.

// liveTermAttachment is what the app needs from a termpane: ui.LiveView (render +
// resize) plus the lifecycle half (close) and the interactive-mode input sinks
// (SendKey/SendMouse). It exists so tests can drive the bind/unbind/forward state
// machine with a fake instead of dialing real WS streams. There is deliberately no
// death signal: the WS subscription self-heals via reconnect+replay, so the app
// never falls back to capture (§6.3).
type liveTermAttachment interface {
	Render(width, height int, showCursor bool) string
	Resize(width, height int)
	Close() error
	// SendKey forwards one keystroke down the attachment's stream, reporting false
	// when the key has no safe encoding (ignored, never guessed).
	SendKey(msg tea.KeyMsg) bool
	// SendMouse forwards one mouse event at grid cell (x, y) — the interactive-mode
	// in-pane mouse path (#1024 R4). The emulator is mode-aware: the event reaches
	// the inner app only if it enabled mouse tracking, and is dropped otherwise.
	SendMouse(msg tea.MouseMsg, x, y int) bool
}

// newLiveTermPaneFn is the attachment creation seam. Production dials the daemon
// WS PTY stream for (title, repoID, tab); tests swap in a fake factory. Read on
// the event loop only.
var newLiveTermPaneFn = func(title, repoID string, tab, width, height int) liveTermAttachment {
	return termpane.New(streamDialer(title, repoID, tab), width, height)
}

// syncLiveTermPane reconciles the live attachments with current focus, pane
// visibility, and instance health, then re-checks the interactive-mode invariant
// (the mode cannot outlive its attachment). Called from the 100ms preview tick;
// steady-state cost is a per-visible-pane eligibility check plus map lookups.
func (m *home) syncLiveTermPane() {
	m.reconcileLiveTermPanes()
	m.enforceInteractiveInvariant()
}

// reconcileLiveTermPanes binds a live attachment to every visible, eligible pane
// and closes attachments for panes that are no longer eligible, visible, or that
// changed binding. A WS drop is handled inside the attachment (reconnect+replay),
// so this never respawns on death and needs no backoff.
func (m *home) reconcileLiveTermPanes() {
	// Full-screen attach: no WS panes. A second client on the same session would
	// fight over size, and our stream would generate tmux traffic in an interactive
	// client's way (#598 class). The attach dispatch path already closed them; this
	// covers any path that flips the flag without going through showHelpScreen.
	if m.attached.Load() {
		m.closeAllLiveTermPanes()
		return
	}

	want := make(map[int]string, len(m.visiblePanes))
	for _, p := range m.visiblePanes {
		// A pane showing a transient #1321 preview of a DIFFERENT binding renders the
		// daemon-capture of the preview target, not a live grid — skip it so no live
		// stream paints over the preview.
		if m.paneIsPreviewing(p) {
			continue
		}
		key, title, repoID, tab, ok := m.liveBindCandidate(p)
		if !ok {
			continue
		}
		w := m.paneWindows[p.ID()]
		if w == nil {
			continue
		}
		width, height := w.GetPreviewSize()
		if width < 2 || height < 2 {
			// Not laid out yet (or degenerate): keep any existing attachment but don't
			// create one at a size that would shrink the session's window.
			if m.liveTerms[p.ID()] != nil {
				want[p.ID()] = m.liveKeys[p.ID()]
			}
			continue
		}
		want[p.ID()] = key
		if m.liveTerms[p.ID()] != nil && m.liveKeys[p.ID()] == key {
			continue // already bound; the attachment self-heals, nothing to do
		}
		// New attachments are created only from stateDefault: an overlay can have a
		// deferred full-screen attach pending (the first-time attach help), and
		// creating a subscription under it would race the attach it made room for.
		// An existing attachment with a stale key is left alone under overlays.
		if m.state != stateDefault {
			continue
		}
		m.closeLiveTermPaneFor(p.ID())
		tp := newLiveTermPaneFn(title, repoID, tab, width, height)
		m.liveTerms[p.ID()] = tp
		m.liveKeys[p.ID()] = key
		w.SetLive(tp)
	}

	// Close attachments for panes no longer wanted (hidden, ineligible, or now
	// remote/dead — those fall back to daemon-Preview capture).
	for id := range m.liveTerms {
		if _, keep := want[id]; !keep {
			m.closeLiveTermPaneFor(id)
		}
	}
}

// paneIsPreviewing reports whether pane p currently owns a transient #1321 preview
// binding (so it renders capture, not a live grid).
func (m *home) paneIsPreviewing(p *store.OpenPane) bool {
	return m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID()
}

// interactivePollPauseCmd holds (and renews) the #1160 capture-poll pause lease
// for the session the user is typing into through the FOCUSED interactive pane
// (#1586), and releases it once that stops. Holding the lease makes the daemon
// treat the session as attached and DEFER automated task deliveries into it, so a
// scheduled prompt can't paste into and submit the user's in-progress input.
//
// It runs from the preview tick, AFTER syncLiveTermPane has settled interactive
// mode, and returns a best-effort RPC cmd (or nil) rather than dialing the daemon
// on the event loop. Full-screen attach owns its own heartbeat and closes the
// embedded attachments (dropping interactive mode), so this yields while
// m.attached is set. Event-loop only.
func (m *home) interactivePollPauseCmd() tea.Cmd {
	// The session the user is actively typing into in-pane, if any. Interactive
	// mode is only ever true while the FOCUSED pane has a live attachment, whose
	// instance is a local, started session, so its title keys the same daemon pause
	// map full-screen attach uses.
	want := ""
	if m.interactive && !m.attached.Load() {
		if p := m.focusedOpenPane(); p != nil {
			if inst := p.Instance(); inst != nil {
				want = inst.Title
			}
		}
	}

	// Capture the seams + repoID on the event loop before any goroutine reads them
	// (the #964 per-home-field discipline).
	repoID := m.repoID
	pause := m.pauseStatusPoll
	resume := m.resumeStatusPoll

	if want == "" {
		if m.interactivePauseTitle == "" {
			return nil
		}
		// Interactive ended (or focus left the pane): release the lease now so the
		// daemon resumes delivering into the session immediately.
		release := m.interactivePauseTitle
		m.interactivePauseTitle = ""
		m.interactivePauseAt = time.Time{}
		return func() tea.Msg {
			_ = resume(release, repoID)
			return nil
		}
	}

	if want != m.interactivePauseTitle {
		// Newly interactive on this session (or the focused session changed): release
		// any previous hold and pause the new target.
		prev := m.interactivePauseTitle
		m.interactivePauseTitle = want
		m.interactivePauseAt = time.Now()
		return func() tea.Msg {
			if prev != "" {
				_ = resume(prev, repoID)
			}
			_ = pause(want, repoID)
			return nil
		}
	}

	// Still interactive on the same session: renew the lease, throttled to the same
	// cadence full-screen attach uses (statusPollRenewInterval).
	if time.Since(m.interactivePauseAt) < statusPollRenewInterval {
		return nil
	}
	m.interactivePauseAt = time.Now()
	return func() tea.Msg {
		_ = pause(want, repoID)
		return nil
	}
}

// enforceInteractiveInvariant drops back to nav mode whenever interactive mode's
// premise breaks: the mode means "keystrokes forward into the FOCUSED pane's live
// attachment", so a closed/hidden pane, focus moved off the pane, or a missing
// attachment each end it. Every keystroke and every sync tick funnels through
// this, so the stale-mode window is at most one tick. Idempotent.
func (m *home) enforceInteractiveInvariant() {
	if !m.interactive {
		return
	}
	p := m.focusedOpenPane()
	if p == nil || m.liveTerms[p.ID()] == nil {
		m.setInteractive(false)
	}
}

// setInteractive flips interactive mode (#1089, RFC §2.3) and keeps every
// dependent surface coherent: the status bar collapses to (or restores from) the
// Ctrl-] escape hatch, and the pane windows' green interactive cue follows the
// focused pane. Idempotent; event-loop only.
func (m *home) setInteractive(on bool) {
	m.interactive = on
	m.menu.SetInteractive(on)
	focused := m.focusedOpenPane()
	for id, w := range m.paneWindows {
		w.SetInteractive(on && focused != nil && id == focused.ID())
	}
}

// focusedLiveTerm returns the focused pane's live attachment (and the pane), or
// (nil, nil) when the focus ring isn't on a pane or that pane has no attachment.
// It is the interactive-mode input target.
func (m *home) focusedLiveTerm() (liveTermAttachment, *store.OpenPane) {
	p := m.focusedOpenPane()
	if p == nil {
		return nil, nil
	}
	return m.liveTerms[p.ID()], p
}

// liveBindCandidate resolves a pane to its bind key + stream coordinates (title,
// repoID, tab), or ok=false when the pane is not eligible for a live attachment
// (remote instances, not-started/transitional/dead instances, tabs with no
// session). The key changes whenever the pane, its tab index, or the underlying
// session name changes, which is exactly when a rebind is needed.
func (m *home) liveBindCandidate(p *store.OpenPane) (key, title, repoID string, tab int, ok bool) {
	if p == nil {
		return "", "", "", 0, false
	}
	tab = p.Tab()
	name := liveSessionName(p.Instance(), tab)
	if name == "" {
		return "", "", "", 0, false
	}
	inst := p.Instance()
	return fmt.Sprintf("%d/%d/%s", p.ID(), tab, name), inst.Title, m.repoID, tab, true
}

// liveSessionName resolves an (instance, tab) to the tmux session a live
// attachment would stream, or "" when embedding is not possible: remote instances
// (streamed via daemon Preview capture, not WS), not-started/transitional/dead/
// lost instances, and tabs with no session. It doubles as the eligibility
// predicate the Enter path uses to choose in-pane interactive vs full-screen
// attach.
func liveSessionName(inst *session.Instance, tab int) string {
	// Only a local-worktree backend has a local tmux session to stream; other
	// workspaces (remote hook) render through the daemon Preview capture path.
	if inst == nil || inst.Capabilities().Workspace != session.WorkspaceLocalWorktree || !inst.Started() {
		return ""
	}
	// No live stream for a row with an in-flight op (create/kill/archive) or a
	// vanished session (Dead/Lost) (#1195). A LimitReached agent is still alive
	// (throttled), so it keeps its name and stays streamable.
	if inst.HasInFlightOp() {
		return ""
	}
	switch inst.GetLiveness() {
	case session.LiveDead, session.LiveLost:
		return ""
	}
	return inst.TabTmuxName(tab)
}

// closeLiveTermPaneFor releases one pane's live attachment: unbind the window's
// render source, then close the subscription (the session keeps running). Idempotent.
func (m *home) closeLiveTermPaneFor(paneID int) {
	lt := m.liveTerms[paneID]
	if lt == nil {
		delete(m.liveKeys, paneID)
		return
	}
	if w := m.paneWindows[paneID]; w != nil {
		w.SetLive(nil)
	}
	if err := lt.Close(); err != nil {
		log.WarningLog.Printf("termpane: close pane %d: %v", paneID, err)
	}
	delete(m.liveTerms, paneID)
	delete(m.liveKeys, paneID)
}

// closeAllLiveTermPanes releases every live attachment. Used on full-screen attach
// and quit.
func (m *home) closeAllLiveTermPanes() {
	for id := range m.liveTerms {
		m.closeLiveTermPaneFor(id)
	}
}
