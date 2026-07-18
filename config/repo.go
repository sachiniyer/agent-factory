package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// repoIDPattern restricts a repoID to characters that are safe to use as a
// single path segment. Legitimate IDs from RepoIDFromRoot are 12 lowercase
// hex characters; tests and any future ID schemes are constrained to the
// same character class so the value can never escape its parent directory.
var repoIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// maxRepoIDLength caps the size of an accepted repoID. Legitimate IDs are
// 12 chars; the cap is loose enough to accommodate future schemes while
// preventing unbounded allocation in path joins or error messages.
const maxRepoIDLength = 128

// ValidateRepoID enforces the shape of a repository identifier before it is
// used to construct a filesystem path. Returns an error when the id is
// empty, exceeds maxRepoIDLength, or contains any character outside
// [a-zA-Z0-9_-] — in particular, "." (used in traversal), "/", or "\".
// Callers that legitimately accept an empty id as "all repos" must check
// that case before calling this function.
func ValidateRepoID(repoID string) error {
	if repoID == "" {
		return fmt.Errorf("invalid repo id: empty")
	}
	if len(repoID) > maxRepoIDLength {
		return fmt.Errorf("invalid repo id: length %d exceeds maximum %d", len(repoID), maxRepoIDLength)
	}
	if !repoIDPattern.MatchString(repoID) {
		return fmt.Errorf("invalid repo id: must match %s", repoIDPattern.String())
	}
	return nil
}

// resolveMainRepoRoot returns the root of the main working tree, resolving
// through linked worktrees so that worktree sessions get the same repo ID
// as the main repository. pathArgs should be empty for cwd, or []string{"-C", path}.
func resolveMainRepoRoot(pathArgs ...string) (string, error) {
	// Get the toplevel for the current location
	topCmd := exec.Command("git", append(pathArgs, "rev-parse", "--show-toplevel")...)
	topOut, err := topCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git repo root: %w", err)
	}
	toplevel := strings.TrimSpace(string(topOut))

	// Get git-dir and git-common-dir to detect if we're in a linked worktree.
	// In the main repo: git-dir == ".git", git-common-dir == ".git"
	// In a worktree:    git-dir == "<main>/.git/worktrees/<name>",
	//                   git-common-dir == "<main>/.git"
	infoCmd := exec.Command("git", "-C", toplevel, "rev-parse", "--git-dir", "--git-common-dir")
	infoOut, err := infoCmd.Output()
	if err != nil {
		return toplevel, nil // fallback to toplevel
	}
	parts := strings.SplitN(strings.TrimSpace(string(infoOut)), "\n", 2)
	if len(parts) != 2 {
		return toplevel, nil
	}
	gitDir := parts[0]
	commonDir := parts[1]

	// If they're equal, we're in the main working tree
	if gitDir == commonDir {
		return toplevel, nil
	}

	// Resolve commonDir to an absolute path
	if !filepath.IsAbs(commonDir) {
		if filepath.IsAbs(gitDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		} else {
			commonDir = filepath.Join(toplevel, gitDir, commonDir)
		}
	}
	commonDir = filepath.Clean(commonDir)

	// commonDir is the main repo's .git directory.
	// For submodules, git stores the worktree path in core.worktree inside the git dir.
	// For regular repos, core.worktree is unset and the parent of .git is the repo root.
	wtCmd := exec.Command("git", "config", "--file", filepath.Join(commonDir, "config"), "core.worktree")
	wtOut, err := wtCmd.Output()
	if err == nil {
		worktree := strings.TrimSpace(string(wtOut))
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(commonDir, worktree)
		}
		return filepath.Clean(worktree), nil
	}
	// Fallback: parent of .git directory (correct for non-submodule repos)
	return filepath.Dir(commonDir), nil
}

// RepoContext identifies a git repository and provides scoped path resolution.
type RepoContext struct {
	Root string // absolute path from git rev-parse --show-toplevel
	ID   string // first 12 hex chars of SHA-256(Root)
}

