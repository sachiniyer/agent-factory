package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// worktreeListTimeout bounds the branch-hold probe. The probe runs on the
// session-CREATE path while the daemon holds its manager lock, so an
// unbounded read against a stalled filesystem would not merely slow a create
// down — it would wedge every RPC behind that lock (the #896/#1917 lesson,
// applied before it can bite here).
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var worktreeListTimeout = 10 * time.Second

// WorktreeBranchBinding is one registered worktree's branch observation.
// Path is kept exactly as Git reports it for diagnostics; callers resolving
// identity across symlinked roots must normalize only for comparison. Detached
// is structural because "(detached)" is also a legal branch name.
type WorktreeBranchBinding struct {
	Path     string
	Branch   string
	HeadSHA  string
	Detached bool
}

// BranchesHeldByWorktrees returns every local branch that a registered worktree
// of the repo at repoRoot currently has CHECKED OUT, mapped to every worktree
// path holding it. Branches with no worktree — and worktrees on a detached HEAD
// — are absent. The slice is normally singular, but preserves multiplicity when
// an override or older Git has allowed the same branch into several worktrees.
//
// This is the authority the session-name resolver was missing (#2091). A
// branch's mere existence says nothing about whether AF can use it: AF
// deliberately reuses an existing branch when one matches the derived name
// (setupFromExistingBranch), so `git branch` cannot distinguish "reusable" from
// "unusable". A branch already checked out SOMEWHERE is the unusable case, and
// git refuses `worktree add` on it with "already used by worktree at …". Only
// `git worktree list` knows which those are.
//
// The archived worktrees under the AF home are why this must be a git query and
// not a scan of archived/: archiving RELOCATES a worktree and repairs its
// registration (#2013) rather than removing it, so an archived session keeps its
// branch checked out and stays registered under a path the resolver has no
// reason to know. git knows. Ask git.
//
// An unreadable repo returns an error and a nil map, never an empty one: "I
// could not ask" and "nothing is held" are different answers, and a caller that
// cannot tell them apart would treat a failed probe as a confident all-clear.
func BranchesHeldByWorktrees(repoRoot string) (map[string][]string, error) {
	bindings, err := WorktreeBranchBindings(repoRoot)
	if err != nil {
		return nil, err
	}
	holds := make(map[string][]string)
	for _, binding := range bindings {
		if binding.Branch != "" {
			holds[binding.Branch] = append(holds[binding.Branch], binding.Path)
		}
	}
	return holds, nil
}

// WorktreeBranchBindings reads path, branch, HEAD, and detached state for every
// registered worktree in one bounded Git invocation.
func WorktreeBranchBindings(repoRoot string) ([]WorktreeBranchBinding, error) {
	return WorktreeBranchBindingsContext(context.Background(), repoRoot)
}

// WorktreeBranchBindingsContext is WorktreeBranchBindings with caller
// cancellation for shutdown-aware integrity scans.
func WorktreeBranchBindingsContext(parent context.Context, repoRoot string) ([]WorktreeBranchBinding, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return nil, fmt.Errorf("cannot list worktrees: repo path is empty")
	}
	ctx, cancel := context.WithTimeout(parent, worktreeListTimeout)
	defer cancel()

	// -z: NUL-delimited records (#3524). A newline is legal in a POSIX path and
	// the newline-delimited format prints it verbatim, so a path's own newline is
	// indistinguishable from a record terminator. Same fix as worktreeListed
	// (#3423) and worktreeListedBranchBounded (#3278); no new git floor.
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain", "-z")
	// Keep repository selection bound to -C without changing runtime settings.
	cmd.Env = append(repositoryPathEnvironment(os.Environ()), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	// This admission probe has the same helper-spawn shape as the integrity
	// status/reflog probes. Its deadline must terminate the whole Git command
	// tree, not abandon a hook or fsmonitor process after killing git itself.
	isolateGitCommandTree(cmd)
	// Bound the post-exit wait so a child that inherited the capture pipe cannot
	// hold Output() open past the deadline (#856).
	cmd.WaitDelay = gitWaitDelay

	output, err := cmd.Output()
	terminateGitCommandTree(cmd, err)
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited successfully (a non-zero exit surfaces as an
		// ExitError); only a pipe-holder outlived it, so the output is complete.
		err = nil
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git worktree list in %s timed out after %s: %w", repoRoot, worktreeListTimeout, ctx.Err())
		}
		if detail := commandFailureDetail(err); detail != "" {
			return nil, fmt.Errorf("git worktree list in %s failed: %s (%w)", repoRoot, detail, err)
		}
		return nil, fmt.Errorf("git worktree list in %s failed: %w", repoRoot, err)
	}
	bindings, parseErr := parseWorktreeBranchBindings(string(output))
	if parseErr != nil {
		return nil, fmt.Errorf("git worktree list in %s could not be read: %w", repoRoot, parseErr)
	}
	return bindings, nil
}

