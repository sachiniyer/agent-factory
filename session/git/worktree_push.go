package git

import (
	"fmt"
	"strings"
)

// archiveSnapshotMessage labels the WIP commit SnapshotAndPushBranch makes for
// uncommitted work at archive time. It is deliberately recognizable so a user
// who restores the session sees why the extra commit exists.
const archiveSnapshotMessage = "af: pre-archive snapshot (uncommitted work)"

// SnapshotAndPushBranch makes the session's branch durable on origin (#1592
// Phase 4 PR6/PR7) — the archive-side primitive for the disposable off-box
// backends (docker/ssh/hook). Because a sandbox is thrown away on archive and
// re-cloned from GitHub on restore, the branch on origin IS the durable workspace
// (epic decision 4), so archive pushes it there before the sandbox is torn down.
//
// It first snapshots any uncommitted work as a WIP commit so archive preserves
// the working tree, matching the local worktree-move archive's "nothing lost"
// guarantee as closely as the disposable model allows: local archive moves the
// worktree bytes verbatim, but a sandbox has no bytes to move once reaped, so
// the only way its uncommitted work survives is to commit it. The commit is
// clearly labeled (archiveSnapshotMessage) and made with --no-verify so a slow
// or failing pre-commit hook never blocks an archive. A clean tree skips the
// commit entirely.
//
// The snapshot commit lands on whatever the worktree's HEAD currently is, and we
// push that HEAD to the session branch on origin (HEAD:refs/heads/<branch>) — NOT
// a bare `push origin <branch>`. This closes #1721: if an agent switched branches
// inside the sandbox (git checkout -b …) or left HEAD detached, `git commit`
// applies to the current HEAD, so pushing the stored g.branchName would push a
// stale tip that misses the just-committed work and report success — silent data
// loss once the sandbox is reaped. Pushing HEAD makes the actual working-tree
// state durable on the session branch regardless of which branch/HEAD the
// worktree is on. Restore re-clones this branch, so it recovers that state. The
// push is a plain (non-force) push, so a genuinely divergent HEAD is rejected by
// origin and surfaces as an error here rather than silently overwriting or losing
// history.
//
// Before snapshotting, the worktree is reconciled with origin so a behind-only
// HEAD does not wedge the archive. A routine agent action — `git checkout
// <old-sha>`, `git reset --hard` on a restored session — leaves HEAD an ancestor
// of origin's session branch. That state overwrites nothing on origin, yet the
// plain non-force push of HEAD is rejected as non-fast-forward, so the unreformed
// path aborted the archive to Lost AND stranded the just-created snapshot commit
// inside the sandbox (the retry loop then re-failed identically, and the
// --force-reap / `af sessions kill` off-ramps tore the box down with the WIP
// still in it — the exact data loss the "nothing lost" guarantee exists to
// prevent). So we fetch origin's branch, classify HEAD against FETCH_HEAD, and:
//
//   - behind (HEAD is an ancestor of FETCH_HEAD): a clean tree fast-forwards HEAD
//     and the working tree to origin's tip (no snapshot needed — origin already
//     holds this history); a dirty tree builds the snapshot commit directly on
//     FETCH_HEAD via plumbing (write-tree/commit-tree + reset --soft) so the push
//     becomes a trivial fast-forward, WITHOUT `git stash` — refs/stash is shared
//     across worktrees (#2801) and stash-create does not capture untracked files,
//     so the snapshot recipe would both race sibling sessions and drop the very
//     untracked work the snapshot exists to preserve.
//   - diverged (neither is an ancestor of the other): refuse BEFORE creating any
//     snapshot commit, with an error that names the condition and distinguishes
//     it from the recoverable cases — a rejected push never strands a local WIP
//     the retry loop cannot recover.
//   - ahead or equal, or FETCH_HEAD unavailable (origin unreachable, or the
//     branch does not yet exist on origin for a first archive): fall through to
//     the snapshot + push below, which fast-forwards origin (or creates the
//     branch on the first push).
//
// The push runs under the network timeout (a stalled or auth-prompting origin
// must not hang the archive forever). Credentials are the sandbox image's/host's
// responsibility — the same `origin` the sandbox cloned from must be pushable
// (documented in docs/backends.md).
//
// Every git-subprocess failure is annotated with a Go-side write probe over
// the worktree and common-dir filesystems: inside a sandbox these calls are
// subprocesses, so a disk-full failure arrives as an *exec.ExitError whose
// errno survives only as stderr text. The probe re-verifies the environment
// so the archive RPC's error envelope can carry the resource-exhaustion class
// across the wire (#5295 review).
func (g *GitWorktree) SnapshotAndPushBranch() (string, error) {
	if g.externalWorktree {
		return "", fmt.Errorf("cannot push an in-place/external worktree branch (it is user-owned)")
	}
	branch := strings.TrimSpace(g.branchName)
	if branch == "" {
		return "", fmt.Errorf("cannot push worktree %s: branch name is empty", g.worktreePath)
	}

	// Probe the worktree itself — the destination filesystem these git writes
	// actually hit. Probing the parent would read the wrong answer when the
	// worktree is its own mount point, and probing inside the tracked tree is
	// safe because the probe unlinks its file before writing a single byte:
	// the name never coexists with content, so `git add -A` can never stage
	// it (#5295 review).
	probeDir := g.worktreePath

	// One debris edge survives the unlink-before-write contract: on the
	// named-fallback path (no O_TMPFILE) a double unlink failure leaves a
	// .af-add-probe-* name in this tree — the probe reports it in its error
	// text and the file is left alone. It is treated exactly as user
	// content — never deleted on a name match, and snapshotted like any
	// other file: a committed stray is a cosmetic wart, strictly better
	// than discarding user work a name filter cannot distinguish (#5295
	// review).

	// Snapshot uncommitted work so it survives the sandbox teardown. `git status
	// --porcelain` is empty for a clean tree, in which case there is nothing to
	// commit and we push the committed history as-is.
	//
	// --untracked-files=normal is load-bearing (#2101): bare `git status
	// --porcelain` honours status.showUntrackedFiles, and a worktree shares
	// .git/config with its main repo, so a user who set that to `no` made this
	// gate read a tree full of untracked work as clean. The `add -A` below would
	// have staged those files perfectly well — it simply never ran, and the work
	// died with the sandbox. The flag overrides the config, so the gate answers
	// "is there anything to snapshot?" the same way for every user. `normal`
	// rather than `all` because we only need that boolean: `normal` reports an
	// untracked directory as one `?? dir/` entry instead of walking every file
	// beneath it, which is the same answer for less work on a big tree.
	status, err := g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", g.annotateGitWriteError(fmt.Errorf("failed to check worktree status before archive push: %w", err), probeDir)
	}
	dirty := strings.TrimSpace(status) != ""

	// Reconcile HEAD with origin BEFORE creating any snapshot commit, so a
	// rejected push never strands a local WIP commit the retry loop cannot
	// recover. Fetch origin's session branch (best-effort, bounded by
	// networkGitTimeout); a failed fetch — origin unreachable, or the branch
	// does not yet exist on origin for a first archive — leaves FETCH_HEAD
	// unset and we fall through to the unconditional push below, which still
	// fast-forwards origin for the equal/ahead cases and creates the branch
	// on the first push. The refspec is qualified with refs/heads/ so a
	// same-named tag on origin cannot resolve into FETCH_HEAD in its place
	// (git fetch selects a same-named tag for an unqualified refspec), which
	// would fast-forward HEAD onto unrelated tagged history or report a false
	// divergence — the classification and the push both target the branch.
	_, _ = g.runGitNetworkCommand(g.worktreePath, "fetch", "origin", "refs/heads/"+branch)
	// rev-parse --verify -q confirms FETCH_HEAD resolved to a commit before
	// we classify; without it a missing FETCH_HEAD would read as "diverged"
	// and refuse a perfectly pushable ahead/equal HEAD.
	if _, verifyErr := g.runGitCommand(g.worktreePath, "rev-parse", "--verify", "-q", "FETCH_HEAD"); verifyErr == nil {
		// isAncestor reports whether `a` is an ancestor of `b` (a commit
		// counts as its own ancestor, so an equal HEAD reads as "behind"
		// here and is handled by the same path). merge-base --is-ancestor
		// exits 0 iff the first rev is an ancestor of the second.
		isAncestor := func(a, b string) bool {
			_, e := g.runGitCommand(g.worktreePath, "merge-base", "--is-ancestor", a, b)
			return e == nil
		}
		behind := isAncestor("HEAD", "FETCH_HEAD")
		if behind {
			if !dirty {
				// Clean tree: fast-forward HEAD and the working tree to
				// origin's tip. No snapshot commit is needed — origin
				// already holds this history, and the push below becomes a
				// no-op fast-forward. Use `reset --hard` rather than
				// `merge --ff-only`: the ancestor check above already proved
				// FETCH_HEAD is a descendant of HEAD, so this is a
				// fast-forward that discards nothing (the tree is clean), and
				// reset does NOT run a post-merge hook (git merge does, and
				// runGitCommand has no timeout, so a slow/blocking post-merge
				// hook would hang the archive — --no-verify does not suppress
				// post-merge).
				if out, ffErr := g.runGitCommand(g.worktreePath, "reset", "--hard", "FETCH_HEAD"); ffErr != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to fast-forward HEAD onto origin's tip before archive push: %s: %w",
						strings.TrimSpace(out), ffErr), probeDir)
				}
				// Do NOT re-check status here: a fast-forward can advance a
				// submodule gitlink in the index without checking out the new
				// submodule commit, and a re-checked `git status` would report
				// that as ` M <submodule>`. That is reconciliation, not user
				// work — the snapshot block below would stage the stale gitlink
				// and push a commit reverting origin's submodule update. The
				// tree was clean before (we are in the !dirty branch), so
				// preserve that classification: leave `status` empty and let
				// the push fast-forward origin's branch unchanged.
			} else {
				// Dirty tree: build the snapshot commit directly on origin's
				// tip so the push fast-forwards, WITHOUT `git stash`
				// (refs/stash is shared across worktrees, #2801, and stash
				// create does not capture untracked files) and WITHOUT
				// disturbing the working-tree bytes. Stage the work, write a
				// tree, commit it with FETCH_HEAD as its parent, then reset
				// --soft HEAD onto it: the index and working tree already
				// match the new commit (we just staged everything), so the
				// tree reads clean and the snapshot block below is a no-op.
				// The snapshot is created only after reconciliation is
				// decided, so a failure here strands nothing.
				if _, e := g.runGitCommand(g.worktreePath, "add", "-A"); e != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to stage uncommitted work before archive push: %w", e), probeDir)
				}
				treeSha, e := g.runGitCommand(g.worktreePath, "write-tree")
				if e != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to write snapshot tree before archive push: %w", e), probeDir)
				}
				fetchHead, e := g.runGitCommand(g.worktreePath, "rev-parse", "FETCH_HEAD")
				if e != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to resolve origin's tip before archive push: %w", e), probeDir)
				}
				commitSha, e := g.runGitCommand(g.worktreePath, "commit-tree", strings.TrimSpace(treeSha),
					"-p", strings.TrimSpace(fetchHead), "-m", archiveSnapshotMessage)
				if e != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to create snapshot commit on origin's tip before archive push: %w", e), probeDir)
				}
				if out, e := g.runGitCommand(g.worktreePath, "reset", "--soft", strings.TrimSpace(commitSha)); e != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to advance HEAD onto snapshot commit before archive push: %s: %w",
						strings.TrimSpace(out), e), probeDir)
				}
				// Re-read status after the dirty reconciliation: reset --soft
				// onto the just-staged commit leaves the tree clean, so the
				// snapshot block below is a no-op and we proceed straight to
				// the push. (The clean branch deliberately skips this re-check
				// — see the submodule-gitlink note above.)
				status, err = g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
				if err != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to re-check worktree status after reconciling with origin: %w", err), probeDir)
				}
			}
		} else if !isAncestor("FETCH_HEAD", "HEAD") {
			// FETCH_HEAD is NOT an ancestor of HEAD either, and HEAD is not
			// an ancestor of FETCH_HEAD: genuine divergence. Refuse BEFORE
			// creating any snapshot commit, with an error that names the
			// condition and distinguishes it from the recoverable cases —
			// a behind-only HEAD was the wedge, and this is the other half
			// of the classification the old path never did.
			return "", g.annotateGitWriteError(fmt.Errorf(
				"archive push aborted: HEAD has diverged from origin's %q branch and no fast-forward is possible "+
					"(behind-only HEAD is reconciled automatically; genuine divergence is not) — "+
					"reconcile HEAD with origin inside the sandbox (e.g. `git merge origin/%[1]s` or `git rebase origin/%[1]s`) before re-archiving, "+
					"or the uncommitted work in the sandbox will be lost when it is reaped",
				branch), probeDir)
		}
		// else: ahead or equal — the push below will fast-forward (or no-op).
	}

	if strings.TrimSpace(status) != "" {
		// No name exclusions here: whatever the sweep could not prove it
		// created is user content and must be snapshotted like everything
		// else — filtering probe-shaped names out of `add` would silently
		// drop a user file whose only sin is its name (#5295 review).
		if _, err := g.runGitCommand(g.worktreePath, "add", "-A"); err != nil {
			return "", g.annotateGitWriteError(fmt.Errorf("failed to stage uncommitted work before archive push: %w", err), probeDir)
		}
		if _, err := g.runGitCommand(g.worktreePath, "commit", "--no-verify", "-m", archiveSnapshotMessage); err != nil {
			return "", g.annotateGitWriteError(fmt.Errorf("failed to snapshot uncommitted work before archive push: %w", err), probeDir)
		}
	}

	// Push the CURRENT HEAD (which now carries the snapshot commit, if any) to the
	// session branch on origin, under the network timeout. Pushing HEAD rather than
	// the stored branch name means the actual working-tree state is what becomes
	// durable even if the agent left HEAD on a different branch or detached (#1721);
	// a non-fast-forward divergence is rejected by origin and surfaces here rather
	// than silently dropping the work. A "src refspec ..." or auth failure surfaces
	// here too, with git's stderr folded in.
	refspec := "HEAD:refs/heads/" + branch
	if out, err := g.runGitNetworkCommand(g.worktreePath, "push", "origin", refspec); err != nil {
		return "", g.annotateGitWriteError(fmt.Errorf("failed to push branch %q to origin (archive stores durable state on GitHub): %s: %w",
			branch, strings.TrimSpace(out), err), probeDir)
	}
	return branch, nil
}
