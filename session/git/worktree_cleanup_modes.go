package git

import "fmt"

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

// shouldRemoveWorktreeDir decides whether Cleanup may delete the worktree
// directory itself after `git worktree remove -f` returned removeErr. It is the
// #802/#726 decision tree documented at the call site.
//
// It no longer needs its own timeout guard: the probe runs through r.git, so a
// timed-out probe marks the run unknown and r.removeDir refuses regardless of what
// this returns. That is the point of the run — the safety no longer depends on this
// function remembering anything. It still refuses on an UNKNOWN registration rather
// than falling back to the string gate, so a probe that could not be asked is never
// read as "not ours" (#1917 round 4).
func (r *cleanupRun) shouldRemoveWorktreeDir(removeErr error) bool {
	registered, ok := r.registered()
	if !ok && r.unknown {
		// The probe TIMED OUT. Never act on a verdict we could not obtain, and never
		// re-enter the unbounded delete on a filesystem that just stalled. The run
		// is already unknown, so the record is retained and a retry can finish.
		//
		// This branch is the RUN's, not the rule's — which is why it lives here and
		// the rule itself is the shared function below. Conflating a stall with an
		// error was itself a bug (found reviewing #1917's own diff).
		return false
	}
	return mayDeleteWorktreeDir(registered, ok, removeErr)
}

// requireRegisteredBranchMatch proves the registered worktree at the recorded
// path is THIS session's before the registered-only mode issues its
// destructive remove (#3278 review). `git worktree remove -f` trusts the
// registration alone, so a different worktree of the same still-present
// origin parked at the archived path would be accepted and deleted, dirty
// changes included. The registration's branch is the session-identifying fact
// git itself maintains; a different branch, a detached checkout, or a probe
// that could not answer all refuse and retain. An unlisted path passes
// through: the remove below fails on its own and the registered-only mode
// already refuses the unregistered fallback.
func (r *cleanupRun) requireRegisteredBranchMatch() error {
	output, err := r.git("worktree", "list", "--porcelain")
	if err != nil {
		r.unknown = true
		refusal := fmt.Errorf(
			"cannot verify that the registered worktree at %s belongs to this session: %v",
			r.g.worktreePath, err,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	branch, listed := worktreeListedBranch(output, r.g.worktreePath)
	if !listed {
		return nil
	}
	expected := "refs/heads/" + r.g.branchName
	if branch != expected {
		r.unknown = true
		refusal := fmt.Errorf(
			"refusing to remove %s: git registers it with branch %q, not this session's %q — it is not provably this session's worktree; leaving it and the record in place",
			r.g.worktreePath, branch, expected,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	return nil
}
