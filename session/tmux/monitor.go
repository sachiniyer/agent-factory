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

// generationResolution is what a rebind learned about the session standing at
// the name, and WHEN it learned it.
//
// startedAt is the instant the resolution began — the same ordering evidence
// HasUpdatedWithBaseline records as captureStartedAt, and it is load-bearing
// for exactly the same reason: liveness observed BEFORE the asking close()
// returned proves nothing about that close's outcome. A zero startedAt reads
// as "no ordering evidence", which fails safe (a mark is kept, so an expected
// teardown can at worst be logged at ERROR — never the reverse).
type generationResolution struct {
	// generation is what display-message resolved at the name; nil when it did
	// not answer or answered nothing provable.
	generation *tmuxGeneration
	// answered is whether the earlier has-session probe answered at all.
	answered bool
	// startedAt is when the resolution probe began; zero when none ran.
	startedAt time.Time
}

// setMonitor swaps in a new status monitor under monitorMu and binds its
// generation from res.
//
//   - res.generation matches the outgoing monitor's generation: the SAME
//     concrete session answered — the fresh monitor SHARES the generation
//     object, so a close() marking the current monitor still reaches a poll in
//     flight on the swapped-out one (Codex on #4473). A settled mark on a
//     session that just answered live proves the teardown did not take, so the
//     resolution retires it — but only a resolution that BEGAN after the
//     settle, per the ordering rule on generationResolution.startedAt. An
//     in-flight (unsettled) teardown keeps its mark.
//   - res.generation is a different generation: the confirmed-live replacement
//     is not the session af closed, so it starts unmarked — a resolved id is
//     affirmative liveness even when the earlier has-session timed out.
//   - nothing answered (generation nil, probe unanswered): a wedged server is
//     no evidence the request resolved, so the outgoing generation — mark
//     included — carries to the replacement monitor.
//   - answered but unresolved (has-session answered, display-message gave no
//     id): confirmed live but unbindable — fresh unbound monitor, unmarked.
func (t *TmuxSession) setMonitor(m *statusMonitor, res generationResolution) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	switch {
	case res.generation != nil && t.monitor != nil && t.monitor.generation.sameAs(res.generation):
		m.generation = t.monitor.generation
		// Same settle-ordering guard the capture path applies in
		// HasUpdatedWithBaseline: a resolution that STRADDLED the close —
		// begun while the kill was still in flight, applied after close()
		// returned — saw the session the kill was about to take, so it is no
		// evidence the teardown failed. Retiring on it would let af's own
		// teardown reach ERROR. The two sites must not disagree about what
		// retires a mark (#4473 review).
		if g := m.generation; !g.teardownSettledAt.IsZero() && res.startedAt.After(g.teardownSettledAt) {
			g.teardownInitiated = false
			g.teardownSettledAt = time.Time{}
		}
	case res.generation != nil:
		m.generation = res.generation
	case !res.answered && t.monitor != nil:
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