// CurrentRepo returns the RepoContext for the git repository containing cwd.
// If cwd is inside a linked worktree, this resolves to the main repository.
func CurrentRepo() (*RepoContext, error) {
	root, err := resolveMainRepoRoot()
	if err != nil {
		return nil, err
	}
	return repoContextFromRoot(root), nil
}

// RepoFromPath returns the RepoContext for the git repository at the given path.
// If the path is inside a linked worktree, this resolves to the main repository.
func RepoFromPath(path string) (*RepoContext, error) {
	root, err := resolveMainRepoRoot("-C", path)
	if err != nil {
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, err)
	}
	return repoContextFromRoot(root), nil
}

// ResolveMainRepoRoot returns the root of the main working tree for the
// repository at the given path. If path is inside a linked worktree, this
// resolves back to the main repository root so that all worktrees of the
// same repository share a single identity.
func ResolveMainRepoRoot(path string) (string, error) {
	return resolveMainRepoRoot("-C", path)
}

// RepoIDFromRoot computes a repo ID from an absolute repo root path.
func RepoIDFromRoot(root string) string {
	hash := sha256.Sum256([]byte(root))
	return hex.EncodeToString(hash[:6])
}

func repoContextFromRoot(root string) *RepoContext {
	return &RepoContext{
		Root: root,
		ID:   RepoIDFromRoot(root),
	}
}

// ResolvedProject is a recorded project path resolved to its owning repository.
// Root is "" when nothing could be resolved, which is what makes an invented
// identity distinguishable from a real one.
type ResolvedProject struct {
	ID   string
	Root string
}

// ResolveProjectPath maps a recorded project path to its owning repository.
//
// An EXISTING path — including a subdirectory or a linked worktree — resolves
// through git to the main repo root, which is why identity matching (rather
// than path-string equality) sees a task in its own project no matter which
// directory it was created from.
//
// A path that no longer exists is the hard case. Hashing the stale leaf invents
// an ID that equals nothing: the surviving project's ID is sha256 of ITS root,
// never of a deleted child. That strands the record — hidden from its project
// and unaddressable. So walk up to the nearest ANCESTOR that still resolves: a
// deleted subdirectory of a surviving project resolves back to that project.
//
// The walk answers what git itself would say about the path if it existed, so
// it cannot be more wrong than the path is. When nothing up the chain resolves,
// fall back to the leaf hash — path equality, which at least keeps an orphan
// addressable at its own recorded path — and report Root "" so callers can tell
// this identity is derived rather than real.
//
// This is the ONE path→project-identity mechanism. It backs both the CLI's
// project scoping (api/scope.go, #1893) and the TUI task pane's repo filter
// (task.LoadTasksForRepo, #2098); they were separate rules until the latter's
// raw path equality hid subdirectory-created tasks from their own pane.
func ResolveProjectPath(projectPath string) ResolvedProject {
	if repo, err := RepoFromPath(projectPath); err == nil {
		return ResolvedProject{ID: repo.ID, Root: repo.Root}
	}
	cleaned := filepath.Clean(projectPath)
	// Only walk an ABSOLUTE path. A relative one has no meaning independent of
	// the current directory, so climbing it reaches "." and resolves to whatever
	// repository the caller happens to be standing in — adopting a record into
	// the current project on no evidence at all. That is the same invented
	// identity the leaf hash produced, just harder to spot, so a relative path
	// degrades to path equality instead. No supported writer records one (the
	// CLI stores repo.Root, the TUI an absolute path), so this only guards
	// hand-edited rows.
	if filepath.IsAbs(cleaned) {
		for dir := filepath.Dir(cleaned); ; dir = filepath.Dir(dir) {
			if repo, err := RepoFromPath(dir); err == nil {
				return ResolvedProject{ID: repo.ID, Root: repo.Root}
			}
			if parent := filepath.Dir(dir); parent == dir {
				break // reached the filesystem root
			}
		}
	}
	return ResolvedProject{ID: RepoIDFromRoot(cleaned)}
}
