package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// Setup creates a new worktree for the session
func (g *GitWorktree) Setup() error {
	// An external worktree (an in-place `--here` session, or a legacy
	// pre-#930-PR-3 record) IS the user's existing working tree: there is
	// nothing to create, and post-worktree hooks are deliberately skipped —
	// they provision fresh checkouts and must not run unasked inside the
	// user's live tree. Mirrors the Cleanup() no-op below.
	if g.externalWorktree {
		return nil
	}

	// Ensure worktrees directory exists early (can be done in parallel with branch check)
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}

	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}

	// Check if branch exists using git CLI (much faster than go-git PlainOpen)
	_, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName))
	branchExists := err == nil

	var setupErr error
	if branchExists {
		setupErr = g.setupFromExistingBranch()
	} else {
		setupErr = g.setupNewWorktree()
	}
	if setupErr != nil {
		return setupErr
	}

	// Fire-and-forget post-worktree hooks (cancellable via hooksCtx)
	g.hooksDone = RunPostWorktreeHooksAsyncWithEnvironment(g.hooksCtx, g.repoPath, g.worktreePath,
		g.hookAgent, g.hookEnvPassthrough)
	return nil
}

// RebuildFromExistingBranch recreates this session worktree at its persisted
// path using its persisted branch. It is the Lost-recovery path for a vanished
// worktree whose branch survived: unlike Setup, it must never create a fresh
// branch when the recorded branch is gone.
func (g *GitWorktree) RebuildFromExistingBranch() error {
	if g.externalWorktree {
		return fmt.Errorf("cannot rebuild external worktree %s", g.worktreePath)
	}
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}
	if strings.TrimSpace(g.branchName) == "" {
		return fmt.Errorf("cannot rebuild worktree %s: branch name is empty", g.worktreePath)
	}
	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName)); err != nil {
		return fmt.Errorf("cannot rebuild worktree %s: branch %s is unavailable: %w", g.worktreePath, g.branchName, err)
	}

	branchCreatedByUs := g.branchCreatedByUs
	if err := g.setupFromExistingBranch(); err != nil {
		g.branchCreatedByUs = branchCreatedByUs
		return err
	}
	g.branchCreatedByUs = branchCreatedByUs

	g.hooksDone = RunPostWorktreeHooksAsyncWithEnvironment(g.hooksCtx, g.repoPath, g.worktreePath,
		g.hookAgent, g.hookEnvPassthrough)
	return nil
}

// RebuildFreshFromRecordedBase recreates a vanished session worktree when both
// the directory and branch are gone. It creates a new branch with the persisted
// name from the recorded base commit when possible, falling back to origin's
// default branch and then HEAD. The caller must only use this when it can resume
// the agent's exact recorded conversation; otherwise this would be a fresh
// redispatch into an empty worktree.
func (g *GitWorktree) RebuildFreshFromRecordedBase() error {
	if g.externalWorktree {
		return fmt.Errorf("cannot rebuild external worktree %s", g.worktreePath)
	}
	if g.worktreeDir == "" {
		return fmt.Errorf("failed to get worktree directory: empty worktree directory")
	}
	if strings.TrimSpace(g.branchName) == "" {
		return fmt.Errorf("cannot rebuild worktree %s: branch name is empty", g.worktreePath)
	}
	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0755); err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", fmt.Sprintf("refs/heads/%s", g.branchName)); err == nil {
		return fmt.Errorf("cannot fresh rebuild worktree %s: branch %s already exists", g.worktreePath, g.branchName)
	}

	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	baseCommit, err := g.rebuildBaseCommit()
	if err != nil {
		return err
	}
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", "-b", g.branchName, g.worktreePath, baseCommit); err != nil {
		return fmt.Errorf("failed to create fresh worktree from commit %s: %w", baseCommit, err)
	}

	g.baseCommitSHA = baseCommit
	g.branchCreatedByUs = true
	g.hooksDone = RunPostWorktreeHooksAsyncWithEnvironment(g.hooksCtx, g.repoPath, g.worktreePath,
		g.hookAgent, g.hookEnvPassthrough)
	return nil
}

