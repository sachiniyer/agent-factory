package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// Setup creates a new worktree for the session
func (g *GitWorktree) Setup() error {
	// An external worktree (an in-place `--here` session, or a legacy
	// pre-#930-PR-3 record) IS the user's existing working tree: there is
	// nothing to create, and post-worktree hooks are deliberately skipped —
	// they provision fresh checkouts and must not run unasked inside the
	// user's live tree. Mirrors the Cleanup() no-op below.
	if g.externalWorktree {
		return nil
	}

	// Ensure worktrees directory exists early (can be done in parallel with branch check)
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}

	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}

	// Check if branch exists using git CLI (much faster than go-git PlainOpen)
	_, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName))
	branchExists := err == nil

	var setupErr error
	if branchExists {
		setupErr = g.setupFromExistingBranch()
	} else {
		setupErr = g.setupNewWorktree()
	}
	if setupErr != nil {
		return setupErr
	}

	// Fire-and-forget post-worktree hooks (cancellable via hooksCtx)
	g.hooksDone = RunPostWorktreeHooksAsync(g.hooksCtx, g.repoPath, g.worktreePath)
	return nil
}

// RebuildFromExistingBranch recreates this session worktree at its persisted
// path using its persisted branch. It is the Lost-recovery path for a vanished
// worktree whose branch survived: unlike Setup, it must never create a fresh
// branch when the recorded branch is gone.
func (g *GitWorktree) RebuildFromExistingBranch() error {
	if g.externalWorktree {
		return fmt.Errorf("cannot rebuild external worktree %s", g.worktreePath)
	}
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}
	if strings.TrimSpace(g.branchName) == "" {
		return fmt.Errorf("cannot rebuild worktree %s: branch name is empty", g.worktreePath)
	}
	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName)); err != nil {
		return fmt.Errorf("cannot rebuild worktree %s: branch %s is unavailable: %w", g.worktreePath, g.branchName, err)
	}

	branchCreatedByUs := g.branchCreatedByUs
	if err := g.setupFromExistingBranch(); err != nil {
		g.branchCreatedByUs = branchCreatedByUs
		return err
	}
	g.branchCreatedByUs = branchCreatedByUs

	g.hooksDone = RunPostWorktreeHooksAsync(g.hooksCtx, g.repoPath, g.worktreePath)
	return nil
}

// RebuildFreshFromRecordedBase recreates a vanished session worktree when both
// the directory and branch are gone. It creates a new branch with the persisted
// name from the recorded base commit when possible, falling back to origin's
// default branch and then HEAD. The caller must only use this when it can resume
// the agent's exact recorded conversation; otherwise this would be a fresh
// redispatch into an empty worktree.
func (g *GitWorktree) RebuildFreshFromRecordedBase() error {
	if g.externalWorktree {
		return fmt.Errorf("cannot rebuild external worktree %s", g.worktreePath)
	}
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}
	if strings.TrimSpace(g.branchName) == "" {
		return fmt.Errorf("cannot rebuild worktree %s: branch name is empty", g.worktreePath)
	}
	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName)); err == nil {
		return fmt.Errorf("cannot fresh rebuild worktree %s: branch %s already exists", g.worktreePath, g.branchName)
	}

	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	baseCommit, err := g.rebuildBaseCommit()
	if err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", "-b", g.branchName, g.worktreePath, baseCommit); err != nil {
		return fmt.Errorf("failed to create fresh worktree from commit %s: %w", baseCommit, err)
	}

	g.baseCommitSHA = baseCommit
	g.branchCreatedByUs = true
	g.hooksDone = RunPostWorktreeHooksAsync(g.hooksCtx, g.repoPath, g.worktreePath)
	return nil
}

