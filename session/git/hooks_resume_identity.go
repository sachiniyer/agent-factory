package git

import (
	"context"
	"fmt"
)

// A saved path and session ID do not identify the directory now occupying that
// path. Reuse registered cleanup's branch and bidirectional gitdir checks; the
// repo's worktree listing alone can still describe a deleted/replaced checkout.
func verifyHookResumeWorktree(ctx context.Context, repoPath, worktreePath, branchName string) error {
	ctx, cancel := context.WithTimeout(ctx, localGitTimeout)
	defer cancel()
	g := &GitWorktree{}
	output, err := g.runGitCommandContext(ctx, repoPath, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return fmt.Errorf("cannot read worktree registration: %w", err)
	}
	branch, listed, err := worktreeListedBranchBounded(output, worktreePath)
	if err != nil {
		return err
	}
	if !listed {
		return fmt.Errorf("path is not registered in the owning repository")
	}
	if branchName != "" && branch != "refs/heads/"+branchName {
		return fmt.Errorf("registered branch %q does not match session branch %q", branch, branchName)
	}
	return VerifyRegisteredWorktreeOccupant(worktreePath, repoPath)
}
