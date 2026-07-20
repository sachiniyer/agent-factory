package session

import "fmt"

// Closing one tab: the destructive tab verb. Where tab_arrange.go's rename and
// reorder never touch tmux, a close ends a real process — and, since #1592 Phase
// 2 PR6, a real PTY STREAM. Both halves live here so neither can be updated
// without the other in view: killing the tmux session while leaving that tab's
// broker open is exactly the bug #2136 reported (a PTY-only subscriber blocked
// until its 15s keepalive gave up, with nothing on the wire to say the tab went
// away).

// CloseTab kills the tab at idx, ends its PTY stream, and removes it from Tabs.
// The agent tab (idx 0) is unclosable; CloseTab errors on idx 0 or any
// out-of-range index. The tab is removed from Tabs regardless of whether the tmux
// teardown succeeds (best-effort, matching LocalBackend.Kill) so a broken session
// can't wedge the tab list. Unlike Kill this does not wait for the pane to exit:
// the worktree is not being removed, so there is no #802 delete race to guard
// against.
func (i *Instance) CloseTab(idx int) error {
	i.mu.Lock()
	if idx <= 0 || idx >= len(i.Tabs) {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.Tabs[idx]
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	i.mu.Unlock()

	// End this tab's stream BEFORE its tmux dies — the same order localAgentServer
	// .Kill uses for the session-wide case (brokers first, then the backend), and
	// for the same reason: a subscriber should be told the stream is over rather
	// than left reading a pane that has just been killed out from under it. Run
	// unconditionally, including on the tmux-less paths below, so the notification
	// never depends on how far the teardown gets.
	i.endTabPTYStream(tab.ID)

	if tab.tmux == nil {
		return nil
	}
	// Pane state deliberately ignored: closing ONE tab touches no worktree, so
	// nothing destructive follows an unknown here. (A Close that fails after the
	// tab was already dropped from i.Tabs above leaks the tmux session — true of
	// every Close failure, not just a timeout, and unchanged by #1917.)
	if _, err := tab.tmux.Close(); err != nil {
		return fmt.Errorf("failed to close tab %q: %w", tab.Name, err)
	}
	return nil
}

// endTabPTYStream shuts down the PTY broker serving the tab with this STABLE id
// (#1738), so its subscribers get a prompt end-of-stream (ErrTabClosed → an exit
// with reason "tab_closed" on the wire) instead of a silent socket that only the
// keepalive eventually reaps (#2136). Sibling tabs keep their own brokers and
// keep streaming.
//
// It reads the CACHED agent-server rather than calling AgentServer(): a session
// nobody has streamed has no cached server and therefore no brokers, so there is
// nothing to notify — and building one here purely to close nothing would be a
// side effect on a teardown path. The cached field is read under i.mu, then
// released before the call: closing a broker takes the agent-server's own lock,
// and the ordering rule is i.mu THEN s.mu, never nested the other way.
func (i *Instance) endTabPTYStream(tabID string) {
	i.mu.RLock()
	as := i.agentSrv
	i.mu.RUnlock()
	if c, ok := as.(tabStreamCloser); ok {
		c.closeTabStream(tabID)
	}
}