// parseWorktreeBranchHolds reads `git worktree list --porcelain -z` output into
// branch -> holding worktree path. Each record is a NUL-terminated
// `worktree <path>` line followed by attribute records and terminated by an
// EMPTY record; a record carries a `branch refs/heads/<name>` only when that
// worktree has a branch checked out (detached and bare worktrees do not).
//
// Paths are taken verbatim to the end of the record, which is what git emits —
// every archived worktree path contains spaces, so any whitespace-splitting
// parse would truncate exactly the paths this exists to report. The same is true
// of NEWLINES, which is why this reads -z and not the newline-delimited form
// (#3524): a path ending in a newline left an empty trailing fragment that read
// as a record separator, so the record's branch line was dropped and a branch
// that WAS checked out came back reported as free.
//
// It returns an ERROR for a listing it could not read, and that is the half that
// generalises. A bare map has no way to say "I could not read that", so a
// partially-parsed listing arrived looking exactly like one that genuinely held
// nothing, and every read failure resolved to the permissive answer. The caller
// already has a deliberate, safe response to an unanswerable probe — log it and
// proceed, letting `git worktree add` refuse loudly if the name really is held
// (#2127) — it simply could never be reached. Nil map with the error, never a
// partial one: a caller that ranges over what arrived would be reading a
// truncated listing as the complete set.
//
// No \r trimming: that only ever made sense for line-delimited output, and under
// -z a trailing \r is part of the path.
func parseWorktreeBranchHolds(porcelain string) (map[string][]string, error) {
	bindings, err := parseWorktreeBranchBindings(porcelain)
	if err != nil {
		return nil, err
	}
	holds := make(map[string][]string)
	for _, binding := range bindings {
		if binding.Branch != "" {
			holds[binding.Branch] = append(holds[binding.Branch], binding.Path)
		}
	}
	return holds, nil
}

func parseWorktreeBranchBindings(porcelain string) ([]WorktreeBranchBinding, error) {
	if err := requireCompleteWorktreeListing(porcelain); err != nil {
		return nil, err
	}
	var bindings []WorktreeBranchBinding
	var current *WorktreeBranchBinding
	finish := func() {
		if current != nil && current.Path != "" {
			bindings = append(bindings, *current)
		}
		current = nil
	}
	// TrimSuffix first so the final terminator does not yield a trailing empty
	// record; the empty records that remain are the real entry separators.
	for _, field := range strings.Split(strings.TrimSuffix(porcelain, "\x00"), "\x00") {
		switch {
		case field == "":
			finish()
		case strings.HasPrefix(field, "worktree "):
			finish()
			current = &WorktreeBranchBinding{Path: strings.TrimPrefix(field, "worktree ")}
		case strings.HasPrefix(field, "HEAD ") && current != nil:
			current.HeadSHA = strings.TrimSpace(strings.TrimPrefix(field, "HEAD "))
		case field == "detached" && current != nil:
			current.Detached = true
		case strings.HasPrefix(field, "branch ") && current != nil:
			branch := strings.TrimPrefix(strings.TrimPrefix(field, "branch "), "refs/heads/")
			if branch != "" {
				current.Branch = branch
			}
		}
	}
	finish()
	return bindings, nil
}