func (g *GitWorktree) rebuildBaseCommit() (string, error) {
	if recorded := strings.TrimSpace(g.baseCommitSHA); recorded != "" {
		if output, err := g.runGitCommand(g.repoPath, "rev-parse", "--verify", recorded+"^{commit}"); err == nil {
			return strings.TrimSpace(output), nil
		}
		log.WarningLog.Printf("recorded base commit %s for worktree %s is unavailable; falling back to origin default/HEAD", recorded, g.worktreePath)
	}
	if baseCommit := g.resolveOriginHead(); baseCommit != "" {
		return baseCommit, nil
	}
	output, err := g.runGitCommand(g.repoPath, "rev-parse", "HEAD")
	if err != nil {
		if strings.Contains(err.Error(), "fatal: ambiguous argument 'HEAD'") ||
			strings.Contains(err.Error(), "fatal: not a valid object name") ||
			strings.Contains(err.Error(), "fatal: HEAD: not a valid object name") {
			return "", fmt.Errorf("this appears to be a brand new repository: please create an initial commit before restoring an instance")
		}
		return "", fmt.Errorf("failed to get HEAD commit hash: %w", err)
	}
	log.InfoLog.Printf("no recorded base/origin remote found, falling back to HEAD for recovered worktree")
	return strings.TrimSpace(output), nil
}

// setupFromExistingBranch creates a worktree from an existing branch
func (g *GitWorktree) setupFromExistingBranch() error {
	// Directory already created in Setup(), skip duplicate creation

	// We are reusing a pre-existing branch — Cleanup() must not delete it.
	g.branchCreatedByUs = false

	// Clean up any existing worktree first. Ignore the error (the worktree
	// usually doesn't exist) and, unlike Cleanup(), do NOT fall back to
	// deleting the directory: at this point the path has not been
	// established as a session-owned worktree, and a path that stays
	// blocked surfaces loudly via the `worktree add` below (#802 audit).
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)

	// Prune stale worktree metadata BEFORE re-adding. If the worktree
	// directory was deleted externally (rm -rf, disk cleanup, etc.), git
	// still tracks it internally and `worktree add <same-path>` fails with
	// "missing but already registered worktree". Recent git clears that
	// registration on the `worktree remove -f` above, but older git errors
	// ("is not a working tree") and leaves it behind; pruning here recovers
	// either way. Mirrors the prune-before-add ordering in setupNewWorktree.
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	// Create a new worktree from the existing branch
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", g.worktreePath, g.branchName); err != nil {
		return fmt.Errorf("failed to create worktree from branch %s: %w", g.branchName, err)
	}

	// Resolve the base commit SHA so diffs and other operations have a reference point.
	// Try merge-base between the branch and origin's default branch first, then fall back to HEAD.
	baseRef := g.resolveOriginHead()
	if baseRef == "" {
		baseRef = "HEAD"
	}
	output, err := g.runGitCommand(g.repoPath, "merge-base", baseRef, g.branchName)
	if err == nil {
		g.baseCommitSHA = strings.TrimSpace(output)
	} else {
		// Fallback: use the branch's own HEAD as the base commit
		output, err = g.runGitCommand(g.worktreePath, "rev-parse", "HEAD")
		if err == nil {
			g.baseCommitSHA = strings.TrimSpace(output)
		}
	}

	return nil
}

// resolveOriginHead tries to resolve the latest commit from origin's default branch.
// It fetches from origin first, then tries origin/HEAD, origin/main, and origin/master.
// Returns the commit SHA if successful, or empty string if no remote ref is available.
func (g *GitWorktree) resolveOriginHead() string {
	// Fetch from origin to ensure we have the latest refs (best-effort). This
	// is the one network call on the session-creation path, so it is bounded
	// by networkGitTimeout: a stalled remote must not hang creation forever
	// (#896). The error is intentionally ignored — on timeout or failure we
	// fall through to whatever origin refs are already cached locally.
	_, _ = g.runGitNetworkCommand(g.repoPath, "fetch", "origin")

	// Try origin/HEAD (symbolic ref pointing to the default branch)
	for _, ref := range []string{"origin/HEAD", "origin/main", "origin/master"} {
		output, err := g.runGitCommand(g.repoPath, "rev-parse", ref)
		if err == nil {
			return strings.TrimSpace(string(output))
		}
	}
	return ""
}

