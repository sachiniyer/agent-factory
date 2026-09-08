package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
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
		return worktreeIdentityMismatchf("path is not registered in the owning repository")
	}
	if branchName != "" && branch != "refs/heads/"+branchName {
		return worktreeIdentityMismatchf("registered branch %q does not match session branch %q", branch, branchName)
	}
	err = VerifyRegisteredWorktreeOccupant(worktreePath, repoPath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return worktreeIdentityMismatchf("worktree identity no longer exists at the recorded path: %v", err)
	}
	return err
}

// Only positive mismatch evidence permits a pending journal watcher to stop.
// Timeouts, filesystem I/O errors and all other unknown failures remain retryable.
var errWorktreeIdentityMismatch = errors.New("worktree identity mismatch")

func worktreeIdentityMismatchf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errWorktreeIdentityMismatch, fmt.Sprintf(format, args...))
}
