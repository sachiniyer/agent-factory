package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// This file owns worktree REMOVAL and the registration identity it depends on:
// the factory reset's removal path (#1736, #2110), the shared ownership rule
// that decides when a directory is AF's to delete, and the path/porcelain
// primitives both this file and Cleanup() compare registrations with.
//
// Split out of worktree_ops.go for the file-length limit (#1145). Same package,
// same behavior — Cleanup() still calls straight into the helpers below.

// ErrWorktreeStillRegistered marks a worktree removal that did NOT complete:
// git still lists the path as a linked worktree of the repo, so its branch
// cannot be deleted. Callers classify on this to preserve the session record
// (see commands/reset.go) instead of erasing it and orphaning the branch.
var ErrWorktreeStillRegistered = errors.New("worktree is still registered with git")

// RemoveWorktreeDir removes a SINGLE worktree directory that AF created for a
// session in the repo at repoRoot, and prunes the registry. It deletes NO
// branch. It reports whether a directory was actually removed.
//
// This is the factory reset's only worktree-removal path (`af reset`, #1736):
// reset must remove ONLY the worktrees AF created — identified by the paths in
// AF's own session records — and NEVER the user's manually-created linked
// worktrees, so there is deliberately no per-repo bulk pass. It also refuses to
// touch the main worktree: a worktreePath that resolves to repoRoot (an
// external `--here` session's tree) is a no-op. Branch deletion is handled
// separately, gated on BranchCreatedByUs, so this never removes a branch either.
//
// A missing directory is not an error (idempotent: a second reset is a clean
// no-op); the registry is still pruned so a stale entry cannot later block a
// `git branch -D`.
//
// It VERIFIES the outcome rather than inferring it from exit codes (#2110).
// `git worktree prune` REFUSES to prune a locked worktree's metadata and still
// exits 0 — "prune ran" is not "the registration is gone" — so this re-probes
// `git worktree list` afterwards and returns ErrWorktreeStillRegistered when the
// registration survived. Reporting that success left the branch permanently
// undeletable behind a "re-run to finish" message that could never finish.
func RemoveWorktreeDir(repoRoot, worktreePath string) (bool, error) {
	if repoRoot == "" || worktreePath == "" {
		return false, nil
	}
	// Never remove the main repo/working tree (e.g. an external --here session
	// whose worktree path IS the user's repo).
	if filepath.Clean(repoRoot) == filepath.Clean(worktreePath) {
		return false, nil
	}

	// A repo that registers nothing has no worktree list to consult and no stale
	// registration that could block a branch delete — there is no repo left to hold
	// the branch either. Remove AF's orphaned directory and report it, rather than
	// failing the verification below on a probe that cannot run. This is the same
	// skip-and-proceed rule CleanupWorktreesForRepo applies to deleted repo roots,
	// and `af reset` legitimately iterates over records whose repo the user has
	// since deleted, moved, unmounted, or stripped of its .git.
	if repoRegistersNothing(repoRoot) {
		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			return false, nil
		}
		if rmErr := os.RemoveAll(worktreePath); rmErr != nil {
			return false, fmt.Errorf("remove worktree dir %s: %w", worktreePath, rmErr)
		}
		return true, nil
	}

	removed := false
	// A missing directory is not proof the registration is gone — git keeps the
	// admin entry for a locked worktree whose directory was deleted out from under
	// it. Skip the removal, but still run the prune-and-VERIFY below.
	if _, err := os.Stat(worktreePath); err == nil {
		// Reap any process still writing inside the tree before removing it (#2025) —
		// the same race GitWorktree.Cleanup guards: a survivor re-creating files
		// defeats both the git remove and the os.RemoveAll fallback. worktreePath here
		// is always an AF-created session worktree (reset passes AF's own record paths
		// and the main tree is refused above), so the cwd-scoped reap only ever hits
		// this session's processes.
		reapWorktreeWriters(worktreePath)

		// Remove the worktree FIRST (git refuses to delete a branch checked out in a
		// worktree). Fall back to a manual directory removal if git can't (e.g. the
		// worktree was relocated to the archive and is no longer registered).
		if err := exec.Command("git", "-C", repoRoot, "worktree", "remove", "-f", worktreePath).Run(); err != nil {
			log.ErrorLog.Printf("failed to remove worktree %s: %v", worktreePath, err)

			// Ownership check, restored from Cleanup (#2110). The reset's fallback
			// used to delete unconditionally, which is how a LOCKED worktree — a
			// path git still owns — lost its directory while keeping the
			// registration. Same rule, one shared function: only a deregistered
			// worktree (or the #726 corrupted-pointer case) is ours to delete.
			registered, probeErr := worktreeRegisteredIn(repoRoot, worktreePath)
			if !mayDeleteWorktreeDir(registered, probeErr == nil, err) {
				if probeErr != nil {
					// Do not name a lock we never confirmed: the probe is what failed.
					return false, fmt.Errorf("%w: could not determine whether git still owns worktree %s, so it was left in place: %w (git worktree remove: %v)",
						ErrWorktreeStillRegistered, worktreePath, probeErr, err)
				}
				return false, worktreeStillRegisteredError(repoRoot, worktreePath, err)
			}
			if rmErr := os.RemoveAll(worktreePath); rmErr != nil {
				return false, fmt.Errorf("remove worktree dir %s: %w", worktreePath, rmErr)
			}
		}
		removed = true
	}

	// Prune stale metadata so a subsequent `git branch -D` for this session's
	// branch isn't blocked by a lingering worktree registration.
	if err := exec.Command("git", "-C", repoRoot, "worktree", "prune").Run(); err != nil {
		log.ErrorLog.Printf("failed to prune worktrees for %s: %v", repoRoot, err)
	}

	// VERIFY. prune's exit 0 means "I ran", not "the metadata is gone": it
	// silently declines to prune a locked worktree. Ask git what it actually
	// still tracks — the registration, not the exit code, is what blocks the
	// branch delete the caller is about to attempt.
	//
	// A probe that ERRORS is not evidence of success either; report it rather
	// than let the caller drop the record on an answer we never got.
	registered, probeErr := worktreeRegisteredIn(repoRoot, worktreePath)
	if probeErr != nil {
		return removed, fmt.Errorf("%w: could not verify that worktree %s was deregistered: %w",
			ErrWorktreeStillRegistered, worktreePath, probeErr)
	}
	if registered {
		return removed, worktreeStillRegisteredError(repoRoot, worktreePath, nil)
	}
	return removed, nil
}