func (g *GitWorktree) rebuildBaseCommit() (string, error) {
	if recorded := strings.TrimSpace(g.baseCommitSHA); recorded != "" {
		if output, err := g.runGitCommand(g.repoPath, "rev-parse", "--verify", recorded+"^{commit}"); err == nil {
			return strings.TrimSpace(output), nil
		}
		log.WarningLog.Printf("recorded base commit %s for worktree %s is unavailable; falling back to origin default/HEAD", recorded, g.worktreePath)
	}
	if baseCommit := g.resolveOriginHead(); baseCommit != "" {
		return baseCommit, nil
	}
	output, err := g.runGitCommand(g.repoPath, "rev-parse", "HEAD")
	if err != nil {
		if strings.Contains(err.Error(), "fatal: ambiguous argument 'HEAD'") ||
			strings.Contains(err.Error(), "fatal: not a valid object name") ||
			strings.Contains(err.Error(), "fatal: HEAD: not a valid object name") {
			return "", fmt.Errorf("this appears to be a brand new repository: please create an initial commit before restoring an instance")
		}
		return "", fmt.Errorf("failed to get HEAD commit hash: %w", err)
	}
	log.InfoLog.Printf("no recorded base/origin remote found, falling back to HEAD for recovered worktree")
	return strings.TrimSpace(output), nil
}

// setupFromExistingBranch creates a worktree from an existing branch
func (g *GitWorktree) setupFromExistingBranch() error {
	// Directory already created in Setup(), skip duplicate creation

	// We are reusing a pre-existing branch — Cleanup() must not delete it.
	g.branchCreatedByUs = false

	// Clean up any existing worktree first. Ignore the error (the worktree
	// usually doesn't exist) and, unlike Cleanup(), do NOT fall back to
	// deleting the directory: at this point the path has not been
	// established as a session-owned worktree, and a path that stays
	// blocked surfaces loudly via the `worktree add` below (#802 audit).
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)

	// Prune stale worktree metadata BEFORE re-adding. If the worktree
	// directory was deleted externally (rm -rf, disk cleanup, etc.), git
	// still tracks it internally and `worktree add <same-path>` fails with
	// "missing but already registered worktree". Recent git clears that
	// registration on the `worktree remove -f` above, but older git errors
	// ("is not a working tree") and leaves it behind; pruning here recovers
	// either way. Mirrors the prune-before-add ordering in setupNewWorktree.
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	// Create a new worktree from the existing branch
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", g.worktreePath, g.branchName); err != nil {
		return fmt.Errorf("failed to create worktree from branch %s: %w", g.branchName, err)
	}

	// Resolve the base commit SHA so diffs and other operations have a reference point.
	// Try merge-base between the branch and origin's default branch first, then fall back to HEAD.
	baseRef := g.resolveOriginHead()
	if baseRef == "" {
		baseRef = "HEAD"
	}
	output, err := g.runGitCommand(g.repoPath, "merge-base", baseRef, g.branchName)
	if err == nil {
		g.baseCommitSHA = strings.TrimSpace(output)
	} else {
		// Fallback: use the branch's own HEAD as the base commit
		output, err = g.runGitCommand(g.worktreePath, "rev-parse", "HEAD")
		if err == nil {
			g.baseCommitSHA = strings.TrimSpace(output)
		}
	}

	return nil
}

// resolveOriginHead tries to resolve the latest commit from origin's default branch.
// It fetches from origin first, then tries origin/HEAD, origin/main, and origin/master.
// Returns the commit SHA if successful, or empty string if no remote ref is available.
func (g *GitWorktree) resolveOriginHead() string {
	// Fetch from origin to ensure we have the latest refs (best-effort). This
	// is the one network call on the session-creation path, so it is bounded
	// by networkGitTimeout: a stalled remote must not hang creation forever
	// (#896). The error is intentionally ignored — on timeout or failure we
	// fall through to whatever origin refs are already cached locally.
	_, _ = g.runGitNetworkCommand(g.repoPath, "fetch", "origin")

	// Try origin/HEAD (symbolic ref pointing to the default branch)
	for _, ref := range []string{"origin/HEAD", "origin/main", "origin/master"} {
		output, err := g.runGitCommand(g.repoPath, "rev-parse", ref)
		if err == nil {
			return strings.TrimSpace(string(output))
		}
	}
	return ""
}

