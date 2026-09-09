package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/sachiniyer/agent-factory/log"
)

// Resume requires the positive random identity recorded in the original
// checkout's Git administrative directory. Registration and bidirectional
// linkage prove that the current occupant belongs to the repository; they
// cannot distinguish a newly created worktree at the same path on their own.
func verifyHookResumeWorktree(ctx context.Context, repoPath, worktreePath, branchName string, expected *hookWorktreeIdentity) error {
	if expected == nil {
		return fmt.Errorf("hook journal has no recorded checkout identity")
	}
	current, err := readHookWorktreeIdentity(worktreePath)
	if err != nil {
		// Missing or unreadable identity is unknown. Recovery may make the path
		// authoritative again, so only a successful, unequal read is mismatch proof.
		return fmt.Errorf("cannot read checkout identity: %w", err)
	}
	if !expected.same(current) {
		return worktreeIdentityMismatchf("a different linked worktree occupies the recorded path")
	}
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

// Positive mismatch evidence refuses authorization to resume in the current
// checkout. It does not prove the saved list complete: the adoption watcher
// remains pending so worktree recovery can restore the original occupant.
// Timeouts, filesystem I/O errors and all other unknown failures are retryable too.
var errWorktreeIdentityMismatch = errors.New("worktree identity mismatch")

func worktreeIdentityMismatchf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errWorktreeIdentityMismatch, fmt.Sprintf(format, args...))
}