// repoRegistersNothing reports whether repoRoot definitively cannot hold a
// worktree registration: the directory is gone, or it is no longer a git repo
// because its .git is gone.
//
// Both are DIRECT filesystem observations, deliberately — not inferences from a
// failed git command. "git errored, so there must be no repo" is the reasoning
// this whole change exists to remove, and it would turn a transient failure
// (permissions, an unmounted parent) into a deletion. An error that is not
// "does not exist" therefore leaves the conservative path below to handle it.
//
// The alternative — letting a de-git'd repo fall through to the probe — reads
// worse than it sounds: the probe fails, the worktree is reported as possibly
// still registered, and reset retains the session record. But no re-run could
// ever clear it, so the user is left with a permanent record and advice to
// unlock a worktree that no longer has a repo to be registered with. That is the
// same unactionable dead end #2110 is about, so this case is settled here.
func repoRegistersNothing(repoRoot string) bool {
	if _, err := os.Stat(repoRoot); os.IsNotExist(err) {
		return true
	}
	// .git is a directory in a main worktree and a file in a linked one; Stat
	// accepts either.
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); os.IsNotExist(err) {
		return true
	}
	return false
}

// worktreeRegisteredIn reports whether git still lists worktreePath as a linked
// worktree of the repo at repoRoot. The free-function counterpart of
// GitWorktree.isWorktreeRegistered, for callers (the factory reset) that hold
// only two paths; both read the answer through worktreeListed.
func worktreeRegisteredIn(repoRoot, worktreePath string) (bool, error) {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	return worktreeListed(string(out), worktreePath), nil
}

