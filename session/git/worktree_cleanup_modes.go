package git

// Cleanup removes the worktree and associated branch. It reports whether it
// ESTABLISHED the outcome (see CleanupState) alongside any error: callers that
// go on to delete the session's record MUST gate on the state, not on the
// error. If the worktree was not created by agent-factory (externalWorktree),
// only prune is done.
func (g *GitWorktree) Cleanup() (CleanupState, error) {
	return g.cleanup(true)
}

// CleanupRegisteredOnly is Cleanup without the unregistered-directory RemoveAll
// fallback (#3278 review). That fallback is the one deletion git's own
// registration cannot vouch for, and the archived record-free kill must never
// use it: a genuine archive is always registered — both archive move variants
// preserve the registration — so an unregistered occupant of the archived path
// is not provably the archive, and deleting it could destroy an unrelated
// directory that replaced it. Refusing marks the run unknown, so the record is
// retained as the handle for a manual resolution.
func (g *GitWorktree) CleanupRegisteredOnly() (CleanupState, error) {
	return g.cleanup(false)
}
