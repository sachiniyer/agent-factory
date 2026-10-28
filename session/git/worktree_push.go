package git

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// archiveSnapshotMessage labels the WIP commit SnapshotAndPushBranch makes for
// uncommitted work at archive time. It is deliberately recognizable so a user
// who restores the session sees why the extra commit exists.
const archiveSnapshotMessage = "af: pre-archive snapshot (uncommitted work)"

// archiveMergeMessage labels the merge commit archive makes when the sandbox HEAD
// and origin's session branch diverged but combine cleanly (#5407).
const archiveMergeMessage = "af: merge origin's session branch into the pre-archive snapshot"

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
// push is a plain (non-force) push, so origin's history is never overwritten:
// when HEAD and origin's branch have diverged (or HEAD sits behind it and just
// took a snapshot), the two are merged in memory and the merge is pushed as a
// fast-forward, or — on conflicts — HEAD goes to a side branch whose name is
// returned for restore to clone (publishArchiveHead, #5407).
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

	// The snapshot is captured: HEAD now carries the work exactly as the worktree
	// holds it under its CURRENT ignore rules. Everything below only reads and
	// publishes commits — the working tree and index are never rewritten
	// (#5407), because for a sandbox the workspace is destroyed after archive and
	// anything not in the pushed commits is lost.
	return g.publishArchiveHead(branch, probeDir)
}

// publishArchiveHead makes HEAD durable on origin without touching the working
// tree, classifying it against origin's current session branch (#5407):
//
//   - origin's branch missing or unreachable, or HEAD ahead of it: a plain
//     fast-forward push of HEAD (the pre-existing path; the push reports its own
//     error, with git's stderr).
//   - HEAD already contained in origin's branch (it sits behind, nothing to
//     snapshot): origin holds every commit, so there is nothing to push, and the
//     branch must not move backwards.
//   - diverged (this includes a behind HEAD that just took a snapshot commit): if
//     HEAD and origin's tip combine cleanly in memory (git merge-tree, no
//     worktree), push that merge as a fast-forward of the session branch;
//     otherwise push HEAD to a side branch (<branch>-af-archive-<utc timestamp>)
//     that nothing else writes, and return THAT name — the caller records the
//     returned branch as the one restore clones back, so the work is found.
//     Only a failing side push fails the archive.
func (g *GitWorktree) publishArchiveHead(branch, probeDir string) (string, error) {
	push := func(src, dst string) error {
		refspec := src + ":refs/heads/" + dst
		if out, err := g.runGitNetworkCommand(g.worktreePath, "push", "origin", refspec); err != nil {
			return g.annotateGitWriteError(fmt.Errorf("failed to push branch %q to origin (archive stores durable state on GitHub): %s: %w",
				dst, strings.TrimSpace(out), err), probeDir)
		}
		return nil
	}

	head, tip := g.revParse("HEAD"), g.fetchOriginTip(branch)
	switch {
	case head == "" || tip == "" || head == tip || g.isAncestor(tip, head):
		// Push HEAD itself so the actual working-tree state is what becomes durable
		// even if the agent left HEAD on a different branch or detached (#1721).
		if err := push("HEAD", branch); err != nil {
			return "", err
		}
		return branch, nil
	case g.isAncestor(head, tip):
		return branch, nil
	}

	if merged, ok := g.mergeCommitWithoutWorktree(head, tip); ok {
		if err := push(merged, branch); err != nil {
			return "", err
		}
		return branch, nil
	}
	side := fmt.Sprintf("%s-af-archive-%s", branch, time.Now().UTC().Format("20060102T150405Z"))
	if err := push("HEAD", side); err != nil {
		return "", err
	}
	log.WarningLog.Printf("archive of %q: HEAD cannot be combined with origin's branch without conflicts; the work was pushed to %q instead and the session will restore from it", branch, side)
	return side, nil
}

func (g *GitWorktree) revParse(rev string) string {
	out, err := g.runGitCommand(g.worktreePath, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// fetchOriginTip fetches origin's session branch (best effort, under the network
// timeout) and returns its tip, or "" when it cannot be fetched or resolved.
// Fetching writes only objects and FETCH_HEAD, never the worktree.
func (g *GitWorktree) fetchOriginTip(branch string) string {
	if _, err := g.runGitNetworkCommand(g.worktreePath, "fetch", "origin", "refs/heads/"+branch); err != nil {
		return ""
	}
	return g.revParse("FETCH_HEAD")
}

func (g *GitWorktree) isAncestor(ancestor, descendant string) bool {
	_, err := g.runGitCommand(g.worktreePath, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// mergeCommitWithoutWorktree merges head and tip entirely in the object
// database (git merge-tree --write-tree, git >= 2.38) and returns a merge commit
// with parents head and tip, or ok=false on conflicts, on an older git, or on any
// error. The working tree and index are not read or written.
func (g *GitWorktree) mergeCommitWithoutWorktree(head, tip string) (string, bool) {
	out, err := g.runGitCommand(g.worktreePath, "merge-tree", "--write-tree", head, tip)
	if err != nil {
		return "", false
	}
	tree, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return "", false
	}
	commit, err := g.runGitCommand(g.worktreePath, "commit-tree", tree, "-p", head, "-p", tip, "-m", archiveMergeMessage)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(commit), true
}
