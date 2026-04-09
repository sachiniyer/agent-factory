package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// DataDir returns the path ~/.agent-factory/<subdir>/<repoID>/, creating it if necessary.
func (rc *RepoContext) DataDir(subdir string) (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, subdir, rc.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}
	return dir, nil
}
