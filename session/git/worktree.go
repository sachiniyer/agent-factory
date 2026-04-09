package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// getWorktreeDirectoryForRepo returns the parent directory of the repo,
// so worktrees are created as siblings next to the repository.
func getWorktreeDirectoryForRepo(repoPath string) (string, error) {
	if repoPath == "" {
		return "", fmt.Errorf("repo path is required for worktree creation")
	}

	repoRoot, err := findGitRepoRoot(repoPath)
	if err != nil {
		return "", err
	}

	return filepath.Dir(repoRoot), nil
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
	// externalWorktree is true if the worktree was not created by agent-factory
	externalWorktree bool
	// hooksCtx and hooksCancel control the lifetime of post-worktree hooks.
	// Cancelling hooksCtx stops any in-flight hook commands so they don't
	// outlive the worktree itself.
	hooksCtx    context.Context
	hooksCancel context.CancelFunc
}

// WorktreeInfo holds information about an existing git worktree
type WorktreeInfo struct {
	Path           string
	Branch         string
	IsMainWorktree bool
}

// IsExternalWorktree returns true if this worktree was not created by agent-factory
func (g *GitWorktree) IsExternalWorktree() bool {
	return g.externalWorktree
}

func NewGitWorktreeFromStorage(repoPath string, worktreePath string, sessionName string, branchName string, baseCommitSHA string, externalWorktree bool) (*GitWorktree, error) {
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree path is empty")
	}
	if repoPath == "" {
		return nil, fmt.Errorf("repo path is empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &GitWorktree{
		repoPath:         repoPath,
		worktreePath:     worktreePath,
		worktreeDir:      filepath.Dir(worktreePath),
		sessionName:      sessionName,
		branchName:       branchName,
		baseCommitSHA:    baseCommitSHA,
		externalWorktree: externalWorktree,
		hooksCtx:         ctx,
		hooksCancel:      cancel,
	}, nil
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

	// Worktree is placed as a sibling: {repoParent}/{repoName}-{sessionName}
	// Only append a numeric suffix if the path already exists (collision).
	repoName := filepath.Base(repoPath)
	basePath := filepath.Join(worktreeDir, repoName+"-"+sessionName)
	worktreePath := basePath
	for i := 2; ; i++ {
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			break
		}
		worktreePath = fmt.Sprintf("%s-%d", basePath, i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &GitWorktree{
		repoPath:     repoPath,
		sessionName:  sessionName,
		branchName:   branchName,
		worktreePath: worktreePath,
		worktreeDir:  worktreeDir,
		hooksCtx:     ctx,
		hooksCancel:  cancel,
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
// not created by agent-factory. It determines the baseCommitSHA via git merge-base.
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

	ctx, cancel := context.WithCancel(context.Background())
	return &GitWorktree{
		repoPath:         repoRoot,
		worktreePath:     worktreePath,
		worktreeDir:      filepath.Dir(worktreePath),
		branchName:       branchName,
		baseCommitSHA:    baseCommitSHA,
		externalWorktree: true,
		hooksCtx:         ctx,
		hooksCancel:      cancel,
	}, nil
}

// ListWorktrees returns all worktrees for the given repo, including the main worktree.
// The main worktree (root tree) is marked with IsMainWorktree=true.
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
	isFirst := true
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") {
			branchPath := strings.TrimPrefix(line, "branch ")
			currentBranch = strings.TrimPrefix(branchPath, "refs/heads/")
		} else if line == "" {
			if currentPath != "" {
				worktrees = append(worktrees, WorktreeInfo{
					Path:           currentPath,
					Branch:         currentBranch,
					IsMainWorktree: isFirst,
				})
				isFirst = false
			}
			currentPath = ""
			currentBranch = ""
		}
	}
	// Handle last entry if output doesn't end with a blank line
	if currentPath != "" {
		worktrees = append(worktrees, WorktreeInfo{
			Path:           currentPath,
			Branch:         currentBranch,
			IsMainWorktree: isFirst,
		})
	}

	if len(worktrees) == 0 {
		return nil, nil
	}
	return worktrees, nil
}
