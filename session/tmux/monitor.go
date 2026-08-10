package tmux

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

// setMonitor swaps in a new status monitor under monitorMu.
func (t *TmuxSession) setMonitor(m *statusMonitor) {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
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
