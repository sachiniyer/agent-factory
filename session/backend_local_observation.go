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