// setupNewWorktree creates a new worktree from origin's default branch (or HEAD as fallback)
func (g *GitWorktree) setupNewWorktree() error {
	// We are creating the branch ourselves — Cleanup() may delete it.
	g.branchCreatedByUs = true

	// Clean up any existing worktree first. Ignore the error (the worktree
	// usually doesn't exist) and, unlike Cleanup(), do NOT fall back to
	// deleting the directory: at this point the path has not been
	// established as a session-owned worktree, and a path that stays
	// blocked surfaces loudly via the `worktree add` below (#802 audit).
	_, _ = g.runGitCommand(g.repoPath, "worktree", "remove", "-f", g.worktreePath)

	// Prune stale worktree metadata BEFORE deleting the branch. If `worktree
	// remove -f` above failed (corrupted .git pointer, etc.), git still tracks
	// the worktree internally and `branch -D` will fail with "branch is
	// checked out", leaving the orphaned branch behind and blocking
	// `worktree add -b` below.
	_, _ = g.runGitCommand(g.repoPath, "worktree", "prune")

	// Clean up any existing branch using git CLI (much faster than go-git PlainOpen)
	_, _ = g.runGitCommand(g.repoPath, "branch", "-D", g.branchName) // Ignore error if branch doesn't exist

	// Try to base the new branch off origin's default branch for a fresh starting point.
	// Fall back to HEAD if no remote is available.
	baseCommit := g.resolveOriginHead()
	if baseCommit == "" {
		output, err := g.runGitCommand(g.repoPath, "rev-parse", "HEAD")
		if err != nil {
			if strings.Contains(err.Error(), "fatal: ambiguous argument 'HEAD'") ||
				strings.Contains(err.Error(), "fatal: not a valid object name") ||
				strings.Contains(err.Error(), "fatal: HEAD: not a valid object name") {
				return fmt.Errorf("this appears to be a brand new repository: please create an initial commit before creating an instance")
			}
			return fmt.Errorf("failed to get HEAD commit hash: %w", err)
		}
		baseCommit = strings.TrimSpace(string(output))
		log.InfoLog.Printf("no origin remote found, falling back to HEAD for new worktree")
	}
	g.baseCommitSHA = baseCommit

	// Create a new worktree from the base commit.
	// This starts the worktree with a clean slate without inheriting uncommitted changes.
	if _, err := g.runGitCommand(g.repoPath, "worktree", "add", "-b", g.branchName, g.worktreePath, baseCommit); err != nil {
		return fmt.Errorf("failed to create worktree from commit %s: %w", baseCommit, err)
	}

	return nil
}

// CleanupState is what Cleanup could ESTABLISH about the worktree it was asked to
// remove, returned SEPARATELY from the error (#1917).
//
// THE ZERO VALUE IS UNKNOWN, deliberately. Every miss in this PR's review history
// was the same shape — a bounded call tripped its deadline and something
// destructive proceeded anyway — because the safe outcome had to be REMEMBERED and
// the destructive one was the default. Inverting that makes forgetting produce the
// safe result: a state nobody set, or a struct field nobody filled, refuses to
// destroy rather than permitting it.
//
// Callers must not construct this themselves; it comes from cleanupRun.state(),
// which reports Settled only if no command in the run tripped its deadline.
type CleanupState int

const (
	// CleanupStateUnknown (the ZERO VALUE): a bounded git command tripped its
	// deadline, so the workspace may be partially removed and still registered —
	// or nobody established the outcome at all. Callers MUST keep the session
	// record so the cleanup can be retried.
	CleanupStateUnknown CleanupState = iota
	// CleanupSettled: every git command in the run ANSWERED. The worktree is gone,
	// or Cleanup's #802/#726 decision tree deliberately left it and said why —
	// either way the outcome is established and the caller's best-effort contract
	// governs.
	CleanupSettled
)

// cleanupRun executes ONE Cleanup and OWNS its state.
//
// This is the structural half of the #1917 hardening. Cleanup runs five bounded git
// commands; the state used to be asserted once (`state := CleanupSettled`) and
// downgraded by hand at ONE of them, so `branch -D`, both `prune`s and the
// `worktree list` probe could all trip their deadlines and still report Settled —
// and a timed-out probe could even open the door to an UNBOUNDED os.RemoveAll,
// reintroducing the very wedge this work removes.
//
// The author no longer writes the state at all. Every command goes through run.git,
// which records a tripped deadline; every destructive act goes through a method that
// refuses while the run is unknown; state() derives the answer. A command added to
// Cleanup participates automatically, because using the run is the only way to reach
// git from here — there is no marking left to forget.
type cleanupRun struct {
	g       *GitWorktree
	errs    []error
	unknown bool
}

