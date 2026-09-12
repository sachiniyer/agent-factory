package git

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Cleanup removes the worktree and associated branch. It reports whether it
// ESTABLISHED the outcome (see CleanupState) alongside any error: callers that
// go on to delete the session's record MUST gate on the state, not on the
// error. If the worktree was not created by agent-factory (externalWorktree),
// only prune is done.
func (g *GitWorktree) Cleanup() (CleanupState, error) {
	return g.cleanup(true)
}

// CleanupRegisteredOnly is Cleanup without the unregistered-directory RemoveAll
// fallback (#3278 review). That fallback is the one deletion git's own
// registration cannot vouch for, and the archived record-free kill must never
// use it: a genuine archive is always registered — both archive move variants
// preserve the registration — so an unregistered occupant of the archived path
// is not provably the archive, and deleting it could destroy an unrelated
// directory that replaced it. Refusing marks the run unknown, so the record is
// retained as the handle for a manual resolution.
func (g *GitWorktree) CleanupRegisteredOnly() (CleanupState, error) {
	return g.cleanup(false)
}

// shouldRemoveWorktreeDir decides whether Cleanup may delete the worktree
// directory itself after `git worktree remove -f` returned removeErr. It is the
// #802/#726 decision tree documented at the call site.
//
// It no longer needs its own timeout guard: the probe runs through r.git, so a
// timed-out probe marks the run unknown and r.removeDir refuses regardless of what
// this returns. That is the point of the run — the safety no longer depends on this
// function remembering anything. It still refuses on an UNKNOWN registration rather
// than falling back to the string gate, so a probe that could not be asked is never
// read as "not ours" (#1917 round 4).
func (r *cleanupRun) shouldRemoveWorktreeDir(removeErr error) bool {
	registered, ok := r.registered()
	if !ok && r.unknown {
		// The probe TIMED OUT. Never act on a verdict we could not obtain, and never
		// re-enter the unbounded delete on a filesystem that just stalled. The run
		// is already unknown, so the record is retained and a retry can finish.
		//
		// This branch is the RUN's, not the rule's — which is why it lives here and
		// the rule itself is the shared function below. Conflating a stall with an
		// error was itself a bug (found reviewing #1917's own diff).
		return false
	}
	return mayDeleteWorktreeDir(registered, ok, removeErr)
}

