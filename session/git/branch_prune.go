package git

import (
	"fmt"
	"os"
	"os/exec"
)

// LocalBranchExists reports whether `branch` exists as a local ref in the repo
// at `repoRoot`. A missing repo, a non-git path, or an empty argument all
// report false (there is simply no such branch to act on).
func LocalBranchExists(repoRoot, branch string) bool {
	if repoRoot == "" || branch == "" {
		return false
	}
	if _, err := os.Stat(repoRoot); err != nil {
		return false
	}
	check := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return check.Run() == nil
}

// DeleteLocalBranch force-deletes the local branch `branch` in the repo at
// `repoRoot`, but only if it currently exists. It reports whether a branch was
// actually deleted.
//
// It is the SOLE branch-deletion path of the factory reset (`af reset`, #1736):
// reset removes worktree directories via RemoveWorktreesForRepo, which deletes
// no branches, and then deletes branches ONLY through here. The caller
// enumerates exactly the branches AF created for its own sessions — live and
// archived — gated on GitWorktreeData.BranchCreatedByUs, so the user's own
// branches (master/main/their feature branches, and any branch a session merely
// reused) are never touched, even for a session whose worktree was still
// registered with git.
//
// A missing branch is not an error (idempotent: a second `af reset` is a
// clean no-op), and neither is a non-git or missing repo path — those are
// logged-and-skipped by the caller's surrounding cleanup and there is simply
// nothing to prune. `git branch -D` force-deletes regardless of merge state,
// which is intended: AF session branches may be unmerged work the reset is
// deliberately discarding.
func DeleteLocalBranch(repoRoot, branch string) (bool, error) {
	// Only delete a branch that exists, so a missing ref (already pruned by
	// worktree cleanup, or never created) is a silent no-op rather than a
	// `git branch -D` error.
	if !LocalBranchExists(repoRoot, branch) {
		return false, nil
	}
	del := exec.Command("git", "-C", repoRoot, "branch", "-D", branch)
	if out, err := del.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git branch -D %s in %s: %w: %s", branch, repoRoot, err, string(out))
	}
	return true, nil
}
