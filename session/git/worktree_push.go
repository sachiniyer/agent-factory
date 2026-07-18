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
// Phase 4 PR6) — the archive-side primitive for the disposable sandbox backends
// (docker/ssh). Because a sandbox is thrown away on archive and re-cloned from
// GitHub on restore, the branch on origin IS the durable workspace (epic
// decision 4), so archive pushes it there before the sandbox is torn down.
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
func (g *GitWorktree) SnapshotAndPushBranch() (string, error) {
	if g.externalWorktree {
		return "", fmt.Errorf("cannot push an in-place/external worktree branch (it is user-owned)")
	}
	branch := strings.TrimSpace(g.branchName)
	if branch == "" {
		return "", fmt.Errorf("cannot push worktree %s: branch name is empty", g.worktreePath)
	}

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
		return "", fmt.Errorf("failed to check worktree status before archive push: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		if _, err := g.runGitCommand(g.worktreePath, "add", "-A"); err != nil {
			return "", fmt.Errorf("failed to stage uncommitted work before archive push: %w", err)
		}
		if _, err := g.runGitCommand(g.worktreePath, "commit", "--no-verify", "-m", archiveSnapshotMessage); err != nil {
			return "", fmt.Errorf("failed to snapshot uncommitted work before archive push: %w", err)
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
		return "", fmt.Errorf("failed to push branch %q to origin (archive stores durable state on GitHub): %s: %w",
			branch, strings.TrimSpace(out), err)
	}
	return branch, nil
}
