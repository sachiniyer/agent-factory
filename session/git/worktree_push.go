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
	status, err := g.runGitCommand(g.worktreePath, "status", "--porcelain")
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

	// Push the branch to origin under the network timeout. A "src refspec ..."
	// or auth failure surfaces here with git's stderr folded in.
	if out, err := g.runGitNetworkCommand(g.worktreePath, "push", "origin", branch); err != nil {
		return "", fmt.Errorf("failed to push branch %q to origin (archive stores durable state on GitHub): %s: %w",
			branch, strings.TrimSpace(out), err)
	}
	return branch, nil
}
