package git

import (
	"claude-squad/config"
	"claude-squad/log"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func getWorktreeDirectory() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "worktrees"), nil
}

func getWorktreeDirectoryForRepo(repoPath string) (string, error) {
	cfg := config.LoadConfig()
	if cfg.WorktreeRoot == config.WorktreeRootSibling {
		if repoPath == "" {
			return "", fmt.Errorf("repo path is required when worktree_root is %q", config.WorktreeRootSibling)
		}

		repoRoot, err := findGitRepoRoot(repoPath)
		if err != nil {
			return "", err
		}

		repoParent := filepath.Dir(repoRoot)
		return repoParent, nil
	}

	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(configDir, "worktrees"), nil
}

// GitWorktree manages git worktree operations for a session
type GitWorktree struct {
	// Path to the repository
	repoPath string
	// Path to the worktree
	worktreePath string
	// Root directory containing all worktrees for this repo/config mode
	worktreeDir string
	// Name of the session
	sessionName string
	// Branch name for the worktree
	branchName string
	// Base commit hash for the worktree
	baseCommitSHA string
	// externalWorktree is true if the worktree was not created by claude-squad
	externalWorktree bool
}

// WorktreeInfo holds information about an existing git worktree
type WorktreeInfo struct {
	Path   string
	Branch string
}

// IsExternalWorktree returns true if this worktree was not created by claude-squad
func (g *GitWorktree) IsExternalWorktree() bool {
	return g.externalWorktree
}

func NewGitWorktreeFromStorage(repoPath string, worktreePath string, sessionName string, branchName string, baseCommitSHA string, externalWorktree bool) *GitWorktree {
	return &GitWorktree{
		repoPath:         repoPath,
		worktreePath:     worktreePath,
		worktreeDir:      filepath.Dir(worktreePath),
		sessionName:      sessionName,
		branchName:       branchName,
		baseCommitSHA:    baseCommitSHA,
		externalWorktree: externalWorktree,
	}
}

// NewGitWorktree creates a new GitWorktree instance
func NewGitWorktree(repoPath string, sessionName string) (tree *GitWorktree, branchname string, err error) {
	cfg := config.LoadConfig()
	branchName := fmt.Sprintf("%s%s", cfg.BranchPrefix, sessionName)
	// Sanitize the final branch name to handle invalid characters from any source
	// (e.g., backslashes from Windows domain usernames like DOMAIN\user)
	branchName = sanitizeBranchName(branchName)

	// Convert repoPath to absolute path
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		log.ErrorLog.Printf("git worktree path abs error, falling back to repoPath %s: %s", repoPath, err)
		// If we can't get absolute path, use original path as fallback
		absPath = repoPath
	}

	repoPath, err = findGitRepoRoot(absPath)
	if err != nil {
		return nil, "", err
	}

	worktreeDir, err := getWorktreeDirectoryForRepo(repoPath)
	if err != nil {
		return nil, "", err
	}

	// Use sanitized branch name for the worktree directory name
	var worktreePath string
	if cfg.WorktreeRoot == config.WorktreeRootSibling {
		repoName := filepath.Base(repoPath)
		worktreePath = filepath.Join(worktreeDir, repoName+"-"+sessionName)
	} else {
		worktreePath = filepath.Join(worktreeDir, branchName)
	}
	worktreePath = worktreePath + "_" + fmt.Sprintf("%x", time.Now().UnixNano())

	return &GitWorktree{
		repoPath:     repoPath,
		sessionName:  sessionName,
		branchName:   branchName,
		worktreePath: worktreePath,
		worktreeDir:  worktreeDir,
	}, branchName, nil
}

// GetWorktreePath returns the path to the worktree
func (g *GitWorktree) GetWorktreePath() string {
	return g.worktreePath
}

// GetBranchName returns the name of the branch associated with this worktree
func (g *GitWorktree) GetBranchName() string {
	return g.branchName
}

// GetRepoPath returns the path to the repository
func (g *GitWorktree) GetRepoPath() string {
	return g.repoPath
}

// GetRepoName returns the name of the repository (last part of the repoPath).
func (g *GitWorktree) GetRepoName() string {
	return filepath.Base(g.repoPath)
}

// GetBaseCommitSHA returns the base commit SHA for the worktree
func (g *GitWorktree) GetBaseCommitSHA() string {
	return g.baseCommitSHA
}

// NewGitWorktreeFromExistingWorktree creates a GitWorktree that points at an existing worktree
// not created by claude-squad. It determines the baseCommitSHA via git merge-base.
func NewGitWorktreeFromExistingWorktree(repoPath, worktreePath, branchName string) (*GitWorktree, error) {
	// Resolve the repo root
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		absRepo = repoPath
	}
	repoRoot, err := findGitRepoRoot(absRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to find git repo root: %w", err)
	}

	// Get the base commit SHA via merge-base between HEAD and the branch
	cmd := exec.Command("git", "-C", repoRoot, "merge-base", "HEAD", branchName)
	output, err := cmd.Output()
	baseCommitSHA := ""
	if err == nil {
		baseCommitSHA = strings.TrimSpace(string(output))
	} else {
		// Fallback: use HEAD if merge-base fails (e.g. detached HEAD)
		cmd2 := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD")
		out2, err2 := cmd2.Output()
		if err2 == nil {
			baseCommitSHA = strings.TrimSpace(string(out2))
		}
	}

	return &GitWorktree{
		repoPath:         repoRoot,
		worktreePath:     worktreePath,
		worktreeDir:      filepath.Dir(worktreePath),
		branchName:       branchName,
		baseCommitSHA:    baseCommitSHA,
		externalWorktree: true,
	}, nil
}

// ListWorktrees returns all non-main worktrees for the given repo.
func ListWorktrees(repoPath string) ([]WorktreeInfo, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	repoRoot, err := findGitRepoRoot(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find git repo root: %w", err)
	}

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	var worktrees []WorktreeInfo
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
				worktrees = append(worktrees, WorktreeInfo{Path: currentPath, Branch: currentBranch})
			}
			currentPath = ""
			currentBranch = ""
		}
	}
	// Handle last entry if output doesn't end with a blank line
	if currentPath != "" {
		worktrees = append(worktrees, WorktreeInfo{Path: currentPath, Branch: currentBranch})
	}

	// Skip the first entry (the main worktree / repo itself)
	if len(worktrees) > 1 {
		return worktrees[1:], nil
	}
	return nil, nil
}
