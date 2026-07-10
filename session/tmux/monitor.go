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
