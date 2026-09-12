package git

import "fmt"

// requireSetupRemovalOwnership proves that a registered occupant of setup's
// selected path is on the branch this GitWorktree means to install. The path
// resolver is only a point-in-time free-path observation: restore can register
// another session there before Setup reaches its destructive cleanup.
//
// An unlisted path is the ordinary create case. `git worktree remove -f` then
// answers harmlessly that there is nothing to remove, and the following add is
// still the authority on whether the filesystem path is usable. A listed path,
// however, carries an ownership token: only the expected branch authorizes the
// forced removal. Detached and differently branched occupants refuse.
func (g *GitWorktree) requireSetupRemovalOwnership() error {
	output, err := g.runGitLocalCommand(g.repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return g.latchSetupRemovalRefusal(fmt.Errorf(
			"cannot verify the registered branch at %s: %w", g.worktreePath, err,
		))
	}
	if err := requireCompleteWorktreeListing(output); err != nil {
		return g.latchSetupRemovalRefusal(fmt.Errorf(
			"cannot read the complete worktree registration set for %s: %w", g.worktreePath, err,
		))
	}
	branch, listed, err := worktreeListedBranchBounded(output, g.worktreePath)
	if err != nil {
		return g.latchSetupRemovalRefusal(fmt.Errorf(
			"cannot read the worktree registration for %s: %w", g.worktreePath, err,
		))
	}
	if !listed {
		return nil
	}
	expected := "refs/heads/" + g.branchName
	if branch != expected {
		return g.latchSetupRemovalRefusal(fmt.Errorf(
			"refusing to remove worktree %s: git registers branch %q there, not the create's expected branch %q",
			g.worktreePath, branch, expected,
		))
	}
	return nil
}

func (g *GitWorktree) latchSetupRemovalRefusal(err error) error {
	reason := err.Error()
	g.setupRemovalRefusal.Store(&reason)
	return err
}

func (g *GitWorktree) setupRemovalRefusalReason() string {
	if reason := g.setupRemovalRefusal.Load(); reason != nil {
		return *reason
	}
	return ""
}

func (g *GitWorktree) clearSetupRemovalRefusal() {
	g.setupRemovalRefusal.Store(nil)
}

// refuseAfterSetupRemovalFailure carries setup's ownership answer into the
// automatic Cleanup a failed first-time create invokes. Cleanup normally has
// broader authority to remove unregistered leftovers; it must not reinterpret
// this explicit "the registered worktree is not ours" refusal as permission to
// retry against that same path.
func (r *cleanupRun) refuseAfterSetupRemovalFailure() error {
	reason := r.g.setupRemovalRefusalReason()
	if reason == "" {
		return nil
	}
	r.unknown = true
	r.errs = append(r.errs, fmt.Errorf(
		"refusing cleanup of %s after setup could not prove ownership: %s; leaving the worktree and branch intact",
		r.g.worktreePath, reason,
	))
	return r.errs[0]
}
