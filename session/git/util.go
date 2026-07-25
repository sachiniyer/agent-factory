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
	reUnsafe                    = regexp.MustCompile(`[^a-z0-9\-_/.]+`)
	reUnsafeWorktreePathSegment = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	reMultiDash                 = regexp.MustCompile(`-+`)
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
		// Bound the component BEFORE the leading-dot / trailing-".lock" cleanup
		// below. Each component becomes a directory/file under .git/refs/heads (and
		// the worktree's refs), so it is bound by NAME_MAX; a long title creatable
		// via the CLI/API (no TUI 32-char cap) otherwise derived a component over the
		// limit and failed with a cryptic "Unable to create ...: File name too long"
		// (#2528). The subset here is ASCII, so a byte truncation is rune-safe.
		//
		// Truncating FIRST is load-bearing (#2528 review): the cut can land inside —
		// or newly expose a trailing — ".lock", which git reserves on EVERY
		// slash-separated component, not just the last. The cleanup that follows only
		// removes it because it runs AFTER the cut; the whole-ref trimBranchNameEdges
		// below repairs only the final component, so a re-exposed ".lock" on a
		// non-final component would otherwise survive and git would reject the ref.
		if len(part) > maxBranchComponentLen {
			part = part[:maxBranchComponentLen]
		}
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
	// 3. Clean up invalid suffixes and edge separators until stable. Trimming
	// dashes/slashes can reveal a trailing dot or .lock suffix that was not at
	// the end of the string during the earlier component pass (#1476).
	s = trimBranchNameEdges(s)

	// If the result is empty (e.g., input was only special characters),
	// generate a fallback branch name to prevent worktree creation failures.
	if s == "" {
		s = "session-" + randomHex(4)
	}

	return s
}

func trimBranchNameEdges(s string) string {
	for {
		before := s
		s = strings.Trim(s, "-/")
		s = strings.TrimRight(s, ".")
		for strings.HasSuffix(s, ".lock") {
			s = strings.TrimSuffix(s, ".lock")
		}
		if s == before {
			return s
		}
	}
}

// BranchForTitle derives the git branch name a session title would receive,
// applying the same prefix + sanitization the worktree layer uses when it
// actually creates the branch (see worktree.go's "<prefix><title>" ->
// SanitizeBranchName). Exported so both the daemon's authoritative create-time
// validation and the TUI's pre-submit naming check derive branches identically.
func BranchForTitle(branchPrefix, title string) string {
	result := SanitizeBranchName(branchPrefix + title)
	// When the title portion sanitizes away entirely (e.g. a Unicode- or
	// punctuation-only title), a non-empty prefix keeps the combined result
	// non-empty, so SanitizeBranchName's empty-result random fallback never
	// fires and every such title collapses to the sanitized-prefix string.
	// That falsely collides distinct titles like "日本語" and "مرحبا" under a
	// prefix (#1640). Detect the prefix-only outcome and append a random
	// suffix so those titles still get distinct branches, matching the
	// empty-prefix behavior.
	if prefixOnly := SanitizeBranchName(branchPrefix); result == prefixOnly && result != "" {
		result = result + "-" + randomHex(4)
	}
	return result
}

// sanitizeWorktreePathSegment turns a display title into one filesystem path
// segment for AF-owned worktree directories. It preserves ordinary title case
// for readability while removing separators/traversal and replacing whitespace
// or shell-hostile punctuation with dashes.
//
// It does NOT bound the length: the worktree directory component is the JOINED
// "<repoName>-<segment>", so the segment's NAME_MAX allowance depends on the repo
// name and is applied by the caller at the join site (resolveWorktreePlacement),
// where that prefix is known (#2528).
func sanitizeWorktreePathSegment(title string) string {
	s := reUnsafeWorktreePathSegment.ReplaceAllString(title, "-")
	s = strings.ReplaceAll(s, "..", "")
	s = reMultiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		return "session"
	}
	return s
}

// maxBranchComponentLen bounds a single title-derived git ref component. Linux
// NAME_MAX is 255 bytes per path component; 200 is deliberately conservative,
// leaving headroom for git's transient "<ref>.lock" and for the "-N" suffix a
// worktree directory derived from the same name may carry (see worktree.go). A
// branch ref component has no variable prefix eating into it — the branch prefix
// is its own slash component — so this fixed budget is a real bound.
const maxBranchComponentLen = 200

// disambiguationSuffixReserve is the room BoundTitleForDisambiguation keeps free
// within a branch component for a uniquifying suffix ("-2", " (archived 3)", …).
// It comfortably exceeds the longest such suffix (" (archived 10000)").
const disambiguationSuffixReserve = 32

// BoundTitleForDisambiguation trims a base title so that appending a short
// uniquifying suffix derives a DISTINCT branch instead of one that truncation
// makes identical (#2528).
//
// The daemon's title walks (nextAvailableTitleLocked, uniqueArchivedTitleLocked)
// append "-N" / " (archived N)" to a base title and decide availability on the
// DERIVED branch. When the base is long, SanitizeBranchName truncates the suffix
// away, so every rung derives the same branch as the taken base: the walk finds
// nothing free and burns all 10,000 iterations under the manager lock before
// failing. Bounding the base here keeps the suffix inside maxBranchComponentLen so
// distinct suffixes stay injective and the walk converges at a low rung.
//
// Sanitization never lengthens a string (it only lowercases, replaces 1:1, and
// removes), so bounding the base by RUNES to (budget - reserve) guarantees its
// sanitized form leaves room for the suffix, and keeps the returned title valid
// UTF-8.
func BoundTitleForDisambiguation(base string) string {
	limit := maxBranchComponentLen - disambiguationSuffixReserve
	runes := []rune(base)
	if len(runes) <= limit {
		return base
	}
	return string(runes[:limit])
}

// TitlesCollide reports whether two session titles cannot coexist in the same
// repo because they would derive the same git branch. Exact (case-insensitive)
// duplicates always collide (#605); beyond that, titles collide when they
// sanitize to the same branch name, e.g. "A B" and "a-b" -> "af-a-b" (#741).
// The EqualFold guard also covers titles made only of unsafe characters, whose
// sanitized branch is a random fallback that would otherwise never compare
// equal.
//
// This is the single source of truth for title collisions: the daemon calls it
// at create time and the TUI calls it in its naming pre-check, so the two can
// never drift apart again (#936).
func TitlesCollide(a, b, branchPrefix string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	return BranchForTitle(branchPrefix, a) == BranchForTitle(branchPrefix, b)
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

// EnsureGitInstalled returns an actionable error when the git binary is not on
// PATH. Split out of EnsureRepo so a caller that tolerates running outside a
// repository — the registry-mode TUI launch (#2477) — can still require git
// itself, which af's worktrees and sessions are built on.
func EnsureGitInstalled() error {
	if !IsGitInstalled() {
		return fmt.Errorf("git is not installed or could not be found in PATH; install git and ensure it is available in your PATH")
	}
	return nil
}

// EnsureRepo verifies that git is installed and that path is within a git
// repository, returning an actionable error that distinguishes the two failure
// modes. IsGitRepo collapses both into a bare false, which previously produced
// a misleading "must be run from within a git repository" message for users
// who simply did not have git installed (issue #737).
func EnsureRepo(path string) error {
	if err := EnsureGitInstalled(); err != nil {
		return err
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