// setupNewWorktree creates a new worktree from origin's default branch (or HEAD as fallback)
func (g *GitWorktree) setupNewWorktree() error {
	// We are creating the branch ourselves — Cleanup() may delete it.
	g.branchCreatedByUs = true

	// Clean up any existing worktree first. Ignore the error (the worktree
	// usually doesn't exist) and, unlike Cleanup(), do NOT fall back to
	// deleting the directory: at this point the path has not been
	// established as a session-owned worktree, and a path that stays
	// blocked surfaces loudly via the `worktree add` below (#802 audit).
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)

	// Prune stale worktree metadata BEFORE deleting the branch. If `worktree
	// remove -f` above failed (corrupted .git pointer, etc.), git still tracks
	// the worktree internally and `branch -D` will fail with "branch is
	// checked out", leaving the orphaned branch behind and blocking
	// `worktree add -b` below.
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	// Clean up any existing branch using git CLI (much faster than go-git PlainOpen)
	_, _ = g.runGitCommand(g.repoPath, "branch", "-D", g.branchName) // Ignore error if branch doesn't exist

	// Try to base the new branch off origin's default branch for a fresh starting point.
	// Fall back to HEAD if no remote is available.
	baseCommit := g.resolveOriginHead()
	if baseCommit == "" {
		output, err := g.runGitCommand(g.repoPath, "rev-parse", "HEAD")
		if err != nil {
			if strings.Contains(err.Error(), "fatal: ambiguous argument 'HEAD'") ||
				strings.Contains(err.Error(), "fatal: not a valid object name") ||
				strings.Contains(err.Error(), "fatal: HEAD: not a valid object name") {
				return fmt.Errorf("this appears to be a brand new repository: please create an initial commit before creating an instance")
			}
			return fmt.Errorf("failed to get HEAD commit hash: %w", err)
		}
		baseCommit = strings.TrimSpace(string(output))
		log.InfoLog.Printf("no origin remote found, falling back to HEAD for new worktree")
	}
	g.baseCommitSHA = baseCommit

	// Create a new worktree from the base commit.
	// This starts the worktree with a clean slate without inheriting uncommitted changes.
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", "-b", g.branchName, g.worktreePath, baseCommit); err != nil {
		return fmt.Errorf("failed to create worktree from commit %s: %w", baseCommit, err)
	}

	return nil
}

