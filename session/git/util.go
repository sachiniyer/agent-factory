package git

import (
	"crypto/rand"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

var (
	reUnsafe    = regexp.MustCompile(`[^a-z0-9\-_/.]+`)
	reMultiDash = regexp.MustCompile(`-+`)
)

// sanitizeBranchName transforms an arbitrary string into a Git branch name friendly string.
// Note: Git branch names have several rules, so this function uses a simple approach
// by allowing only a safe subset of characters.
func sanitizeBranchName(s string) string {
	// Convert to lower-case
	s = strings.ToLower(s)

	// Replace spaces with a dash
	s = strings.ReplaceAll(s, " ", "-")

	// Remove any characters not allowed in our safe subset.
	// Here we allow: letters, digits, dash, underscore, slash, and dot.
	s = reUnsafe.ReplaceAllString(s, "")

	// Replace multiple dashes with a single dash (optional cleanup)
	s = reMultiDash.ReplaceAllString(s, "-")

	// Trim leading and trailing dashes or slashes to avoid issues
	s = strings.Trim(s, "-/")

	// Handle git dot restrictions:
	// 1. Remove leading dots from each path component (no hidden-file-style names like .env)
	//    This also handles ".." since stripping leading dots from ".." leaves it empty.
	parts := strings.Split(s, "/")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimLeft(part, ".")
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	s = strings.Join(filtered, "/")
	// 2. Replace any remaining double dots with a dash (e.g., "a..b")
	s = strings.ReplaceAll(s, "..", "-")
	// 3. No .lock suffix (reserved by git)
	s = strings.TrimSuffix(s, ".lock")
	// 4. No trailing dots
	s = strings.TrimRight(s, ".")
	// 5. Clean up any trailing dashes or slashes left after dot removal
	s = strings.Trim(s, "-/")

	// If the result is empty (e.g., input was only special characters),
	// generate a fallback branch name to prevent worktree creation failures.
	if s == "" {
		s = "session-" + randomHex(4)
	}

	return s
}

// randomHex returns a hex string of n random bytes (2n hex characters).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// IsGitRepo checks if the given path is within a git repository
func IsGitRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	return cmd.Run() == nil
}

func findGitRepoRoot(path string) (string, error) {
	// Use ResolveMainRepoRoot to resolve through linked worktrees so that
	// all worktrees of a repository share the same root path. Without this,
	// running from a linked worktree would return the linked worktree's
	// path, causing new worktrees to be placed in the wrong directory and
	// post-worktree hooks to look up the wrong repo config.
	root, err := config.ResolveMainRepoRoot(path)
	if err != nil {
		return "", fmt.Errorf("failed to find Git repository root from path: %s", path)
	}
	return root, nil
}
