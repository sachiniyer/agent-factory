package session

// Launch starts (or restores) the agent PROCESS in the workspace Provision
// established (#1592 Phase 1 PR4): it materializes the worktree on disk
// (worktree.Setup on a fresh create), spawns or reconnects the tmux session, and
// brings up the non-agent tabs. It owns the failure-cleanup scope: a fresh
// worktree is removed only when the failure positively proves no runtime began,
// while an unknown post-spawn outcome preserves it; a restore failure releases
// only the attach PTY. worktree.Setup deliberately stays here rather than in
// provision because it is the first on-disk mutation and therefore belongs
// inside this cleanup scope.
func (b *LocalBackend) Launch(i *Instance, firstTimeSetup bool) error {
	return b.launch(i, firstTimeSetup, nil)
}
