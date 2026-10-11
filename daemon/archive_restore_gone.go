package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// rebuildAbsentArchivedWorktree is the restore route for an archived session
// whose worktree is conclusively absent with no relocation record behind it —
// the row the no-move archive commits for a worktree deleted outside af, or an
// archive directory deleted after the archive (#5102). Restoring it used to
// refuse at the relocation claim forever, though the branch was kept: every
// session archived through the deletion route was a dead end.
//
// There are no bytes to move back, so no claim is taken. The caller still holds
// the op-lock, the restore claim, the OpRestoring fence and create's worktree
// admission, so the destination chosen here cannot be raced by a create. What
// is checked before anything is written:
//
//   - The origin repo must be present: the branch lives there.
//   - git must not have the branch checked out at a live path. A user's own
//     `git worktree move` of the archive leaves exactly this footprint, and so
//     does an earlier restore whose move landed before the daemon could record
//     it; either way the session's bytes are intact somewhere, and rebuilding
//     a second checkout would fail on the branch anyway.
//
// Then the record is re-aimed at the same destination the moving route would
// pick and the worktree is re-created there from the branch. A failure leaves
// the row archived with its branch, and its state is persisted before
// returning. On success the caller's shared tail commits and re-spawns.
//
// A crash between the rebuild and that commit leaves disk naming the old,
// absent path while git registers the branch at the rebuilt one; the retry then
// lands on the live-checkout refusal above, which names both paths. That loses
// nothing, but it is a refusal rather than a resume.
func (m *Manager) rebuildAbsentArchivedWorktree(repoID, title string, instance *session.Instance) error {
	source := instance.GetWorktreePath()
	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return fmt.Errorf("cannot restore session %q: no repo path on record", title)
	}
	if err := sessiongit.CheckRepoPresentForRelocation(repoPath); err != nil {
		if errors.Is(err, sessiongit.ErrRepoGone) {
			return fmt.Errorf(
				"cannot restore session %q: its archived worktree %s is gone and so is its origin repo %s, so there is no branch to rebuild it from",
				title, source, repoPath)
		}
		return fmt.Errorf("cannot restore session %q: cannot establish origin repo state for %s: %w", title, repoPath, err)
	}
	moved, live, err := instance.LiveBranchCheckout()
	if err != nil {
		return fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, and af could not read where git has its branch checked out, so it cannot tell a deletion from a move: %w",
			title, source, err)
	}
	if live {
		return fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, but git has its branch checked out at %s (moved outside af, or placed by an earlier restore that was cut off before it could record it) — move it back to %s and retry, or remove it with 'git worktree remove' and retry to rebuild it from the branch",
			title, source, moved, source)
	}
	beforeRestoreWorktreePath()
	dest, err := sessiongit.RestoreWorktreePath(repoPath, title, instance.GetBranch())
	if err != nil {
		return fmt.Errorf("cannot determine restore location for %q: %w", title, err)
	}
	if err := instance.RebuildAbsentArchivedWorktree(dest); err != nil {
		wrapped := fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, and rebuilding it from branch %s at %s failed; the session stays archived with its branch kept: %w",
			title, source, instance.GetBranch(), dest, err)
		if perr := m.persistInstanceErr(repoID, instance); perr != nil {
			return errors.Join(wrapped, fmt.Errorf("could not persist the archived row: %w", perr))
		}
		return wrapped
	}
	m.info().Printf("restore of session %q: archived worktree %s was gone; rebuilt it from branch %s at %s", title, source, instance.GetBranch(), dest)
	return nil
}
