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
	//
	// Fetch from the push URL (git remote get-url --push), not the fetch URL,
	// so the classification inspects the same server the push targets: a
	// separate remote.origin.pushurl (git remote -h documents the distinct
	// [--push] endpoint) would otherwise fetch from an upstream or read-only
	// source while the push writes to the user's fork, so a same-named branch
	// on the upstream could report a false divergence or reset HEAD onto an
	// unrelated tip the push then carries. Fall back to the "origin" remote
	// if the push URL is not configured (the common case: one remote, one URL)
	// or get-url fails.
	fetchRemote := "origin"
	if pushURL, urlErr := g.runGitCommand(g.worktreePath, "remote", "get-url", "--push", "origin"); urlErr == nil {
		if trimmed := strings.TrimSpace(pushURL); trimmed != "" {
			fetchRemote = trimmed
		}
	}
	_, _ = g.runGitNetworkCommand(g.worktreePath, "fetch", fetchRemote, "refs/heads/"+branch)
	// rev-parse --verify -q confirms FETCH_HEAD resolved to a commit before
	// we classify; without it a missing FETCH_HEAD would read as "diverged"
	// and refuse a perfectly pushable ahead/equal HEAD. The resolved OID is
	// pinned here and used for every later step (the ancestry check, the
	// fast-forward, the read-tree, the commit-tree parent): FETCH_HEAD is a
	// mutable pseudoref that a concurrent `git fetch` (the still-running agent
	// can issue one while ArchiveSandbox runs this push before reaping the
	// sandbox) overwrites in place — `git fetch -h` documents that the default
	// fetch writes fetched references to FETCH_HEAD. Re-resolving it on each
	// step would let a later fetch swap the commit the classification verified,
	// so the reset/commit could land on an agent-fetched branch and the push
	// would carry that unrelated history onto the session branch when it is a
	// fast-forward. The pinned OID is immutable for the rest of this call.
	fetchHead, verifyErr := g.runGitCommand(g.worktreePath, "rev-parse", "--verify", "-q", "FETCH_HEAD")
	if verifyErr == nil {
		fetchHead = strings.TrimSpace(fetchHead)
		// isAncestor reports whether `a` is an ancestor of `b` (a commit
		// counts as its own ancestor, so an equal HEAD reads as "behind"
		// here and is handled by the same path). merge-base --is-ancestor
		// exits 0 iff the first rev is an ancestor of the second.
		isAncestor := func(a, b string) bool {
			_, e := g.runGitCommand(g.worktreePath, "merge-base", "--is-ancestor", a, b)
			return e == nil
		}
		behind := isAncestor("HEAD", fetchHead)
		if behind {
			// Re-check the worktree status AFTER the network fetch, before
			// the destructive clean-tree fast-forward. The fetch is bounded
			// by networkGitTimeout (not zero), and the still-running agent can
			// edit a tracked file during it — ArchiveSandbox invokes this
			// push before tearing down the sandbox, so the agent is live while
			// the fetch runs. `dirty` was computed before the fetch, so
			// trusting it would let `reset --hard` overwrite a fresh edit and
			// the push would lose it when the sandbox is reaped. If the tree is
			// no longer clean, take the dirty reconciliation path instead — it
			// preserves the working-tree bytes (read-tree -m -u + add -A)
			// rather than overwriting them.
			if !dirty {
				status, err = g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
				if err != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to re-check worktree status before reconciling with origin: %w", err), probeDir)
				}
				dirty = strings.TrimSpace(status) != ""
			}
			if !dirty {
				// Clean tree: fast-forward HEAD and the working tree to
				// origin's tip. No snapshot commit is needed — origin
				// already holds this history, and the push below becomes a
				// no-op fast-forward. Use `reset --keep` rather than
				// `merge --ff-only` (which runs a post-merge hook; the
				// runGitCommand runner has no timeout, so a slow/blocking
				// hook would hang the archive — --no-verify does not suppress
				// post-merge) or `reset --hard` (which would unconditionally
				// overwrite a concurrent edit that lands in the window
				// between the status re-check above and this reset — the
				// still-running agent can modify a tracked file there, and a
				// destructive checkout operation must not lose it). The
				// ancestor check above already proved FETCH_HEAD is a
				// descendant of HEAD, so for a clean tree this is a
				// fast-forward that discards nothing; `--keep` only aborts
				// when a local change would be overwritten by the fast-forward,
				// which closes the post-re-check race by surfacing the edit as
				// an error rather than discarding it (the working tree is
				// unchanged on abort, so the archive can be retried without
				// data loss). The reset is routed through runGitLocalCommand
				// (worktree_git.go documents it as the runner destructive
				// checkout operations must use): runGitCommand is unbounded,
				// and a stalled filesystem would hang the archive forever on the
				// checkout-producing reset.
				if out, ffErr := g.runGitLocalCommand(g.worktreePath, "reset", "--keep", fetchHead); ffErr != nil {
					return "", g.annotateGitWriteError(fmt.Errorf("failed to fast-forward HEAD onto origin's tip before archive push: %s: %w",
						strings.TrimSpace(out), ffErr), probeDir)
				}
				// Synchronize submodule checkouts after the fast-forward so a
				// retry of SnapshotAndPushBranch (e.g. the ArchiveSandbox retry
				// in the reap-failure path) does not see ` M <submodule>` — the
				// non-recursive reset advances the gitlink in the index without
				// checking out the new submodule commit, and a retry's `git
				// status` would report that as dirt, sending it into the dirty
				// path where `add -A` stages the stale gitlink and pushes a
				// commit that reverts origin's submodule update. --recursive
				// descends into nested submodules; the update is best-effort
				// (a failure leaves the gitlink advanced and the push a no-op,
				// degrading only the retry's cleanliness, not the archive).
				// runGitNetworkCommand bounds a submodule fetch (the new commit
				// may not be local) under the network timeout.
				_, _ = g.runGitNetworkCommand(g.worktreePath, "submodule", "update", "--recursive")
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
				// tree, commit it with the pinned origin tip as its parent,
				// then reset --soft HEAD onto it: the index and working tree
				// already match the new commit (we just staged everything), so
				// the tree reads clean and the snapshot block below is a no-op.
				// The snapshot is created only after reconciliation is
				// decided, so a failure here strands nothing.
				//
				// A merge in progress (MERGE_HEAD present) is handled by the
				// snapshot block below (git commit --no-verify), not this
				// plumbing path: commit-tree would create a single-parent commit
				// and leave MERGE_HEAD in place, after which `git reset --soft`
				// aborts ("Cannot do a soft reset in the middle of a merge")
				// and the archive fails indefinitely with no durable snapshot.
				// The porcelain commit completes the merge with both parents,
				// honors commit.gpgSign, and clears MERGE_HEAD. Skip the
				// plumbing for the merge case and fall through to the snapshot
				// block; the push below fast-forwards when HEAD is at the
				// pinned tip (the common resolved-merge case) and is rejected
				// (surfacing the error, with the merge work committed in the
				// worktree for recovery) when HEAD is behind.
				if _, mergeErr := g.runGitCommand(g.worktreePath, "rev-parse", "--verify", "-q", "MERGE_HEAD"); mergeErr != nil {
					// No merge in progress: use the plumbing path to build the
					// snapshot commit directly on the pinned origin tip so the
					// push fast-forwards. Update the index (and working tree)
					// to the origin tip's tree with `read-tree -m -u HEAD
					// <fetchHead>` so paths the user did NOT touch reflect origin's
					// tip, not the behind HEAD. The two-way merge advances both
					// the index AND the working tree for files the user did not
					// modify: for a behind HEAD, files unchanged by the user are
					// fast-forwarded to the tip's version in the working tree
					// (git read-tree -h documents -u as the flag that updates the
					// working tree). Without -u, the index-only merge would
					// advance the index but leave the stale HEAD bytes in the
					// working tree, and the following `add -A` would restage
					// those stale bytes — silently reverting an upstream change
					// to an in-cone file the user did not touch. With -u, `add
					// -A` stages the tip's version for those files (no change)
					// and the user's version for modified files, so the snapshot
					// carries the user's delta onto the tip.
					//
					// The two-way merge preserves the user's modifications: for
					// a file the user changed but origin did not, the merge sees
					// the working tree already differs from HEAD (the user's
					// edit) and leaves the working tree untouched (same content
					// in both trees from the merge's perspective — the merge
					// only updates files where the tip differs from HEAD).
					// skip-worktree bits for out-of-cone paths are preserved,
					// so `add -A` stages only the user's in-cone changes and
					// `write-tree` produces a tree with the tip's out-of-cone
					// blobs. If the merge fails (an in-cone file the user
					// modified was also advanced by origin — a conflict the
					// two-way merge refuses to overwrite), abort: read-tree -m
					// -u is atomic on failure (the index and working tree are
					// unchanged), so continuing would stage on the behind index
					// and `write-tree` would produce a tree that carries the
					// user's conflicting file but reverts every unrelated file
					// origin advanced — parenting that tree to the tip would
					// silently push those reverts. Aborting preserves the
					// working tree for a retry rather than reverting origin.
					// read-tree -m -u is the preferred reconciliation: it updates
					// both the index and the working tree to the tip for files the
					// user did not modify, so the following `add -A` stages the
					// user's changes on top of the tip's tree and `write-tree`
					// produces a tree that carries the user's delta onto the tip.
					// It fails when a file the user and origin both modified would
					// be overwritten, or an untracked working tree file would be
					// overwritten by a file the tip adds — read-tree -m -u is
					// atomic on failure, so the index and working tree are
					// unchanged. In that case, fall back to applying the user's
					// delta to the tip directly (read-tree <tip> + selective add)
					// rather than staging on the behind index, which would revert
					// every unrelated file origin advanced (the behind index has
					// those files' stale HEAD blobs, and `add -A` would stage them,
					// parenting that tree to the tip and silently pushing the
					// reverts).
					//
					// Synchronize submodule checkouts after the read so a
					// dirty-behind worktree whose origin advanced an initialized
					// submodule does not stage the stale gitlink: the two-way read
					// advances the index gitlink to the tip without checking out the
					// new submodule commit (git read-tree -h documents
					// --recurse-submodules as the option controlling recursive
					// checkout updates; -u does not recurse into submodules), so the
					// staging would restage the stale checkout and push a commit
					// reverting origin's submodule advance. --recursive descends
					// into nested submodules; the update is best-effort (a failure
					// leaves the gitlink advanced, and the selective add below stages
					// only the user's files so the index keeps the tip's gitlink —
					// the worst case is a submodule at the old commit, never a
					// revert of origin's advance). runGitNetworkCommand bounds a
					// submodule fetch (the new commit may not be local).
					readTreeFailed := false
					if _, rtErr := g.runGitCommand(g.worktreePath, "read-tree", "-m", "-u", "HEAD", fetchHead); rtErr != nil {
						// read-tree -m -u failed (a file the user and origin both
						// modified, or an untracked file the tip would add).
						// Apply the user's delta to the tip instead: load the tip
						// into the index without touching the working tree
						// (read-tree without -u/-m does not fail on untracked
						// files), then stage only the files the user changed so
						// the index keeps the tip's version of every file the user
						// did not touch. `git diff --name-only HEAD` reports the
						// tracked files whose working-tree content differs from
						// HEAD (the user's modifications and deletions — the stale
						// HEAD blobs of unrelated files match the working tree, so
						// they are absent from the diff and stay at the tip's
						// version in the index); `git ls-files --others` reports
						// the user's untracked additions. Staging only those paths
						// (rather than `add -A`, which would stage the stale
						// working-tree blobs of unrelated files and revert the
						// tip's advances) builds a tree that carries the user's
						// delta onto the tip.
						readTreeFailed = true
						if _, rtErr2 := g.runGitCommand(g.worktreePath, "read-tree", fetchHead); rtErr2 != nil {
							return "", g.annotateGitWriteError(fmt.Errorf("failed to load origin's tip into the index before archive push: %w", rtErr2), probeDir)
						}
						_, _ = g.runGitNetworkCommand(g.worktreePath, "submodule", "update", "--recursive")
						changedOut, _ := g.runGitCommand(g.worktreePath, "diff", "--name-only", "-z", "HEAD")
						untrackedOut, _ := g.runGitCommand(g.worktreePath, "ls-files", "--others", "--exclude-standard", "-z")
						var addPaths []string
						for _, p := range strings.Split(changedOut, "\x00") {
							if p != "" {
								addPaths = append(addPaths, p)
							}
						}
						for _, p := range strings.Split(untrackedOut, "\x00") {
							if p != "" {
								addPaths = append(addPaths, p)
							}
						}
						if len(addPaths) > 0 {
							if _, e := g.runGitCommand(g.worktreePath, append([]string{"add", "-A", "--"}, addPaths...)...); e != nil {
								return "", g.annotateGitWriteError(fmt.Errorf("failed to stage uncommitted work before archive push: %w", e), probeDir)
							}
						}
					} else {
						// read-tree -m -u succeeded: the index and working tree are
						// updated to the tip for files the user did not modify, so
						// `add -A` stages the user's changes on top of the tip's
						// tree. Sync the submodule checkout (see above) so add -A
						// does not stage a stale gitlink and revert origin's advance.
						_, _ = g.runGitNetworkCommand(g.worktreePath, "submodule", "update", "--recursive")
						if _, e := g.runGitCommand(g.worktreePath, "add", "-A"); e != nil {
							return "", g.annotateGitWriteError(fmt.Errorf("failed to stage uncommitted work before archive push: %w", e), probeDir)
						}
					}
					treeSha, e := g.runGitCommand(g.worktreePath, "write-tree")
					if e != nil {
						return "", g.annotateGitWriteError(fmt.Errorf("failed to write snapshot tree before archive push: %w", e), probeDir)
					}
					// commit-tree does not honor commit.gpgSign the way `git
					// commit` does (git commit-tree -h exposes signing only via
					// -S). In a repo configured to sign commits whose remote
					// enforces signed pushes, an unsigned snapshot commit would
					// be rejected, wedging the retry on the same rejected
					// commit. Mirror `git commit`'s honoring of commit.gpgSign:
					// pass -S when it is set so the snapshot commit is signed the
					// same way the porcelain commit below would sign it.
					commitTreeArgs := []string{strings.TrimSpace(treeSha), "-p", fetchHead, "-m", archiveSnapshotMessage}
					if signConf, scErr := g.runGitCommand(g.worktreePath, "config", "--bool", "commit.gpgSign"); scErr == nil && strings.TrimSpace(signConf) == "true" {
						commitTreeArgs = append([]string{"-S"}, commitTreeArgs...)
					}
					commitSha, e := g.runGitCommand(g.worktreePath, append([]string{"commit-tree"}, commitTreeArgs...)...)
					if e != nil {
						return "", g.annotateGitWriteError(fmt.Errorf("failed to create snapshot commit on origin's tip before archive push: %w", e), probeDir)
					}
					// Advance HEAD onto the snapshot commit. On the read-tree -m -u
					// path, the working tree already matches the snapshot (the
					// merge updated it for files the user did not touch and the
					// selective add staged the rest), so `reset --soft` moves HEAD
					// without touching the tree and the post-reset status is clean.
					// On the read-tree failure path, the working tree was NOT
					// updated for unrelated files (the non-recursive read-tree
					// touched only the index); `reset --soft` would leave the
					// stale working-tree blobs, the post-reset status would report
					// them as dirty, and the snapshot block below would `add -A`
					// them — reverting the tip's advances on the next commit.
					// `reset --hard` updates the working tree to the snapshot (the
					// user's changes are in the snapshot, so they are not lost) and
					// leaves the tree clean (the working tree matches the snapshot).
					// It is routed through runGitLocalCommand (worktree_git.go
					// documents it as the runner destructive checkout operations
					// must use) so a stalled filesystem cannot hang the archive on
					// the checkout-producing reset. The user's changes are already
					// captured in the snapshot (the selective add staged them), so
					// the reset does not lose them; the only thing it overwrites is
					// the stale working-tree blobs of unrelated files (the ones the
					// read-tree -m -u could not update), which the snapshot already
					// advanced to the tip's version in the tree.
					if readTreeFailed {
						if out, e := g.runGitLocalCommand(g.worktreePath, "reset", "--hard", strings.TrimSpace(commitSha)); e != nil {
							return "", g.annotateGitWriteError(fmt.Errorf("failed to advance HEAD onto snapshot commit before archive push: %s: %w",
								strings.TrimSpace(out), e), probeDir)
						}
					} else {
						if out, e := g.runGitCommand(g.worktreePath, "reset", "--soft", strings.TrimSpace(commitSha)); e != nil {
							return "", g.annotateGitWriteError(fmt.Errorf("failed to advance HEAD onto snapshot commit before archive push: %s: %w",
								strings.TrimSpace(out), e), probeDir)
						}
					}
					// Re-read status after the dirty reconciliation: the reset
					// (soft or hard) leaves the tree clean (the snapshot is on HEAD
					// and the working tree matches it), so the snapshot block below
					// is a no-op and we proceed straight to the push. (The clean
					// branch deliberately skips this re-check — see the
					// submodule-gitlink note above.)
					status, err = g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
					if err != nil {
						return "", g.annotateGitWriteError(fmt.Errorf("failed to re-check worktree status after reconciling with origin: %w", err), probeDir)
					}
				}
				// else: a merge is in progress. Skip the plumbing path — the
				// snapshot block below uses `git commit --no-verify`, which
				// completes the merge with both parents, honors commit.gpgSign,
				// and clears MERGE_HEAD. The plumbing path's commit-tree would
				// create a single-parent commit and leave MERGE_HEAD, after
				// which `git reset --soft` aborts ("Cannot do a soft reset in
				// the middle of a merge") and the archive fails indefinitely.
			}
		} else if !isAncestor(fetchHead, "HEAD") {
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
