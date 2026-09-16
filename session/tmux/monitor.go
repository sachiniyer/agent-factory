package tmux

import "time"

// Status-monitor accessors for TmuxSession.
//
// monitor is not immutable: Restore() swaps in a fresh statusMonitor on every
// (re)attach — on the restore/RPC/event-loop goroutines — while the daemon's
// per-second poll reads the pointer and mutates its dead/prevOutputHash fields
// inside HasUpdated(). Left unsynchronized this is a data race (the pointer
// write in Restore vs. the read+field-mutations in HasUpdated), so all access
// goes through monitorMu. HasUpdated() takes the lock only in short bursts —
// snapshot the pointer, then re-acquire to update fields — and deliberately
// does NOT hold it across its `tmux capture-pane` exec, so a slow tmux server
// can't stall Restore's setMonitor(). setMonitor() is the only other writer of
// the pointer (#1528).

// setMonitor swaps in a new status monitor under monitorMu and binds its
// generation. resolved is the generation confirmed live by display-message
// during RestoreWithResult (nil when it did not answer); answered is whether
// the earlier has-session probe answered at all.
//
//   - resolved matches the outgoing monitor's generation: the SAME concrete
//     session answered — the fresh monitor SHARES the generation object, so a
//     close() marking the current monitor still reaches a poll in flight on
//     the swapped-out one (Codex on #4473). A settled mark on a session that
//     just answered live proves the teardown did not take, so the resolution
//     retires it — while an in-flight (unsettled) teardown keeps its mark.
//   - resolved is a different generation: the confirmed-live replacement is
//     not the session af closed, so it starts unmarked — a resolved id is
//     affirmative liveness even when the earlier has-session timed out.
//   - nothing answered (resolved nil, probe unanswered): a wedged server is
//     no evidence the request resolved, so the outgoing generation — mark
//     included — carries to the replacement monitor.
//   - answered but unresolved (has-session answered, display-message gave no
//     id): confirmed live but unbindable — fresh unbound monitor, unmarked.
func (t *TmuxSession) setMonitor(m *statusMonitor, resolved *tmuxGeneration, answered bool) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	switch {
	case resolved != nil && t.monitor != nil && t.monitor.generation.sameAs(resolved):
		m.generation = t.monitor.generation
		if !m.generation.teardownSettledAt.IsZero() {
			m.generation.teardownInitiated = false
			m.generation.teardownSettledAt = time.Time{}
		}
	case resolved != nil:
		m.generation = resolved
	case !answered && t.monitor != nil:
		m.generation = t.monitor.generation
	}
	t.monitor = m
}

// seedDeliveryBaseline records the pane captured in the same tmux command queue
// that submitted Enter. Unlike deferring the baseline to the next daemon poll,
// this cannot absorb a fast response that starts and finishes between delivery
// and that poll.
func (t *TmuxSession) seedDeliveryBaseline(content string) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.prevOutputHash = t.monitor.hash(content)
		t.monitor.baselinePending = false
	}
}

// deferDeliveryBaseline makes the next successful status capture establish
// comparison state without claiming pane churn. This is the honest fallback
// when tmux proves Enter ran but the capture later in that command queue fails:
// there is no post-delivery frame against which the next pane can be compared.
func (t *TmuxSession) deferDeliveryBaseline() {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if t.monitor != nil {
		t.monitor.baselinePending = true
	}
}
