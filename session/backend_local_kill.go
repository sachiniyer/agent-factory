package session

// LocalBackend.Kill and its #3413 trusted sibling — split out of
// backend_local.go, which was already at the file-length ceiling, because this
// pair needs its own doc comment explaining who may call which.

// Kill is best-effort: each cleanup step runs independently and a failure in
// one (e.g. a broken git worktree) only logs a warning rather than aborting
// the rest once destructive admission succeeds. Unknown recovery state is
// returned before pane teardown so the daemon retains the persisted handle.
// See issue #478.
func (b *LocalBackend) Kill(i *Instance) error { return b.kill(i, false) }

// KillTrustingOwnLifecycleLock is Kill for a caller holding this instance's
// exclusive lifecycle lock for the whole call (#3413) — see
// tmux.TmuxSession.CloseAndWaitForPaneExitTrustingOwnGeneration for why that
// matters and who may call this.
func (b *LocalBackend) KillTrustingOwnLifecycleLock(i *Instance) error { return b.kill(i, true) }

func (b *LocalBackend) kill(i *Instance, trustLiveGeneration bool) error {
	// The manager already checked the still-missing origin before committing the
	// kill tombstone. prepareKillTeardown is the post-commit guard: it consumes
	// and revalidates the exact directory identity before any pane is touched,
	// without letting a later, separate origin directory revoke the transaction.
	mode, preserveClaim, err := i.prepareKillTeardown(trustLiveGeneration)
	if err != nil {
		return err
	}
	defer preserveClaim()
	// PR 2 of #930 gives an instance N tabs (agent + shell today), so Kill tears
	// down each tab's session, not just the agent's. The kill mode kill-sessions
	// every tab (waiting for each pane to exit before the worktree delete, #802),
	// deletes the worktree, and clears the refs — see teardownTabs. Best-effort:
	// a stuck tmux or a failed worktree cleanup only logs so the caller can still
	// drop the record (#478/#802). Returns nil regardless.
	return i.teardownTabs(mode)
}
