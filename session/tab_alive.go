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