// Cleanup removes the worktree and associated branch.
// If the worktree was not created by agent-factory (externalWorktree), only prune is done.
func (g *GitWorktree) Cleanup() error {
	// Cancel any in-flight post-worktree hooks before removing the worktree.
	if g.hooksCancel != nil {
		g.hooksCancel()
	}

	// For external worktrees, don't remove the worktree or delete the branch
	if g.externalWorktree {
		return nil
	}

	// Guard against empty paths that would cause git commands to fail or
	// operate on unintended directories.
	if g.repoPath == "" {
		return fmt.Errorf("cannot clean up worktree: repo path is empty")
	}
	if g.worktreePath == "" {
		return fmt.Errorf("cannot clean up worktree: worktree path is empty")
	}

	var errs []error

	// Check if worktree path exists before attempting removal
	if _, err := os.Stat(g.worktreePath); err == nil {
		// Remove the worktree using git command. Bounded by localGitTimeout
		// (#1917): this recursive delete is the one local git command that
		// genuinely stalls forever (hung mount, D-state process in the tree), and
		// Cleanup runs inside the daemon's kills-in-flight guard.
		if _, err := g.runGitLocalCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath); err != nil {
			log.ErrorLog.Printf("failed to remove worktree %s: %v", g.worktreePath, err)
			// A failed `git worktree remove -f` may still have released the
			// registration. Decide whether the directory is ours to delete
			// by asking git, not by matching error strings (#802):
			//
			//   - Path no longer in `git worktree list`: git has let go of
			//     the worktree but the directory survived. Observed when the
			//     recursive delete aborts partway ("failed to delete ...:
			//     Directory not empty") because the dying agent process wrote
			//     into the tree mid-removal — git deregisters first, then
			//     fails to finish deleting (#802). RemoveAll the leftovers;
			//     the Prune() below reconciles any remaining metadata.
			//   - Still registered + "validation failed": the worktree's
			//     `.git` pointer is corrupted (#719/#726). git refuses to
			//     remove it, but it is unambiguously one of our registered
			//     worktrees, so deleting the directory is safe.
			//   - Still registered + any other error (locked worktree,
			//     submodules, permissions): git owns the path and we don't
			//     know why removal failed — surface the error instead of
			//     deleting data (preserves the best-effort Kill behavior of
			//     #478).
			//   - Timed out: never ours to delete (#1917, see the helper).
			if g.shouldRemoveWorktreeDir(err) {
				if removeErr := os.RemoveAll(g.worktreePath); removeErr != nil {
					errs = append(errs, fmt.Errorf("failed to remove worktree directory %s: %w", g.worktreePath, removeErr))
				}
			} else {
				errs = append(errs, err)
			}
		}
	} else if !os.IsNotExist(err) {
		// Only append error if it's not a "not exists" error
		errs = append(errs, fmt.Errorf("failed to check worktree path: %w", err))
	}

	// Prune stale worktree metadata BEFORE deleting the branch. When the
	// `git worktree remove -f` above fails (e.g. the worktree's `.git`
	// pointer file was removed externally), git still tracks the worktree
	// internally and `git branch -D` will fail with "branch is checked
	// out", leaving an orphaned branch behind. Mirrors the ordering in
	// CleanupWorktreesForRepo (#330). Best-effort: a prune failure here
	// should not block the branch-delete attempt.
	if err := g.Prune(); err != nil {
		errs = append(errs, err)
	}

	// Only delete the branch if this session actually created it. When we
	// reused a pre-existing branch via setupFromExistingBranch(), the branch
	// may contain unrelated user work and must be preserved.
	if g.branchCreatedByUs {
		// Bounded (#1917): teardown path — see runGitLocalCommand.
		if _, err := g.runGitLocalCommand(g.repoPath, "branch", "-D", g.branchName); err != nil {
			// Only log if it's not a "branch not found" error
			if !strings.Contains(err.Error(), "not found") {
				errs = append(errs, fmt.Errorf("failed to remove branch %s: %w", g.branchName, err))
			}
		}
	}

	// Final prune to clean up any remaining references. Usually a no-op
	// after the prune above, but mirrors CleanupWorktreesForRepo.
	if err := g.Prune(); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// shouldRemoveWorktreeDir decides whether Cleanup may delete the worktree
// directory itself after `git worktree remove -f` returned removeErr. It is the
// #802/#726 decision tree documented at the call site, plus the #1917 timeout
// guard below.
func (g *GitWorktree) shouldRemoveWorktreeDir(removeErr error) bool {
	if errors.Is(removeErr, context.DeadlineExceeded) {
		// The removal did not FAIL, it never FINISHED: git was SIGKILLed
		// mid-delete on localGitTimeout. Two reasons that is not a verdict we
		// may act on. First, the registration state is a snapshot of a half-done
		// operation rather than git's considered answer. Second — and decisive —
		// whatever stalled git (a hung mount, a D-state process holding a file in
		// the tree) will stall os.RemoveAll on the very same paths, and
		// os.RemoveAll takes no context, so it would hang FOREVER and defeat the
		// bound we just added. This is tmuxTimeoutContext's rule one layer up:
		// after a tripped deadline, never re-probe the thing that just wedged.
		// Surfacing the timeout leaves the directory for a later kill to retry,
		// which is the recoverable outcome.
		return false
	}
	if registered, listErr := g.isWorktreeRegistered(); listErr == nil && !registered {
		return true
	}
	// Also the path taken when `worktree list` itself failed (listErr != nil):
	// without a readable registration we fall back to the conservative #726
	// string gate.
	return strings.Contains(removeErr.Error(), "validation failed")
}

