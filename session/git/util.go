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

// SanitizeBranchName transforms an arbitrary string into a Git branch name friendly string.
// Note: Git branch names have several rules, so this function uses a simple approach
// by allowing only a safe subset of characters.
//
// It is exported so the daemon can pre-validate that two distinct session titles
// would not derive the same git branch (e.g. "A B" and "a-b" both -> "af-a-b"),
// rejecting the collision before worktree setup fails with a cryptic git error
// (sachiniyer/agent-factory#741, completing #605).
func SanitizeBranchName(s string) string {
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
	// 1. For each path component: strip leading dots (no hidden-file-style names
	//    like .env; also collapses ".." to empty) and strip any trailing ".lock"
	//    suffixes (reserved by git for every path segment, not just the final one).
	parts := strings.Split(s, "/")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimLeft(part, ".")
		for strings.HasSuffix(part, ".lock") {
			part = strings.TrimSuffix(part, ".lock")
		}
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	s = strings.Join(filtered, "/")
	// 2. Replace any remaining double dots with a dash (e.g., "a..b")
	s = strings.ReplaceAll(s, "..", "-")
	// 3. No trailing dots
	s = strings.TrimRight(s, ".")
	// 4. Clean up any trailing dashes or slashes left after dot removal
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

// IsGitInstalled reports whether the git binary is available on PATH.
func IsGitInstalled() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// IsGitRepo checks if the given path is within a git repository.
//
// Note: this returns false both when git is not installed and when the path is
// not inside a repository. Callers that need to tell those cases apart (e.g.
// to surface an actionable startup error) should use EnsureRepo instead.
func IsGitRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	return cmd.Run() == nil
}

// EnsureRepo verifies that git is installed and that path is within a git
// repository, returning an actionable error that distinguishes the two failure
// modes. IsGitRepo collapses both into a bare false, which previously produced
// a misleading "must be run from within a git repository" message for users
// who simply did not have git installed (issue #737).
func EnsureRepo(path string) error {
	if !IsGitInstalled() {
		return fmt.Errorf("git is not installed or could not be found in PATH; install git and ensure it is available in your PATH")
	}
	if !IsGitRepo(path) {
		return fmt.Errorf("agent-factory must be run from within a git repository")
	}
	return nil
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