// git runs one bounded local git command and RECORDS a tripped deadline. This is
// the only place in the cleanup path that decides what a deadline means.
func (r *cleanupRun) git(args ...string) (string, error) {
	out, err := r.g.runGitLocalCommand(r.g.repoPath, args...)
	if errors.Is(err, context.DeadlineExceeded) {
		r.unknown = true
		// Latch it on the WORKSPACE too: a retry gets a fresh run, and without this
		// the knowledge that this filesystem stalls would die with the attempt
		// (#1917 round 6).
		r.g.markCleanupStalled()
	}
	return out, err
}

// destructive runs a git command that DESTROYS something, and refuses once the run
// is unknown (#1917 round 8).
//
// Every destructive act in the run must be gated, not just the file deletion. When
// `git worktree remove -f` times out AFTER deregistering the checkout but BEFORE
// deleting its files, removeDir correctly preserved the directory — and cleanup
// then went on to `git branch -D`, which SUCCEEDED precisely because the checkout
// was now unregistered, deleting the retained workspace's only ref and making its
// unique commits unreachable. Saving the files and destroying the only pointer to
// them is worse than either alone: the workspace survives and nothing can find it.
//
// So the rule is the run's, not each step's: once anything here is unknown, nothing
// else may destroy.
func (r *cleanupRun) destructive(what string, args ...string) (string, error) {
	if r.unknown || r.g.cleanupHasStalled() {
		err := fmt.Errorf("%w: refusing to %s: a cleanup command timed out against this workspace, so it is being retained — destroying its metadata would leave the files with nothing pointing at them", errRefusedDestructive, what)
		r.errs = append(r.errs, err)
		r.unknown = true
		return "", err
	}
	return r.git(args...)
}

// errRefusedDestructive marks an act this run DECLINED because the workspace's
// state is unknown — as opposed to one that RAN and failed.
//
// Callers must tell them apart. destructive() has already recorded a refusal, so
// re-reporting it duplicates; but a command that ran and TIMED OUT sets r.unknown
// itself, and suppressing that on an "is the run unknown?" test would swallow the
// very deadline the caller needs to see. That mistake is easy and was made here.
var errRefusedDestructive = errors.New("refused: the workspace's state is unknown")

// removeDir is the choke point for the one destructive act Cleanup performs
// directly. It REFUSES while the run is unknown: whatever stalled git (a hung
// mount, a D-state process holding the tree) stalls os.RemoveAll on the very same
// paths, and os.RemoveAll takes no context — so it would hang forever and defeat
// the bound. Refusing leaves the directory for a later retry, which is recoverable.
func (r *cleanupRun) removeDir(path string) {
	// Consult the WORKSPACE's latch, not just this run's flag. A retry after a
	// timeout arrives with a clean run, and the git probes it makes can now answer
	// "not registered" — because the timed-out remove had already deregistered the
	// checkout before it stalled. Trusting only r.unknown therefore walks straight
	// back into the unbounded delete on the second attempt (#1917 round 6).
	if r.unknown || r.g.cleanupHasStalled() {
		// Refusing IS an unknown outcome: the directory is still there, so the run
		// must report it and the caller must keep the record. Marking the run here
		// rather than relying on the caller keeps that from being a fourth thing
		// someone has to remember.
		r.unknown = true
		r.errs = append(r.errs, fmt.Errorf("refusing to delete worktree directory %s: a cleanup command has timed out against this workspace, so an unbounded delete could hang the daemon; leaving it in place — a daemon restart re-probes it", path))
		return
	}
	if err := os.RemoveAll(path); err != nil {
		r.errs = append(r.errs, fmt.Errorf("failed to remove worktree directory %s: %w", path, err))
	}
}

// prune runs `git worktree prune` through the run, so its deadline counts.
func (r *cleanupRun) prune() {
	// Destructive metadata: prune drops git's record of worktrees it believes are
	// gone. Gated like every other destructive step.
	if _, err := r.destructive("prune worktree metadata", "worktree", "prune"); err != nil {
		// Same rule as branch -D: a refusal recorded itself, a real failure is the
		// caller's to see.
		if !errors.Is(err, errRefusedDestructive) {
			r.errs = append(r.errs, fmt.Errorf("failed to prune worktrees: %w", err))
		}
	}
}

