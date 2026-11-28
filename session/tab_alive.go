package session

// TabAlive reports whether the tab at idx has a live tmux session, as a LOSSY
// bool: true means "alive OR could-not-determine (wedged/timeout)", false means
// "no binding, or definitively gone". It is deliberately kept lossy because its
// only consumers (ui/tab_pane.go) act solely on !TabAlive — they swap to the
// "Terminal session not available" fallback. A wedged server reads as alive here,
// which keeps the read-only TUI rendering the pane instead of falsely declaring a
// merely-unreachable terminal dead: the safe direction for a view (#1962). A
// caller that needs existence as EVIDENCE must use TmuxSession.ProbeSession
// directly and handle !known.
//
// A streamed sandbox tab (ssh/sandbox/docker) has no local tmux binding by
// construction; its liveness is the agent-server's roster, which the snapshot
// projects into Tabs: a PTY-bearing tab whose command has not recorded an exit
// is alive (#5874).
func (i *Instance) TabAlive(idx int) bool {
	i.mu.RLock()
	ts := i.tabTmuxAtLocked(idx)
	var rosterTab *Tab
	if ts == nil && idx >= 0 && idx < len(i.Tabs) {
		rosterTab = i.Tabs[idx]
	}
	i.mu.RUnlock()
	if ts != nil {
		return ts.ExistsOrUnknown()
	}
	if rosterTab == nil || !i.StreamsViaAgentServer() {
		return false
	}
	if rosterTab.restoreState != TabRestored {
		// Unknown or confirmed gone: no server holds the terminal right now.
		return false
	}
	switch rosterTab.Kind {
	case TabKindAgent, TabKindShell, TabKindProcess:
		return rosterTab.Exit == nil
	}
	return false
}

// TabDead reports whether the tab at idx is PROVABLY ended — its exit is
// stamped — and nothing else (#7944). It is positive evidence only: every
// probe failure, timeout, or unproven absence answers false, because the
// consumers (the tree row, the pane header, the interactive-entry guard) must
// never steer a user into closing a shell that might be live. It runs no tmux
// call: the stamp is written by the async probe pass (KickShellExitProbe),
// the restore paths, or the roster reconcile, so a read on the Bubble Tea
// loop can never block on tmux. A remote tab whose death its agent-server has
// not yet stamped answers false — the bind reaches the remote pane, which
// shows its own dead screen — the same alive-or-unknown direction TabAlive
// takes for a wedged server (#1962).
func (i *Instance) TabDead(idx int) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return idx >= 0 && idx < len(i.Tabs) && i.Tabs[idx].Exit != nil
}