// isWorktreeRegistered reports whether git still lists g.worktreePath as a
// registered worktree of the repo. Used after a failed `git worktree remove`
// to distinguish "git released the worktree but the directory survived"
// (safe to delete manually, #802) from "git still owns the path" (not ours
// to second-guess).
func (g *GitWorktree) isWorktreeRegistered() (bool, error) {
	// Bounded (#1917): this is Cleanup's error-path probe, so it must not be the
	// step that hangs the kill the bounded `worktree remove` above just rescued.
	output, err := g.runGitLocalCommand(g.repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	target := normalizeWorktreePath(g.worktreePath)
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		if normalizeWorktreePath(strings.TrimPrefix(line, "worktree ")) == target {
			return true, nil
		}
	}
	return false, nil
}

// normalizeWorktreePath cleans the path and resolves symlinks (best-effort)
// so `worktree list` output compares equal to a stored path even when one
// side went through a symlinked parent (e.g. /tmp -> /private/tmp on macOS).
func normalizeWorktreePath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// Prune removes all working tree administrative files and directories.
// Bounded by localGitTimeout (#1917): both callers are on Cleanup's teardown path.
func (g *GitWorktree) Prune() error {
	if _, err := g.runGitLocalCommand(g.repoPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("failed to prune worktrees: %w", err)
	}
	return nil
}

// RemoveWorktreeDir removes a SINGLE worktree directory that AF created for a
// session in the repo at repoRoot, and prunes the registry. It deletes NO
// branch. It reports whether a directory was actually removed.
//
// This is the factory reset's only worktree-removal path (`af reset`, #1736):
// reset must remove ONLY the worktrees AF created — identified by the paths in
// AF's own session records — and NEVER the user's manually-created linked
// worktrees, so there is deliberately no per-repo bulk pass. It also refuses to
// touch the main worktree: a worktreePath that resolves to repoRoot (an
// external `--here` session's tree) is a no-op. Branch deletion is handled
// separately, gated on BranchCreatedByUs, so this never removes a branch either.
//
// A missing directory is not an error (idempotent: a second reset is a clean
// no-op); the registry is still pruned so a stale entry cannot later block a
// `git branch -D`.
func RemoveWorktreeDir(repoRoot, worktreePath string) (bool, error) {
	if repoRoot == "" || worktreePath == "" {
		return false, nil
	}
	// Never remove the main repo/working tree (e.g. an external --here session
	// whose worktree path IS the user's repo).
	if filepath.Clean(repoRoot) == filepath.Clean(worktreePath) {
		return false, nil
	}

	// If the directory is already gone, just prune any stale registry entry so a
	// later branch delete isn't blocked, and report nothing removed.
	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		_ = exec.Command("git", "-C", repoRoot, "worktree", "prune").Run()
		return false, nil
	}

	// Remove the worktree FIRST (git refuses to delete a branch checked out in a
	// worktree). Fall back to a manual directory removal if git can't (e.g. the
	// worktree was relocated to the archive and is no longer registered).
	if err := exec.Command("git", "-C", repoRoot, "worktree", "remove", "-f", worktreePath).Run(); err != nil {
		log.ErrorLog.Printf("failed to remove worktree %s: %v", worktreePath, err)
		if rmErr := os.RemoveAll(worktreePath); rmErr != nil {
			return false, fmt.Errorf("remove worktree dir %s: %w", worktreePath, rmErr)
		}
	}

	// Prune stale metadata so a subsequent `git branch -D` for this session's
	// branch isn't blocked by a lingering worktree registration.
	if err := exec.Command("git", "-C", repoRoot, "worktree", "prune").Run(); err != nil {
		log.ErrorLog.Printf("failed to prune worktrees for %s: %v", repoRoot, err)
	}
	return true, nil
}

