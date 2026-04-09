package git

import (
	"crypto/rand"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to find Git repository root from path: %s", path)
	}
	return strings.TrimSpace(string(out)), nil
}
