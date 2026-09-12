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
		return fmt.Errorf(
			"cannot verify the registered branch at %s: %w", g.worktreePath, err,
		)
	}
	if err := requireCompleteWorktreeListing(output); err != nil {
		return fmt.Errorf(
			"cannot read the complete worktree registration set for %s: %w", g.worktreePath, err,
		)
	}
	branch, listed, err := worktreeListedBranchBounded(output, g.worktreePath)
	if err != nil {
		return fmt.Errorf(
			"cannot read the worktree registration for %s: %w", g.worktreePath, err,
		)
	}
	if !listed {
		return nil
	}
	expected := "refs/heads/" + g.branchName
	if branch != expected {
		return fmt.Errorf(
			"refusing to remove worktree %s: git registers branch %q there, not the create's expected branch %q",
			g.worktreePath, branch, expected,
		)
	}
	return nil
}
