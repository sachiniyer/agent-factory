package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/sachiniyer/agent-factory/log"
)

// A saved path and session ID do not identify the directory now occupying that
// path. Reuse registered cleanup's bidirectional gitdir checks; the
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
	err = VerifyRegisteredWorktreeOccupant(worktreePath, repoPath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return worktreeIdentityMismatchf("worktree identity no longer exists at the recorded path: %v", err)
	}
	if err != nil {
		return err
	}
	if branchName != "" && branch != "refs/heads/"+branchName {
		log.InfoLog.Printf("post-worktree hook branch drift in verified worktree %s: registered branch %q, session branch %q; continuing saved hooks", worktreePath, branch, branchName)
	}
	return nil
}

// Only positive mismatch evidence permits a pending journal watcher to stop.
// Timeouts, filesystem I/O errors and all other unknown failures remain retryable.
var errWorktreeIdentityMismatch = errors.New("worktree identity mismatch")

func worktreeIdentityMismatchf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errWorktreeIdentityMismatch, fmt.Sprintf(format, args...))
}
