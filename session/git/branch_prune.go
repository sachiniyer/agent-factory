package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalBranchExists reports whether `branch` exists as a local ref in the repo
// at `repoRoot`.
//
// The answer is DETERMINATE only when err is nil: (true, nil) means the ref
// verifiably exists, (false, nil) means it verifiably does not. A non-nil error
// means existence could not be established — UNKNOWN, not absent (#3243). A
// destructive caller that reads a failed probe as "absent" deletes the cleanup
// record that is its only durable pointer to the branch, so callers must treat
// the error as "leave the branch and its record alone".
//
// Determinate absence covers three observations:
//
//   - empty arguments name no branch to act on;
//   - the repo root does not exist — no repo, no local branch;
//   - the root exists but has no .git — the repo was deleted out from under the
//     record, taking its refs with it. Both are DIRECT filesystem observations,
//     the same rule repoRegistersNothing applies for worktrees (#2110): "git
//     errored, so there must be no repo" is exactly the inference this function
//     exists to remove. Settling the de-git'd case here also keeps `git -C`
//     from discovering an ENCLOSING repo upward of the recorded root and
//     answering — or worse, letting the caller's `git branch -D` act — on a
//     same-named branch AF never created.
//
// Everything else is probed with `git show-ref --verify --quiet`, whose exit
// surface git does keep distinct: exit 0 is "exists", a silent exit 1 is "no
// such ref", and operational failures (unreadable refs storage, corrupt
// metadata) die loudly with another status. Only the first two are answers; the
// rest return the error, stderr included.
func LocalBranchExists(repoRoot, branch string) (bool, error) {
	if repoRoot == "" || branch == "" {
		return false, nil
	}
	if _, err := os.Stat(repoRoot); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat repo %s: %w", repoRoot, err)
	}
	// .git is a directory in a main worktree and a file in a linked one; Stat
	// accepts either.
	gitPath := filepath.Join(repoRoot, ".git")
	if _, err := os.Stat(gitPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", gitPath, err)
	}
	check := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	var stderr bytes.Buffer
	check.Stderr = &stderr
	err := check.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
		return false, nil
	}
	return false, fmt.Errorf("git show-ref refs/heads/%s in %s: %w: %s",
		branch, repoRoot, err, strings.TrimSpace(stderr.String()))
}

// DeleteLocalBranch force-deletes the local branch `branch` in the repo at
// `repoRoot`, but only if it verifiably exists. It reports whether a branch was
// actually deleted.
//
// It is the SOLE branch-deletion path of the factory reset (`af reset`, #1736):
// reset removes worktree directories via RemoveWorktreeDir, which deletes no
// branches, and then deletes branches ONLY through here. The caller
// enumerates exactly the branches AF created for its own sessions — live and
// archived — gated on GitWorktreeData.BranchCreatedByUs, so the user's own
// branches (master/main/their feature branches, and any branch a session merely
// reused) are never touched, even for a session whose worktree was still
// registered with git.
//
// A DETERMINATELY missing branch is not an error (idempotent: a second
// `af reset` is a clean no-op), and neither is a missing or de-git'd repo path —
// there is simply nothing left to prune. But a failed existence probe is
// returned as an error, never read as absence (#3243): the caller holds the
// only durable record naming this branch, and must retain it whenever the
// branch could not be confirmed gone. `git branch -D` force-deletes regardless
// of merge state, which is intended: AF session branches may be unmerged work
// the reset is deliberately discarding.
func DeleteLocalBranch(repoRoot, branch string) (bool, error) {
	exists, err := LocalBranchExists(repoRoot, branch)
	if err != nil {
		return false, fmt.Errorf("cannot establish whether branch %s exists in %s: %w", branch, repoRoot, err)
	}
	if !exists {
		return false, nil
	}
	del := exec.Command("git", "-C", repoRoot, "branch", "-D", branch)
	if out, err := del.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git branch -D %s in %s: %w: %s", branch, repoRoot, err, string(out))
	}
	return true, nil
}
