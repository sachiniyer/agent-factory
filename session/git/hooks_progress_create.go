package git

// BeginHookCreatePersistence protects a new session's journal before its owner
// row exists. The runner and create each hold one reference to the same flock;
// either may finish first without exposing an ownerless launch gap to pruning.
func (g *GitWorktree) BeginHookCreatePersistence() {
	g.hookCreatePending = true
}

func (g *GitWorktree) retainHookProgressForCreate(p *hookProgress) {
	if !g.hookCreatePending || p == nil || !p.retainLease() {
		return
	}
	g.hookCreateRelease = p.releaseLease
}

// SettleHookCreatePersistence releases the create's hold after either the owner
// row commits or the create aborts. It is idempotent for deferred cleanup.
func (g *GitWorktree) SettleHookCreatePersistence() {
	g.hookCreatePending = false
	if g.hookCreateRelease != nil {
		g.hookCreateRelease()
		g.hookCreateRelease = nil
	}
}
