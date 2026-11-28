package session

import "github.com/sachiniyer/agent-factory/session/tmux"

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

// TabDead reports whether the tab at idx owns no live PTY, as a LOSSY bool in
// the opposite direction of TabAlive: true means "provably ended" — the tab's
// exit is stamped, its held pane is dead (pane_dead, the remain-on-exit
// contract process tabs have had since #4479 and shell tabs since #7944), or
// its session is definitively gone — and false means "live, or could not be
// proven dead". The consumer is interactive-mode entry (#7944): entering a
// dead pane binds a terminal that swallows every keystroke, so a provable
// death refuses there rather than after the user has typed. A remote tab whose
// death its agent-server has not yet stamped answers false — the bind reaches
// the remote pane, which shows its own dead screen — the same
// alive-or-unknown direction TabAlive takes for a wedged server (#1962).
func (i *Instance) TabDead(idx int) bool {
	i.mu.RLock()
	var exit *TabExit
	var ts *tmux.TmuxSession
	if idx >= 0 && idx < len(i.Tabs) {
		exit = i.Tabs[idx].Exit
		ts = i.Tabs[idx].tmux
	}
	i.mu.RUnlock()
	if exit != nil {
		return true
	}
	if ts == nil {
		return false
	}
	return ts.PaneDead() || !ts.ExistsOrUnknown()
}
