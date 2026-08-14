package session

// HasUpdatedWithBaseline preserves the local tmux monitor's reattach-baseline
// signal for AgentServer.Snapshot. Backend.HasUpdated remains the compatibility
// projection for callers that only need changed/prompt/content.
func (b *LocalBackend) HasUpdatedWithBaseline(i *Instance) (updated bool, hasPrompt bool, content string, baseline bool) {
	i.mu.RLock()
	s := i.started
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if !s || ts == nil {
		return false, false, "", false
	}
	return ts.HasUpdatedWithBaseline()
}

// resetAgentBrokerCaptures is the single post-replacement hook for every local
// pane respawn. A handoff and a recovery both replace the process behind the
// same stable tab; retaining a broker capture from either old pane strands every
// existing and future subscriber on a silent read loop.
func resetAgentBrokerCaptures(i *Instance) {
	i.noteAgentRuntimeReplaced()
	if as, ok := i.AgentServer().(*localAgentServer); ok {
		as.resetBrokerCaptures()
	}
}