// registered reports whether git still lists the worktree. The bool is only
// meaningful when ok is true; a timed-out probe reports ok=false AND marks the run
// unknown via run.git, so no caller can mistake "could not ask" for "not there".
func (r *cleanupRun) registered() (yes bool, ok bool) {
	output, err := r.git("worktree", "list", "--porcelain")
	if err != nil {
		return false, false
	}
	return worktreeListed(output, r.g.worktreePath), true
}

// state derives the run's outcome. Settled ONLY if nothing tripped a deadline.
func (r *cleanupRun) state() CleanupState {
	if r.unknown {
		return CleanupStateUnknown
	}
	return CleanupSettled
}

// Cleanup removes the worktree and associated branch. It reports whether it
// ESTABLISHED the outcome (see CleanupState) alongside any error: callers that go
// on to delete the session's record MUST gate on the state, not on the error.
// If the worktree was not created by agent-factory (externalWorktree), only prune is done.
func (g *GitWorktree) Cleanup() (CleanupState, error) {
	// Cancel any in-flight post-worktree hooks before removing the worktree.
	if g.hooksCancel != nil {
		g.hooksCancel()
	}

	// The run owns the state from the first line, so even the early returns below
	// derive it instead of asserting one (#1917). Nothing in this function names a
	// CleanupState constant: that is the rule that makes the next command added here
	// safe by default. These early paths run no git at all, so the run is trivially
	// settled — but that is r.state()'s answer to give, not this function's.
	r := &cleanupRun{g: g}

	// For external worktrees, don't remove the worktree or delete the branch
	if g.externalWorktree {
		return r.state(), nil
	}

	// Guard against empty paths that would cause git commands to fail or
	// operate on unintended directories.
	if g.repoPath == "" {
		return r.state(), fmt.Errorf("cannot clean up worktree: repo path is empty")
	}
	if g.worktreePath == "" {
		return r.state(), fmt.Errorf("cannot clean up worktree: worktree path is empty")
	}

	// Check if worktree path exists before attempting removal
	if _, err := os.Stat(g.worktreePath); err == nil {
		// Reap any process still writing inside the tree BEFORE removing it
		// (#2025). Both the git remove below and the os.RemoveAll fallback delete
		// recursively and fail "directory not empty" only when a live writer keeps
		// re-creating files under the path faster than they can be unlinked; a
		// session whose agent backgrounded a survivor (installer, dev server) leaks
		// the worktree otherwise. Best-effort and bounded — see reapWorktreeWriters.
		reapWorktreeWriters(g.worktreePath)

		// Remove the worktree using git command. Bounded by localGitTimeout
		// (#1917): this recursive delete is the one local git command that
		// genuinely stalls forever (hung mount, D-state process in the tree), and
		// Cleanup runs inside the daemon's kills-in-flight guard.
		if _, err := r.git("worktree", "remove", "-f", g.worktreePath); err != nil {
			log.ErrorLog.Printf("failed to remove worktree %s: %v", g.worktreePath, err)
			// A failed `git worktree remove -f` may still have released the
			// registration. Decide whether the directory is ours to delete
			// by asking git, not by matching error strings (#802):
			//
			//   - Path no longer in `git worktree list`: git has let go of
			//     the worktree but the directory survived. Observed when the
			//     recursive delete aborts partway ("failed to delete ...:
			//     Directory not empty") because the dying agent process wrote
			//     into the tree mid-removal — git deregisters first, then
			//     fails to finish deleting (#802). RemoveAll the leftovers;
			//     the prune below reconciles any remaining metadata.
			//   - Still registered + "validation failed": the worktree's
			//     `.git` pointer is corrupted (#719/#726). git refuses to
			//     remove it, but it is unambiguously one of our registered
			//     worktrees, so deleting the directory is safe.
			//   - Still registered + any other error (locked worktree,
			//     submodules, permissions): git owns the path and we don't
			//     know why removal failed — surface the error instead of
			//     deleting data (preserves the best-effort Kill behavior of
			//     #478).
			//
			// Every branch here is git ANSWERING. A deadline anywhere in the run —
			// the remove itself, OR the registration probe below — leaves the state
			// unknown, and r.removeDir refuses on that, so no timeout can reach the
			// unbounded os.RemoveAll.
			if r.shouldRemoveWorktreeDir(err) {
				r.removeDir(g.worktreePath)
			} else {
				r.errs = append(r.errs, err)
			}
		}
	} else if !os.IsNotExist(err) {
		// Only append error if it's not a "not exists" error
		r.errs = append(r.errs, fmt.Errorf("failed to check worktree path: %w", err))
	}

	// Prune stale worktree metadata BEFORE deleting the branch. When the
	// `git worktree remove -f` above fails (e.g. the worktree's `.git`
	// pointer file was removed externally), git still tracks the worktree
	// internally and `git branch -D` will fail with "branch is checked
	// out", leaving an orphaned branch behind. Mirrors the ordering in
	// CleanupWorktreesForRepo (#330). Best-effort: a prune failure here
	// should not block the branch-delete attempt.
	r.prune()

	// Only delete the branch if this session actually created it. When we
	// reused a pre-existing branch via setupFromExistingBranch(), the branch
	// may contain unrelated user work and must be preserved.
	if g.branchCreatedByUs {
		// THE branch delete. Gated (#1917 round 8): a timed-out `worktree remove`
		// deregisters before it stalls, which is exactly what makes this succeed —
		// so an ungated branch -D destroys the ref of the workspace the same run just
		// decided to keep, and its unique commits become unreachable.
		if _, err := r.destructive("delete branch "+g.branchName, "branch", "-D", g.branchName); err != nil {
			// A REFUSAL already recorded itself. Anything else RAN — including a
			// deadline, which the caller must still see — and "branch not found" is
			// success (#478). Testing r.unknown here instead would swallow the branch
			// delete's own timeout, since that timeout is what set it.
			if !errors.Is(err, errRefusedDestructive) && !strings.Contains(err.Error(), "not found") {
				r.errs = append(r.errs, fmt.Errorf("failed to remove branch %s: %w", g.branchName, err))
			}
		}
	}

	// Final prune to clean up any remaining references. Usually a no-op
	// after the prune above, but mirrors CleanupWorktreesForRepo.
	r.prune()

	if len(r.errs) > 0 {
		return r.state(), errors.Join(r.errs...)
	}
	return r.state(), nil
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

// isWorktreeRegistered reports whether git still lists g.worktreePath as a
// registered worktree of the repo. Used after a failed `git worktree remove`
// to distinguish "git released the worktree but the directory survived"
// (safe to delete manually, #802) from "git still owns the path" (not ours
// to second-guess).
func (g *GitWorktree) isWorktreeRegistered() (bool, error) {
	// Bounded (#1917): this is Cleanup's error-path probe, so it must not be the
	// step that hangs the kill the bounded `worktree remove` above just rescued.
	output, err := g.runGitLocalCommand(g.repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	return worktreeListed(output, g.worktreePath), nil
}

var (
	// worktreeReapGrace is how long a writer discovered inside the worktree gets
	// to exit on its own before it is SIGTERMed. Zero: by the time a worktree is
	// being deleted the pane teardown has already SIGHUP'd the pane's process
	// group and waited for the pane to exit (#802), so a process still writing
	// here has already had its grace and there is no reason to wait again — go
	// straight to the escalation. var, not const, so a test can tune the pacing.
	worktreeReapGrace = 0 * time.Second
	// worktreeReapTermWait is how long a SIGTERMed writer gets before SIGKILL.
	// WaitForExits returns as soon as everything is gone, so a well-behaved writer
	// costs a poll interval, not the whole wait.
	worktreeReapTermWait = 2 * time.Second
)

// reapWorktreeWriters kills every live process still working inside worktreePath
// BEFORE the tree is deleted (#2025): any process whose current working directory
// is at or under the tree, plus that process's whole descendant subtree.
//
// The leak this closes: `git worktree remove -f` and the os.RemoveAll fallback
// both delete recursively and do NOT fail on a merely-non-empty directory — they
// fail "directory not empty" only when files are being CREATED into the tree
// faster than they can be unlinked, i.e. a live process is still writing to it.
// A session whose agent backgrounded a long-lived writer (an installer, a dev
// server, a package manager) can leave that writer alive after the kill tore down
// the agent/tmux — the tmux reaper (#1104) escalates asynchronously and does not
// block this removal — and it then races, and beats, the delete, orphaning the
// worktree (the worktree_ops.go / teardown.go "directory not empty" pair).
// Killing the writers first removes the racer so the delete can finish.
//
// WHICH processes are killed is deliberately narrow (the #1104 "only our own
// descendants" discipline, and the "which children are garbage" hazard): an AF
// worktree directory is a session-private path, so a process cwd'd inside it is
// unambiguously this session's and no unrelated process is ever signalled. The
// kill itself routes through the existing #1104 reaper (proctree.KillEscalating):
// every signal is identity-verified against (pid, start-time) so a recycled PID
// is never hit, and the SIGTERM→SIGKILL escalation is shared, not re-implemented.
//
// Best-effort, like every reaper on this path: an unreadable process table (no
// /proc, an unsupported platform) degrades to a no-op — nothing is reaped, and
// the existing WARNING plus doctor's stale-worktree path own whatever survives.
// It never errors and never loops: a writer that ignores SIGKILL (a D-state,
// uninterruptible I/O, a mount) is left for the removal to fail loudly on, not
// spun on forever.
//
// Linux and darwin both back proctree.WorkingDir (/proc/<pid>/cwd and
// proc_info(PROC_PIDVNODEPATHINFO) respectively), so the reap is live on both
// as of #2050. Elsewhere, and for any process whose cwd the kernel will not
// disclose, WorkingDir reports the honest unknown and that process is simply not
// matched — the safe degradation, since it can only fail to signal the right
// process, never signal the wrong one.
func reapWorktreeWriters(worktreePath string) {
	// The path exists (callers reap only after an os.Stat succeeds), so resolving
	// symlinks here matches /proc/<pid>/cwd, which the kernel already resolves.
	root := normalizeWorktreePath(worktreePath)
	snap, err := proctree.Snapshot()
	if err != nil {
		// Could not READ the process table — never the same fact as "no writers"
		// (proctree's whole design). Skip the reap and let the removal proceed; a
		// writer that really is alive surfaces as the existing "directory not empty"
		// WARNING for doctor to reconcile, never a silently-swept process table.
		return
	}
	seen := make(map[int]bool)
	var procs []proctree.Process
	add := func(p proctree.Process) {
		if !seen[p.PID] {
			seen[p.PID] = true
			procs = append(procs, p)
		}
	}
	for pid := range snap {
		cwd, ok := proctree.WorkingDir(pid)
		if !ok {
			// Foreign process (its cwd link is not readable) or already gone: it
			// cannot be proven to be one of ours, and an unreadable cwd is never
			// treated as a match — the honest-unknown rule this package enforces.
			continue
		}
		if !pathAtOrUnder(root, filepath.Clean(cwd)) {
			continue
		}
		// Take the whole subtree of the matching process: a child of a
		// worktree-cwd'd writer is this session's too even if it chdir'd elsewhere,
		// and it may be the actual file-creator holding the directory non-empty.
		for _, p := range proctree.TreeOf(snap, pid) {
			add(p)
		}
	}
	if len(procs) == 0 {
		return
	}
	proctree.KillEscalating(procs, worktreeReapGrace, worktreeReapTermWait, func(format string, args ...any) {
		// worktreePath is a runtime value that may legally contain `%`, so it MUST
		// be a `%s` ARGUMENT, never spliced into the format string (the #1211 rule
		// the tmux reaper follows). `format` is KillEscalating's own constant literal.
		log.WarningLog.Printf("worktree %s: "+format, append([]any{worktreePath}, args...)...)
	})
}

// pathAtOrUnder reports whether cleaned path p is root itself or a path nested
// inside it. root must already be cleaned and symlink-resolved (normalizeWorktreePath)
// and p cleaned; the kernel resolves /proc/<pid>/cwd, so the caller cleans the cwd
// but need not re-resolve it. A sibling ("/a/b-other") or a parent is rejected —
// only the tree itself and its descendants match.
func pathAtOrUnder(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CleanupWorktreesForRepo removes all worktrees and their associated branches
// for the given repo root. The main worktree (the repo itself) is preserved.
// The repoRoot must be the main repo path; callers should resolve linked
// worktree paths to the main repo root before invoking this function.
func CleanupWorktreesForRepo(repoRoot string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root is empty")
	}

	// Skip cleanup if the repo path no longer exists on disk. `af reset`
	// iterates over collected repo roots, which may include deleted, moved,
	// or unmounted paths; without this check, `git -C` would fail and abort
	// the entire reset before subsequent repos (and DeleteAllInstances) ran.
	if _, err := os.Stat(repoRoot); os.IsNotExist(err) {
		log.WarningLog.Printf("skipping cleanup for deleted repo: %s", repoRoot)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to access repo path: %w", err)
	}

	// List all worktrees from the repo. If the path exists but is no longer a
	// git repo (e.g. `.git` was removed), `git -C` exits non-zero. Treat that
	// like the missing-directory case above: log and skip, so `af reset` can
	// still clean up other repos and reset storage (issue #370).
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		log.WarningLog.Printf("skipping cleanup for non-git path: %s", repoRoot)
		return nil
	}

	// Parse output to get (worktreePath, branchName) pairs.
	// Each block is separated by a blank line. A worktree may have no branch (detached HEAD).
	type worktreeInfo struct {
		path   string
		branch string // empty if detached HEAD
	}
	var worktrees []worktreeInfo
	currentPath := ""
	currentBranch := ""
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") {
			branchPath := strings.TrimPrefix(line, "branch ")
			currentBranch = strings.TrimPrefix(branchPath, "refs/heads/")
		} else if line == "" {
			if currentPath != "" {
				worktrees = append(worktrees, worktreeInfo{path: currentPath, branch: currentBranch})
			}
			currentPath = ""
			currentBranch = ""
		}
	}
	// Handle last entry if output doesn't end with a blank line
	if currentPath != "" {
		worktrees = append(worktrees, worktreeInfo{path: currentPath, branch: currentBranch})
	}

	// Skip the first entry (the main worktree / repo itself)
	if len(worktrees) > 1 {
		for _, wt := range worktrees[1:] {
			// Remove the worktree FIRST (git refuses to delete a branch checked out in a worktree)
			removeCmd := exec.Command("git", "-C", repoRoot, "worktree", "remove", "-f", wt.path)
			if err := removeCmd.Run(); err != nil {
				log.ErrorLog.Printf("failed to remove worktree %s: %v", wt.path, err)
				// Fallback: remove directory manually. Unconditional — no
				// registration re-check needed here, unlike Cleanup(): wt.path
				// was emitted by `git worktree list` moments ago, so git
				// ownership is already established, and `af reset` semantics
				// are "tear everything down" (#802 audit).
				if err := os.RemoveAll(wt.path); err != nil {
					log.ErrorLog.Printf("failed to remove worktree directory %s: %v", wt.path, err)
				}
			}

			// Prune stale worktree metadata (best-effort) BEFORE deleting the
			// branch. When the `git worktree remove -f` above fails and we fall
			// back to os.RemoveAll, git still tracks the worktree internally,
			// causing `git branch -D` to fail with "branch is checked out".
			pruneCmd := exec.Command("git", "-C", repoRoot, "worktree", "prune")
			if err := pruneCmd.Run(); err != nil {
				log.ErrorLog.Printf("failed to prune worktree metadata before deleting branch %s: %v", wt.branch, err)
			}

			// THEN delete the branch
			if wt.branch != "" {
				deleteCmd := exec.Command("git", "-C", repoRoot, "branch", "-D", wt.branch)
				if err := deleteCmd.Run(); err != nil {
					log.ErrorLog.Printf("failed to delete branch %s: %v", wt.branch, err)
				}
			}
		}
	}

	// Prune worktree references
	pruneCmd := exec.Command("git", "-C", repoRoot, "worktree", "prune")
	if _, err := pruneCmd.Output(); err != nil {
		return fmt.Errorf("failed to prune worktrees: %w", err)
	}

	return nil
}

// markCleanupStalled latches the workspace-level "a cleanup command timed out
// here" fact (see GitWorktree.cleanupStalled).
func (g *GitWorktree) markCleanupStalled() { g.cleanupStalled.Store(true) }

// cleanupHasStalled reports whether any cleanup attempt against this workspace has
// ever tripped a deadline. Consulted by removeDir, which must never enter an
// unbounded delete on a filesystem that has already proven it can stall.
func (g *GitWorktree) cleanupHasStalled() bool { return g.cleanupStalled.Load() }
