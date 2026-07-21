package session

// AgentModelChange is the live, verified model transition observed after Agent
// Factory handled an agent safety dialog. It is projection state: clients need
// the before/after values to explain a degraded-looking healthy row, but the
// daemon derives it from the running agent and never restores it from disk.
type AgentModelChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// NewAgentModelChange constructs only meaningful transitions. Keeping invalid
// equal/empty pairs out at the boundary means every renderer can treat a non-nil
// value as an actionable diagnostic without duplicating validation policy.
func NewAgentModelChange(before, after string) *AgentModelChange {
	if before == "" || after == "" || before == after {
		return nil
	}
	return &AgentModelChange{Before: before, After: after}
}

func cloneAgentModelChange(change *AgentModelChange) *AgentModelChange {
	if change == nil {
		return nil
	}
	return NewAgentModelChange(change.Before, change.After)
}

// agentModelChangeForLiveness is the one projection boundary for this
// runtime-owned fact. An archived row has no live process whose observation can
// still be current, including when consuming a snapshot produced by an older
// daemon that did not clear the field itself.
func agentModelChangeForLiveness(change *AgentModelChange, liveness Liveness) *AgentModelChange {
	if liveness == LiveArchived {
		return nil
	}
	return cloneAgentModelChange(change)
}

func sameAgentModelChange(a, b *AgentModelChange) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Before == b.Before && a.After == b.After
}

// AgentModelChange returns an isolated copy of the active model diagnostic.
func (i *Instance) AgentModelChange() *AgentModelChange {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneAgentModelChange(i.agentModelChange)
}

// SetAgentModelChange replaces the live projection diagnostic and reports
// whether clients need a refreshed snapshot. Invalid transitions normalize to
// nil so malformed wire data fails closed instead of rendering a false alarm.
func (i *Instance) SetAgentModelChange(change *AgentModelChange) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	// Archived rows have no running agent whose observation could be current.
	// Normalize under the same lock as the liveness check so an in-flight poll
	// cannot reinsert the retired runtime's warning after archive commits.
	normalized := agentModelChangeForLiveness(change, i.liveness)
	if sameAgentModelChange(i.agentModelChange, normalized) {
		return false
	}
	i.agentModelChange = normalized
	return true
}

// clearAgentModelChangeLocked retires projection state owned by an agent process
// when that runtime is archived or replaced. Caller holds i.mu.
func (i *Instance) clearAgentModelChangeLocked() {
	i.agentModelChange = nil
}
