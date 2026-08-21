package git

import (
	"context"
	"errors"
	"fmt"
)

// Bounding the SETUP path's speculative worktree cleanup (#3424).
//
// THE ASYMMETRY WAS THE BUG. Teardown has run `git worktree remove -f` and
// `git worktree prune` under localGitTimeout since #1917, for a reason that
// document states plainly: that remove recursively unlinks a whole checkout and
// blocks indefinitely on a hung network mount or a D-state process holding a
// file in the tree. Session CREATION ran the identical commands through the
// unbounded runGitCommand — so the repo's own hardening proved the stall is
// real while the create path stayed exposed to it. A user creating a session
// over a stale worktree on a dead mount got a spinner with no deadline, no
// cancellation and no recovery short of killing the daemon; WaitForReady does
// not help, because its clock only starts once LaunchPreparedCreate returns and
// the stall is inside it.
//
// BOUNDING ALONE WOULD NOT HAVE FIXED IT. Every one of these calls discarded its
// error on purpose — a `worktree remove` against a path with no worktree fails,
// and that is the common case (#802 audit). Keeping the discard after adding the
// bound would have moved the hang exactly one line down, into the `worktree add`
// that follows: `add` is deliberately unbounded (it runs the repo's
// post-checkout hooks, i.e. user code with no defensible upper bound), and it
// would have stalled on the very same paths that just stalled the remove. So a
// tripped deadline has to ABORT setup, while every other error keeps its
// historical discard.
//
// Aborting is also what makes the failure clean. Setup stops before `worktree
// add`, so it registers no worktree and creates no branch, and the error
// propagates out of Setup exactly as a failed `worktree add` already does —
// session/backend_local.go's launch turns it into its ordinary create failure,
// whose teardown runs the bounded Cleanup and reports ErrWorkspaceLeftBehind if
// the same stall keeps that cleanup's outcome unknown. So this introduces no new
// outcome shape; it routes a hang into the contract the create path already has.
// What the KILLED command itself finished stays unknown, and the message says so
// rather than guessing — see setupCleanupGit.
//
// A LIMIT WORTH NAMING, inherited from runGitLocalCommand and unchanged here:
// the deadline works by SIGKILLing git's process group, so it only rescues
// stalls where git is actually killable. A process wedged in an uninterruptible
// (D-state) syscall against a dead mount takes SIGKILL as pending and never
// exits, and cmd.Wait blocks in waitpid before it ever consults the deadline —
// see the supervised-wait note in worktree_remove_bounded.go (#3096). Teardown
// has that same residue; this change deliberately matches teardown rather than
// inventing a third exec discipline for the create path.

// ErrWorktreeSetupStalled marks a setup-path worktree cleanup that made no
// progress before its deadline. The sentinel exists so callers can tell a
// workspace whose state af could not establish apart from git ANSWERING "there
// is nothing here to remove" — the latter is the ordinary case and is ignored.
var ErrWorktreeSetupStalled = errors.New("worktree setup cleanup timed out")

// clearStaleWorktreePath runs the speculative cleanup every setup path performs
// before `git worktree add`: drop any worktree still registered at our path,
// then prune stale registrations. Both are bounded by localGitTimeout (#3424).
//
// Unlike Cleanup(), a failure here does NOT fall back to deleting the directory:
// at this point the path has not been established as a session-owned worktree,
// and a path that stays blocked surfaces loudly via the `worktree add` that
// follows (#802 audit).
//
// The prune has to happen BEFORE the add, and before any `branch -D`, for two
// separate reasons:
//   - If the worktree directory was deleted externally (rm -rf, disk cleanup),
//     git still tracks it internally and `worktree add <same-path>` fails with
//     "missing but already registered worktree". Recent git clears that
//     registration on the `worktree remove -f` above, but older git errors ("is
//     not a working tree") and leaves it behind; pruning recovers either way.
//   - If that remove failed for another reason (corrupted .git pointer), git
//     still tracks the worktree, so `branch -D` fails with "branch is checked
//     out" and leaves an orphaned branch blocking `worktree add -b`.
//
// Returns a non-nil error ONLY when a command tripped its deadline; the caller
// must abort setup on it rather than continue into `worktree add`.
func (g *GitWorktree) clearStaleWorktreePath() error {
	if err := g.setupCleanupGit(
		"remove a worktree still registered at that path",
		"worktree", "remove", "-f", g.worktreePath,
	); err != nil {
		return err
	}
	return g.setupCleanupGit(
		"prune stale worktree registrations",
		"worktree", "prune",
	)
}

// setupCleanupGit runs one bounded setup-path cleanup command and decides what
// its failure means. This is the only place on the setup path that makes that
// call, so a command added to clearStaleWorktreePath participates by
// construction rather than by remembering to classify it.
//
// A tripped deadline is the only error that stops setup. Everything else is the
// historical, deliberate discard: git failing because there is no worktree (or
// no branch) at the path IS the common case, and treating it as fatal would
// break every ordinary create.
func (g *GitWorktree) setupCleanupGit(what string, args ...string) error {
	_, err := g.runGitLocalCommand(g.repoPath, args...)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	// The message states only what is KNOWN. "Setup added no worktree" is always
	// true — `worktree add` comes after every one of these steps and the abort is
	// what stops it. What the killed step itself finished is NOT knowable: a
	// SIGKILLed `worktree remove` deregisters before it stalls, and a SIGKILLed
	// `branch -D` may have taken the ref with it. Claiming otherwise would make
	// this a bounded failure that lies about what it committed, which is worse
	// than the hang it replaces (the #3233-#3237 outcome-contract rule).
	return fmt.Errorf("%w: cannot prepare worktree path %s: the setup step that had to %s made no progress "+
		"and was killed at its deadline, so setup stopped before adding a worktree there — what that step "+
		"itself completed before it was killed is unknown; the path is most likely on a stalled filesystem "+
		"(a hung network mount) or held by a process wedged in an uninterruptible read, so free or unmount it "+
		"before retrying (`git -C %s worktree prune` reconciles the registration once the path answers again): %w",
		ErrWorktreeSetupStalled, g.worktreePath, what, g.repoPath, err)
}

// deleteStaleSetupBranch removes a leftover branch of our own name before
// `worktree add -b` recreates it, bounded by localGitTimeout for the same reason
// as the worktree remove above (#3424) — and it is the same class of call
// teardown already bounds and gates as destructive (#1917 round 8).
//
// A missing branch is the common case and stays ignored. A tripped deadline
// aborts: `worktree add -b` would fail on the branch we could not delete
// anyway, and it would stall on the same metadata this command could not write.
func (g *GitWorktree) deleteStaleSetupBranch() error {
	return g.setupCleanupGit("delete a leftover branch named "+g.branchName, "branch", "-D", g.branchName)
}