// worktreeStillRegisteredError builds the ACTIONABLE failure for a worktree git
// would not let go of. The old message ("re-run `af reset` to finish") was a
// lie in two ways: a re-run repeated the identical failure, and the session
// record had already been deleted so a re-run planned nothing at all. Name the
// command that actually unblocks it, and keep the record so the re-run has
// something to revisit (see commands/reset.go).
//
// AF deliberately does not force past the lock itself (`remove -f -f`): a lock
// is a human saying "in use", and overriding it is the caller's call to make.
func worktreeStillRegisteredError(repoRoot, worktreePath string, cause error) error {
	// The recovery is meant to be PASTED, so quote the paths (#1978): an AF
	// worktree path with a space would otherwise become two arguments.
	msg := fmt.Sprintf("%s is still a registered git worktree, so its branch cannot be deleted"+
		" — a locked worktree is the usual cause (`git worktree prune` refuses to prune one and still exits 0)."+
		" Recover with: git -C %s worktree unlock %s && af reset",
		worktreePath, config.ShellQuotePath(repoRoot), config.ShellQuotePath(worktreePath))
	if cause != nil {
		return fmt.Errorf("%w: %s (git worktree remove: %w)", ErrWorktreeStillRegistered, msg, cause)
	}
	return fmt.Errorf("%w: %s", ErrWorktreeStillRegistered, msg)
}

// mayDeleteWorktreeDir is the #802/#726 ownership rule: after a failed
// `git worktree remove -f`, may we delete the directory ourselves?
//
// It is shared by Cleanup (via shouldRemoveWorktreeDir) and by the factory
// reset (RemoveWorktreeDir), which had no ownership check at all before #2110 —
// it deleted unconditionally, which is how a locked worktree lost its directory
// while keeping the registration that blocks its branch delete.
//
//   - Probe answered "not registered": git has let go of the worktree but the
//     directory survived (#802). Ours to remove.
//   - Probe answered "still registered": git owns the path. Only the
//     conservative #726 corrupted-`.git`-pointer gate may delete — a locked
//     worktree, submodules, or a permissions failure all land here and are
//     surfaced instead of deleted.
//   - Probe ANSWERED WITH AN ERROR (a corrupted repo, not a stall): nothing is
//     unknown, so refusing would report a settled cleanup while leaving the
//     directory on disk and the caller would drop the record and orphan it.
//     Fall back to the same conservative string gate (#719/#726).
//
// A probe that could not be asked at all is the CALLER's to handle: never let
// "could not ask" resolve to "not ours" or to "delete it".
func mayDeleteWorktreeDir(registered, probeAnswered bool, removeErr error) bool {
	if probeAnswered && !registered {
		return true
	}
	return strings.Contains(removeErr.Error(), "validation failed")
}

// worktreeListed reports whether `git worktree list --porcelain` output names
// worktreePath. The single scanner behind every registration probe in this
// package — the probes differ only in how they BOUND the git call, never in how
// they read its answer.
func worktreeListed(porcelain, worktreePath string) bool {
	target := normalizeWorktreePath(worktreePath)
	for _, line := range strings.Split(porcelain, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		if normalizeWorktreePath(strings.TrimPrefix(line, "worktree ")) == target {
			return true
		}
	}
	return false
}

// normalizeWorktreePath cleans the path and resolves symlinks so `worktree list`
// output compares equal to a stored path even when one side went through a
// symlinked parent (e.g. /var -> /private/var on macOS). git reports the
// CANONICAL path; AF stores whatever spelling the session was created with, so
// both sides must be canonicalized the same way or the comparison is meaningless.
//
// It resolves through the deepest EXISTING ancestor, which is the difference
// that matters (#2110). The old plain EvalSymlinks gave up on a path whose last
// component had just been deleted — precisely the state every post-removal probe
// runs in — so on a symlinked root neither side got canonicalized and a
// still-registered worktree compared as absent. A probe that could not resolve
// the path answered "not registered" with full confidence, which is the same
// fabricated negative this file's verification exists to prevent, one layer down.
func normalizeWorktreePath(p string) string {
	return pathutil.ResolveForCompare(p)
}
