package session

// LocalBackend.Kill — split out of backend_local.go, which was already at the
// file-length ceiling, because the #3413 trustLiveGeneration parameter needs
// its own doc comment.

// Kill is best-effort: each cleanup step runs independently and a failure in
// one (e.g. a broken git worktree) only logs a warning rather than aborting
// the rest once destructive admission succeeds. Unknown recovery state is
// returned before pane teardown so the daemon retains the persisted handle.
// See issue #478.
//
// trustLiveGeneration (#3413): true only from a daemon path that holds this
// session's op-lock unbroken for its whole teardown, via
// Instance.KillTrustingOwnLifecycleLock — see there for the roster and what
// licenses it; every other caller passes false. See Backend.Kill for why this
// is a parameter, not a second method.
func (b *LocalBackend) Kill(i *Instance, trustLiveGeneration bool) error {
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
