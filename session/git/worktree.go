package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

func getWorktreeDirectoryForRepo(repoPath string) (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	return getWorktreeDirectoryForRepoWithConfig(cfg, repoPath)
}

// getWorktreeDirectoryForRepoWithConfig returns the parent directory for new
// worktrees under the configured placement mode.
func getWorktreeDirectoryForRepoWithConfig(cfg *config.Config, repoPath string) (string, error) {
	if cfg != nil && cfg.WorktreeRoot == config.WorktreeRootSubdirectory {
		configDir, err := config.GetConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(configDir, "worktrees"), nil
	}

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
	// Set true by two producers: instances persisted by the pre-#930-PR-3
	// create-on-existing-worktree feature (legacy records restored via
	// NewGitWorktreeFromStorage), and in-place sessions created with
	// `af sessions create --here` (NewGitWorktreeInPlace), which attach to the
	// repo's own working tree. Either way the worktree and branch are
	// user-owned: Setup() must not create anything and Cleanup() must not
	// remove the worktree or delete the branch.
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
	// hooksDone is closed when the most recent post-worktree hook run finishes
	// (completion or cancellation). Set by Setup/Rebuild* right before they
	// return, read by the readiness wait after Start returns — a strict
	// happens-before, so it is accessed lock-free like branchCreatedByUs. Nil
	// until the first hook run is launched (e.g. external worktrees that skip
	// hooks entirely), which HooksDone reports as "no hooks in flight".
	hooksDone <-chan struct{}
}

// HooksDone returns a channel that is closed once the worktree's post-worktree
// hooks have finished (completion or cancellation), or nil if no hook run has
// been launched for this worktree. Callers treat nil as "nothing in flight".
func (g *GitWorktree) HooksDone() <-chan struct{} {
	return g.hooksDone
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

	worktreeDir, err := getWorktreeDirectoryForRepoWithConfig(cfg, repoPath)
	if err != nil {
		return nil, "", err
	}

	worktreePath, err := resolveWorktreePlacement(cfg, repoPath, worktreeDir, sessionName, branchName)
	if err != nil {
		return nil, "", err
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

// resolveWorktreePlacement computes a session's worktree path under the
// configured placement mode — the single source of truth NewGitWorktree (create)
// and RestoreWorktreePath (archive restore, #1540) share, so both honor
// worktree_root identically. Subdirectory mode nests the branch name under
// $AF_HOME/worktrees; sibling mode nests {repoName}-{safeSessionName} beside the
// repo. The base path is validated to sit strictly inside worktreeDir (#461) and
// a numeric suffix is appended when it is already occupied.
func resolveWorktreePlacement(cfg *config.Config, repoRoot, worktreeDir, sessionName, branchName string) (string, error) {
	var basePath string
	if cfg != nil && cfg.WorktreeRoot == config.WorktreeRootSubdirectory {
		// Subdirectory mode nests the branch name under the worktrees root. A
		// record with no persisted branch (an old/edge archive) would otherwise
		// resolve to worktreeDir itself, fail the strict-inside guard below, and
		// block a restore the title-based path could still complete — so fall back
		// to the sanitized session name as the leaf. A freshly created session
		// always has a branch, so this fallback only ever engages on restore.
		leaf := branchName
		if strings.TrimSpace(leaf) == "" {
			leaf = sanitizeWorktreePathSegment(sessionName)
		}
		basePath = filepath.Join(worktreeDir, leaf)
	} else {
		// Sibling mode: {repoParent}/{repoName}-{sessionName}
		basePath = filepath.Join(worktreeDir, filepath.Base(repoRoot)+"-"+sanitizeWorktreePathSegment(sessionName))
	}

	// Ensure the worktree path is strictly nested inside worktreeDir. We use
	// IsStrictlyInside instead of a HasPrefix check so the validation is correct
	// when worktreeDir is the filesystem root ("/"): the naive prefix check
	// produces "//" and rejects every valid child path. See #461.
	absBase, _ := filepath.Abs(basePath)
	absDir, _ := filepath.Abs(worktreeDir)
	if !pathutil.IsStrictlyInside(absBase, absDir) {
		return "", fmt.Errorf("invalid session name %q: would place worktree outside %s", sessionName, worktreeDir)
	}
	return firstFreeWorktreePath(basePath)
}

// firstFreeWorktreePath returns basePath, or basePath with the lowest "-N"
// (N>=2) suffix that does not yet exist on disk — the collision discipline both
// worktree creation and archive restore use so a session never clobbers an
// occupied path.
func firstFreeWorktreePath(basePath string) (string, error) {
	p := basePath
	for i := 2; ; i++ {
		_, err := os.Stat(p)
		if os.IsNotExist(err) {
			return p, nil
		}
		if err != nil {
			return "", fmt.Errorf("cannot check worktree path %q: %w", p, err)
		}
		p = fmt.Sprintf("%s-%d", basePath, i)
	}
}

// NewGitWorktreeInPlace creates a GitWorktree attached to the repo's own
// working tree at its current branch — the `af sessions create --here` path,
// reinstating (as an explicit opt-in) the create side of the external-worktree
// capability removed in #930 PR 3. The worktree path IS the resolved repo
// root, no branch or worktree is created, and externalWorktree=true /
// branchCreatedByUs=false so Setup() and Cleanup() never touch the user's
// working tree or branch. Returns the worktree and the current branch name.
func NewGitWorktreeInPlace(repoPath string) (*GitWorktree, string, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		log.ErrorLog.Printf("git worktree path abs error, falling back to repoPath %s: %s", repoPath, err)
		absPath = repoPath
	}

	repoRoot, err := findGitRepoRoot(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("an in-place session must be created inside a git repository: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      repoRoot,
		worktreeDir:       filepath.Dir(repoRoot),
		branchName:        "",
		externalWorktree:  true,
		branchCreatedByUs: false,
		hooksCtx:          ctx,
		hooksCancel:       cancel,
	}

	// Record the repo's current branch verbatim ("HEAD" when detached): the
	// session runs on whatever is checked out, and since Cleanup() never
	// deletes it the name is purely informational (sidebar, PR lookup).
	branchName, err := g.runGitCommand(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		cancel()
		if strings.Contains(err.Error(), "ambiguous argument 'HEAD'") ||
			strings.Contains(err.Error(), "not a valid object name") {
			return nil, "", fmt.Errorf("this appears to be a brand new repository: please create an initial commit before creating an in-place session")
		}
		return nil, "", fmt.Errorf("failed to resolve current branch for in-place session: %w", err)
	}
	g.branchName = strings.TrimSpace(branchName)

	// The base commit is the current HEAD: diffs for an in-place session show
	// what the agent changed on top of where the user already was.
	head, err := g.runGitCommand(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		cancel()
		return nil, "", fmt.Errorf("failed to resolve HEAD for in-place session: %w", err)
	}
	g.baseCommitSHA = strings.TrimSpace(head)

	return g, g.branchName, nil
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