// CleanupWorktreesForRepo removes all worktrees and their associated branches
// for the given repo root. The main worktree (the repo itself) is preserved.
// The repoRoot must be the main repo path; callers should resolve linked
// worktree paths to the main repo root before invoking this function.
func CleanupWorktreesForRepo(repoRoot string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root is empty")
	}

	// Skip cleanup if the repo path no longer exists on disk. `af reset`
	// iterates over collected repo roots, which may include deleted, moved,
	// or unmounted paths; without this check, `git -C` would fail and abort
	// the entire reset before subsequent repos (and DeleteAllInstances) ran.
	if _, err := os.Stat(repoRoot); os.IsNotExist(err) {
		log.WarningLog.Printf("skipping cleanup for deleted repo: %s", repoRoot)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to access repo path: %w", err)
	}

	// List all worktrees from the repo. If the path exists but is no longer a
	// git repo (e.g. `.git` was removed), `git -C` exits non-zero. Treat that
	// like the missing-directory case above: log and skip, so `af reset` can
	// still clean up other repos and reset storage (issue #370).
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		log.WarningLog.Printf("skipping cleanup for non-git path: %s", repoRoot)
		return nil
	}

	// Parse output to get (worktreePath, branchName) pairs.
	// Each block is separated by a blank line. A worktree may have no branch (detached HEAD).
	type worktreeInfo struct {
		path   string
		branch string // empty if detached HEAD
	}
	var worktrees []worktreeInfo
	currentPath := ""
	currentBranch := ""
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") {
			branchPath := strings.TrimPrefix(line, "branch ")
			currentBranch = strings.TrimPrefix(branchPath, "refs/heads/")
		} else if line == "" {
			if currentPath != "" {
				worktrees = append(worktrees, worktreeInfo{path: currentPath, branch: currentBranch})
			}
			currentPath = ""
			currentBranch = ""
		}
	}
	// Handle last entry if output doesn't end with a blank line
	if currentPath != "" {
		worktrees = append(worktrees, worktreeInfo{path: currentPath, branch: currentBranch})
	}

	// Skip the first entry (the main worktree / repo itself)
	if len(worktrees) > 1 {
		for _, wt := range worktrees[1:] {
			// Remove the worktree FIRST (git refuses to delete a branch checked out in a worktree)
			removeCmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "-f", wt.path)
			if err := removeCmd.Run(); err != nil {
				log.ErrorLog.Printf("failed to remove worktree %s: %v", wt.path, err)
				// Fallback: remove directory manually. Unconditional — no
				// registration re-check needed here, unlike Cleanup(): wt.path
				// was emitted by `git worktree list` moments ago, so git
				// ownership is already established, and `af reset` semantics
				// are "tear everything down" (#802 audit).
				if err := os.RemoveAll(wt.path); err != nil {
					log.ErrorLog.Printf("failed to remove worktree directory %s: %v", wt.path, err)
				}
			}

			// Prune stale worktree metadata (best-effort) BEFORE deleting the
			// branch. When the `git worktree remove -f` above fails and we fall
			// back to os.RemoveAll, git still tracks the worktree internally,
			// causing `git branch -D` to fail with "branch is checked out".
			pruneCmd := exec.Command("git", "-C", repoRoot, "worktree", "prune")
			if err := pruneCmd.Run(); err != nil {
				log.ErrorLog.Printf("failed to prune worktree metadata before deleting branch %s: %v", wt.branch, err)
			}

			// THEN delete the branch
			if wt.branch != "" {
				deleteCmd := exec.Command("git", "-C", repoRoot, "branch", "-D", wt.branch)
				if err := deleteCmd.Run(); err != nil {
					log.ErrorLog.Printf("failed to delete branch %s: %v", wt.branch, err)
				}
			}
		}
	}

	// Prune worktree references
	pruneCmd := exec.Command("git", "-C", repoRoot, "worktree", "prune")
	if _, err := pruneCmd.Output(); err != nil {
		return fmt.Errorf("failed to prune worktrees: %w", err)
	}

	return nil
}
