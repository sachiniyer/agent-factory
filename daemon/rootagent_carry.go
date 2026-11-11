package daemon

// A heal reaps the dead root's record — the only pointer to the conversation
// (#2616) and tab roster (#2628) it carries — before its create runs, and that
// create can still fail on a transient after the reap: the in-place admission's
// `git worktree list` is bounded at 10s, the worktree admission wait is
// time-bounded, and the post-reap checkout re-prove reads git (#7088). The next
// pass then found no record to reap and created a blank root. So a heal keeps
// what it carried on the candidate's ensure state until a replacement launches:
// a create that failed after the reap holds it there, the next pass's create
// carries it, and success — or the candidate no longer wanting a root — clears
// it (rootEnsureSucceeded).
//
// It is held in memory only: a daemon restart in that window loses it, as it
// lost the reaped record before.

// rootCarryCheckout is the checkout a carry is held against: one reaped from
// one checkout is never handed to a create on another at the same path.
func rootCarryCheckout(identity *resolvedProjectRoot) string {
	if identity == nil {
		return ""
	}
	return identity.checkoutID
}

// holdRootCarry keeps carried on st for the next pass's create.
func (m *Manager) holdRootCarry(st *rootEnsureState, identity *resolvedProjectRoot, carried reapedRootState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.carry, st.carryCheckout = &carried, rootCarryCheckout(identity)
}

// heldRootCarry is the state a failed heal left on st for this checkout. A
// carry held against another checkout is dropped. It stays held until
// rootEnsureSucceeded clears it, so a pass that returns before its create
// keeps it for the next one.
func (m *Manager) heldRootCarry(st *rootEnsureState, identity *resolvedProjectRoot) (reapedRootState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.carry == nil {
		return reapedRootState{}, false
	}
	if st.carryCheckout != rootCarryCheckout(identity) {
		st.carry, st.carryCheckout = nil, ""
		return reapedRootState{}, false
	}
	return *st.carry, true
}
