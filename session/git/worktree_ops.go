package git

import (
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"os"
	"os/exec"
	"strings"
)

// Setup creates a new worktree for the session
func (g *GitWorktree) Setup() error {
	// Ensure worktrees directory exists early (can be done in parallel with branch check)
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}

	if err := os.MkdirAll(g.worktreeDir, 0755); err != nil {
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

	// Fire-and-forget post-worktree hooks
	RunPostWorktreeHooksAsync(g.repoPath, g.worktreePath)
	return nil
}

// setupFromExistingBranch creates a worktree from an existing branch
func (g *GitWorktree) setupFromExistingBranch() error {
	// Directory already created in Setup(), skip duplicate creation

	// Clean up any existing worktree first
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath) // Ignore error if worktree doesn't exist

	// Create a new worktree from the existing branch
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", g.worktreePath, g.branchName); err != nil {
		return fmt.Errorf("failed to create worktree from branch %s: %w", g.branchName, err)
	}

	return nil
}

// resolveOriginHead tries to resolve the latest commit from origin's default branch.
// It fetches from origin first, then tries origin/HEAD, origin/main, and origin/master.
// Returns the commit SHA if successful, or empty string if no remote ref is available.
func (g *GitWorktree) resolveOriginHead() string {
	// Fetch from origin to ensure we have the latest refs (best-effort)
	_, _ = g.runGitCommand(g.repoPath, "fetch", "origin")

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
	// Clean up any existing worktree first
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath) // Ignore error if worktree doesn't exist

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
	// For external worktrees, don't remove the worktree or delete the branch
	if g.externalWorktree {
		return nil
	}

	var errs []error

	// Check if worktree path exists before attempting removal
	if _, err := os.Stat(g.worktreePath); err == nil {
		// Remove the worktree using git command
		if _, err := g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath); err != nil {
			errs = append(errs, err)
		}
	} else if !os.IsNotExist(err) {
		// Only append error if it's not a "not exists" error
		errs = append(errs, fmt.Errorf("failed to check worktree path: %w", err))
	}

	// Delete the branch using git CLI
	if _, err := g.runGitCommand(g.repoPath, "branch", "-D", g.branchName); err != nil {
		// Only log if it's not a "branch not found" error
		if !strings.Contains(err.Error(), "not found") {
			errs = append(errs, fmt.Errorf("failed to remove branch %s: %w", g.branchName, err))
		}
	}

	// Prune the worktree to clean up any remaining references
	if err := g.Prune(); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// Remove removes the worktree but keeps the branch
func (g *GitWorktree) Remove() error {
	// Remove the worktree using git command
	if _, err := g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath); err != nil {
		return fmt.Errorf("failed to remove worktree: %w", err)
	}

	return nil
}

// Prune removes all working tree administrative files and directories
func (g *GitWorktree) Prune() error {
	if _, err := g.runGitCommand(g.repoPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("failed to prune worktrees: %w", err)
	}
	return nil
}

// CleanupWorktrees removes all worktrees and their associated branches
func CleanupWorktrees() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	repoRoot, err := findGitRepoRoot(cwd)
	if err != nil {
		return fmt.Errorf("failed to find git repo root: %w", err)
	}

	// List all worktrees from the repo
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list worktrees: %w", err)
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
				// Fallback: remove directory manually
				if err := os.RemoveAll(wt.path); err != nil {
					log.ErrorLog.Printf("failed to remove worktree directory %s: %v", wt.path, err)
				}
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
