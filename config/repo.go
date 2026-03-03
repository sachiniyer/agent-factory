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

// RepoContext identifies a git repository and provides scoped path resolution.
type RepoContext struct {
	Root string // absolute path from git rev-parse --show-toplevel
	ID   string // first 12 hex chars of SHA-256(Root)
}

// CurrentRepo returns the RepoContext for the git repository containing cwd.
func CurrentRepo() (*RepoContext, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get git repo root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	return repoContextFromRoot(root), nil
}

// RepoFromPath returns the RepoContext for the git repository at the given path.
func RepoFromPath(path string) (*RepoContext, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get git repo root for %s: %w", path, err)
	}
	root := strings.TrimSpace(string(out))
	return repoContextFromRoot(root), nil
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

// DataDir returns the path ~/.claude-squad/<subdir>/<repoID>/, creating it if necessary.
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
