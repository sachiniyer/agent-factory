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
	// Reconcile with origin BEFORE the snapshot commit (#5407). A HEAD that is an
	// ancestor of origin's session branch (an agent ran `git reset --hard HEAD~1`
	// or checked out an older commit) would otherwise be rejected as a
	// non-fast-forward push AFTER the snapshot commit already landed locally,
	// stranding it inside a sandbox that is about to be reaped. Behind-only is
	// recoverable: nothing on origin would be overwritten.
	behind, diverged := g.classifyAgainstOrigin(branch)
	if diverged {
		// Neither side contains the other: the plain push below can only be
		// rejected. Refuse BEFORE snapshotting so the uncommitted work stays in the
		// tree for the operator's retry rather than becoming a stranded local
		// commit that later retries skip over.
		return "", g.annotateGitWriteError(fmt.Errorf("HEAD has diverged from origin's %q (neither is an ancestor of the other), so the archive push would be rejected; nothing was committed — reconcile the branches in the sandbox and retry", branch), probeDir)
	}
	if behind {
		if clean, err := g.isTreeClean(); err == nil && clean {
			// Origin already holds every commit HEAD has: nothing to push.
			return branch, nil
		}
		// Dirty: fast-forward onto origin's tip, carrying the working tree, so the
		// snapshot lands on top of it. If the uncommitted changes overlap the
		// incoming ones git refuses. Autostash is disabled explicitly: a user's
		// merge.autoStash=true would otherwise save the local edits onto refs/stash
		// on a conflicting re-apply, and refs/stash is ONE stack shared by every
		// linked worktree of the repo, so af must never write to (or pop from) it.
		if _, err := g.runGitCommand(g.worktreePath, "-c", "merge.autoStash=false", "-c", "rebase.autoStash=false", "merge", "--ff-only", "--no-autostash", "FETCH_HEAD"); err != nil {
			return "", g.annotateGitWriteError(fmt.Errorf("HEAD is behind origin's %q and uncommitted changes overlap the incoming commits, so it cannot be fast-forwarded before the archive snapshot (nothing was committed; resolve in the sandbox and retry): %w", branch, err), probeDir)
		}
	}

	status, err := g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", g.annotateGitWriteError(fmt.Errorf("failed to check worktree status before archive push: %w", err), probeDir)
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

// classifyAgainstOrigin fetches origin's session branch (best effort, under the
// network timeout) and compares HEAD to it. behind is true when HEAD is a strict
// ancestor of origin's tip; diverged when neither is an ancestor of the other.
// Any failure to fetch or resolve (no such branch on origin yet, unreachable
// origin) reports neither, leaving the push to surface its own error.
func (g *GitWorktree) classifyAgainstOrigin(branch string) (behind, diverged bool) {
	if _, err := g.runGitNetworkCommand(g.worktreePath, "fetch", "origin", "refs/heads/"+branch); err != nil {
		return false, false
	}
	head, err := g.runGitCommand(g.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return false, false
	}
	tip, err := g.runGitCommand(g.worktreePath, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return false, false
	}
	head, tip = strings.TrimSpace(head), strings.TrimSpace(tip)
	if head == tip {
		return false, false
	}
	if _, err := g.runGitCommand(g.worktreePath, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD"); err == nil {
		return true, false
	}
	if _, err := g.runGitCommand(g.worktreePath, "merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD"); err == nil {
		return false, false // ahead: a plain fast-forward push
	}
	return false, true
}

// isTreeClean reports whether the worktree has nothing to snapshot, using the
// same config-independent status gate as the snapshot itself (#2101).
func (g *GitWorktree) isTreeClean() (bool, error) {
	status, err := g.runGitCommand(g.worktreePath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(status) == "", nil
}