// requireRegisteredBranchMatch proves a registered worktree at the recorded
// path is THIS session's before either cleanup mode acts on it (#3278/#4342).
// `git worktree remove -f` trusts the registration alone, so a different
// worktree parked at the path would be accepted and deleted, dirty changes
// included. The registration's branch is the session-identifying fact git
// persists across daemon restarts; a mismatch, detached checkout, or failed
// observation refuses in every mode.
//
// requireRegistration adds registered-only cleanup's stronger archive rule: an
// unlisted directory and a listed entry whose occupant no longer carries this
// repo's backpointer also refuse. Ordinary cleanup deliberately retains its
// historical unregistered/corrupted-pointer fallbacks, but never for a path Git
// positively registers on another branch.
func (r *cleanupRun) requireRegisteredBranchMatch(requireRegistration bool) error {
	// -z: NUL-delimited records (#3278 review) — a repository or worktree
	// parent containing a newline would otherwise truncate the listed path
	// and misreport the archive as unlisted, retaining it forever.
	output, err := r.git("worktree", "list", "--porcelain", "-z")
	if err != nil {
		// Ordinary cleanup historically handles a fast, answered Git failure through
		// its #726 validation-error fallback. Preserve that path: only a deadline is
		// unknown. Registered-only archive cleanup has no such fallback and still
		// requires a positive listing answer.
		if !requireRegistration && !r.unknown {
			return nil
		}
		r.unknown = true
		refusal := fmt.Errorf(
			"cannot verify that the registered worktree at %s belongs to this session: %v",
			r.g.worktreePath, err,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	if err := requireCompleteWorktreeListing(output); err != nil {
		// An empty answered listing is the ordinary unregistered-directory case.
		// Registered-only cleanup must instead prove its archive registration.
		if !requireRegistration {
			return nil
		}
		r.unknown = true
		refusal := fmt.Errorf(
			"cannot verify the complete worktree listing for %s: %v", r.g.worktreePath, err,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	branch, listed, parseErr := worktreeListedBranchBounded(output, r.g.worktreePath)
	if parseErr != nil {
		// Normalizing listing entries resolves symlinks, and an UNRELATED
		// entry on a stalled mount must not wedge this kill under its
		// operation lock after r.git's own deadline already ended (#3278
		// review). A timed-out parse is an unknown answer.
		r.unknown = true
		refusal := fmt.Errorf(
			"cannot verify the worktree listing for %s: %v", r.g.worktreePath, parseErr,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	if !listed {
		if !requireRegistration {
			return nil
		}
		r.unknown = true
		refusal := fmt.Errorf(
			"refusing to act on %s: git does not register it as a worktree, so it is not provably this session's archive; leaving it and the record in place",
			r.g.worktreePath,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	// A legacy archive persisted before branches were recorded has no branch
	// to compare (#3278 review); ownership for those rests on the exact
	// occupant/backpointer binding below, which needs no branch — refusing on
	// an impossible "refs/heads/" match would make such rows undeletable.
	if r.g.branchName != "" {
		expected := "refs/heads/" + r.g.branchName
		if branch != expected {
			r.unknown = true
			refusal := fmt.Errorf(
				"refusing to remove %s: git registers it with branch %q, not this session's %q — it is not provably this session's worktree; leaving it and the record in place",
				r.g.worktreePath, branch, expected,
			)
			r.errs = append(r.errs, refusal)
			return refusal
		}
	}
	if !requireRegistration {
		return nil
	}
	// The listing is repo-side metadata and keeps reporting the recorded path
	// and branch after the worktree was moved aside (git merely marks the
	// entry prunable), so it is stale evidence about the OCCUPANT (#3278
	// review). Require the occupant itself to carry a linked-worktree pointer
	// INTO THIS origin's metadata — a genuine foreign repository's worktree
	// parked at the path satisfies the generic shape — before anything
	// destructive, the writer reap included, touches it.
	if err := VerifyRegisteredWorktreeOccupant(r.g.worktreePath, r.g.repoPath); err != nil {
		r.unknown = true
		refusal := fmt.Errorf(
			"refusing to act on %s: its occupant does not carry this worktree's linkage (%v) — the registration is stale evidence about a replaced directory; leaving it and the record in place",
			r.g.worktreePath, err,
		)
		r.errs = append(r.errs, refusal)
		return refusal
	}
	return nil
}

// worktreeListedBranchBounded finds the listing entry for worktreePath and its
// recorded branch without unbounded filesystem work (#3278 review). Symlink
// resolution of ancestors preserves a real directory's leaf name, so entries
// whose basename differs from the target's are skipped with no filesystem
// lookups at all — an unrelated entry on a stalled mount costs nothing. The
// resolutions that do run go through boundedResolveForCompare's per-path
// flight, so a stalled candidate times out once and latches instead of
// stacking one blocked worker per retry.
func worktreeListedBranchBounded(porcelain, worktreePath string) (string, bool, error) {
	targetResolved, err := boundedResolveForCompare(worktreePath)
	if err != nil {
		return "", false, err
	}
	targetBase := filepath.Base(filepath.Clean(worktreePath))
	matched := false
	branch := ""
	// NUL-delimited -z records: attributes are NUL-terminated, so paths may
	// contain newlines without truncating the parse.
	for _, field := range strings.Split(porcelain, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			if matched {
				return branch, true, nil
			}
			branch = ""
			entry := strings.TrimPrefix(field, "worktree ")
			if filepath.Base(filepath.Clean(entry)) != targetBase {
				matched = false
				continue
			}
			resolved, resolveErr := boundedResolveForCompare(entry)
			if resolveErr != nil {
				return "", false, fmt.Errorf(
					"cannot normalize listed worktree %s: %w", entry, resolveErr,
				)
			}
			matched = resolved == targetResolved
		case matched && strings.HasPrefix(field, "branch "):
			branch = strings.TrimPrefix(field, "branch ")
		}
	}
	return branch, matched, nil
}
