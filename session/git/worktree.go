package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// isPathStrictlyInside reports whether absBase is a strict descendant of
// absDir (absBase != absDir and absBase is not outside absDir). Both
// arguments must be absolute, cleaned paths.
func isPathStrictlyInside(absBase, absDir string) bool {
	rel, err := filepath.Rel(absDir, absBase)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

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
	// externalWorktree is true if the worktree was not created by agent-factory.
	//
	// Legacy back-compat (#930 PR 3): this was only ever set true by the
	// removed create-on-existing-worktree feature. No code path sets it true
	// anymore, so every newly created worktree has it false. It is still read
	// from storage and honored by Cleanup() (which no-ops for external
	// worktrees) so pre-existing instances created by the old feature do not
	// have their user-owned worktree/branch destroyed on kill. A future PR
	// drops this field once no persisted instance carries it.
	externalWorktree bool
	// branchCreatedByUs is true if this session created the underlying branch
	// itself (via setupNewWorktree). When false, Cleanup() must NOT delete the
	// branch because it pre-existed and likely contains user work.
	branchCreatedByUs bool
	// hooksCtx and hooksCancel control the lifetime of post-worktree hooks.
	// Cancelling hooksCtx stops any in-flight hook commands so they don't
	// outlive the worktree itself.
	hooksCtx    context.Context
	hooksCancel context.CancelFunc
}

// IsExternalWorktree returns true if this worktree was not created by agent-factory
func (g *GitWorktree) IsExternalWorktree() bool {
	return g.externalWorktree
}

// BranchCreatedByUs returns true if this session created the underlying
// branch (rather than reusing a pre-existing one). Cleanup() uses this to
// decide whether it is safe to delete the branch.
func (g *GitWorktree) BranchCreatedByUs() bool {
	return g.branchCreatedByUs
}

// NewGitWorktreeFromStorage restores a GitWorktree from persisted state.
// branchCreatedByUs indicates whether the session originally created the
// branch itself. Existing saved sessions (written before this field was
// persisted) should pass true to preserve prior cleanup behavior.
func NewGitWorktreeFromStorage(repoPath string, worktreePath string, sessionName string, branchName string, baseCommitSHA string, externalWorktree bool, branchCreatedByUs bool) (*GitWorktree, error) {
	if worktreePath == "" {
		return nil, fmt.Errorf("worktree path is empty")
	}
	if repoPath == "" {
		return nil, fmt.Errorf("repo path is empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &GitWorktree{
		repoPath:          repoPath,
		worktreePath:      worktreePath,
		worktreeDir:       filepath.Dir(worktreePath),
		sessionName:       sessionName,
		branchName:        branchName,
		baseCommitSHA:     baseCommitSHA,
		externalWorktree:  externalWorktree,
		branchCreatedByUs: branchCreatedByUs,
		hooksCtx:          ctx,
		hooksCancel:       cancel,
	}, nil
}

// NewGitWorktree creates a new GitWorktree instance
func NewGitWorktree(repoPath string, sessionName string) (tree *GitWorktree, branchname string, err error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load config: %w", err)
	}
	branchName := fmt.Sprintf("%s%s", cfg.BranchPrefix, sessionName)
	// Sanitize the final branch name to handle invalid characters from any source
	// (e.g., backslashes from Windows domain usernames like DOMAIN\user)
	branchName = SanitizeBranchName(branchName)

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

	// Sanitize sessionName for filesystem path to prevent directory traversal
	safeSessionName := strings.ReplaceAll(sessionName, "..", "")
	safeSessionName = strings.ReplaceAll(safeSessionName, "/", "-")
	safeSessionName = strings.TrimLeft(safeSessionName, "-.")
	if safeSessionName == "" {
		safeSessionName = "session"
	}

	basePath := filepath.Join(worktreeDir, repoName+"-"+safeSessionName)

	// Ensure the worktree path is strictly nested inside worktreeDir. We use
	// filepath.Rel instead of a HasPrefix check so the validation is correct
	// when worktreeDir is the filesystem root ("/"): the naive prefix check
	// produces "//" and rejects every valid child path. See #461.
	absBase, _ := filepath.Abs(basePath)
	absDir, _ := filepath.Abs(worktreeDir)
	if !isPathStrictlyInside(absBase, absDir) {
		return nil, "", fmt.Errorf("invalid session name: would create worktree outside expected directory")
	}

	worktreePath := basePath
	for i := 2; ; i++ {
		_, err := os.Stat(worktreePath)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("cannot check worktree path %q: %w", worktreePath, err)
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
