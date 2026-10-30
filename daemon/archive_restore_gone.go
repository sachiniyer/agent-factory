package daemon

import (
	"errors"
	"fmt"
	"os"

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
//   - git must not have the branch checked out at a live path other than the
//     one this restore would rebuild at. A user's own `git worktree move` of
//     the archive leaves exactly this footprint, and so does an earlier
//     restore whose move landed before the daemon could record it; either way
//     the session's bytes are intact somewhere, and rebuilding a second
//     checkout would fail on the branch anyway.
//
// Then the record is re-aimed at the same destination the moving route would
// pick and the worktree is re-created there from the branch. A failure leaves
// the row archived with its branch, and its state is persisted before
// returning. On success the caller's shared tail commits and re-spawns.
//
// The one live checkout that is not refused is this route's own. A daemon that
// dies after the rebuild and before the caller's first persist leaves disk
// naming the old, absent path while git registers the branch at the rebuilt
// destination. Refusing that as a move made af's own rebuild unrestorable
// without git surgery. The recognition keys on the PREFERRED placement
// (RestoreWorktreeBasePath), not on RestoreWorktreePath's answer: the latter
// steps past an occupied path, and on the retry the occupant is the crashed
// rebuild itself, so it would name <preferred>-2 and never match. The
// preferred path is a pure function of the repo, title and branch, so the
// retry derives exactly what the crashed restore did whenever that restore
// found it free. When git has the branch there, the retry adopts it with the
// proof archive uses for its own landed move (AdoptRebuiltArchivedWorktree):
// git's registration on this branch plus the directory's back-binding, the
// recorded path re-proved absent and the directory's identity pinned under the
// relocation lock. Whether af rebuilt it or the user moved the archive to that
// exact path, what is proven is the session's own checkout.
//
// Only the exact preferred path adopts. A checkout at <preferred>-N could be a
// restore that crashed while the preferred path was occupied, but nothing ties
// the suffix to this row rather than to the user, so it refuses like any other
// live path. The comparison resolves symlinks (git reports resolved paths; the
// configured placement may go through one, as macOS's temp dir does) with the
// bounded normalization the worktree listing uses, and a normalization that
// cannot answer refuses rather than guessing.
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
	beforeRestoreWorktreePath()
	preferred, err := sessiongit.RestoreWorktreeBasePath(repoPath, title, instance.GetBranch())
	if err != nil {
		return fmt.Errorf("cannot determine restore location for %q: %w", title, err)
	}
	dest, err := sessiongit.RestoreWorktreePath(repoPath, title, instance.GetBranch())
	if err != nil {
		return fmt.Errorf("cannot determine restore location for %q: %w", title, err)
	}
	moved, live, err := instance.LiveBranchCheckout()
	if err != nil {
		return fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, and af could not read where git has its branch checked out, so it cannot tell a deletion from a move: %w",
			title, source, err)
	}
	if live {
		ownRebuild, err := sessiongit.SameWorktreePath(moved, preferred)
		if err != nil {
			return fmt.Errorf(
				"cannot restore session %q: its archived worktree %s is gone, and git has its branch checked out at %s, but af could not compare that path with its restore location %s, so it cannot tell its own earlier rebuild from a move: %w",
				title, source, moved, preferred, err)
		}
		if ownRebuild {
			return m.adoptRebuiltArchivedWorktree(repoID, title, instance, source, moved, preferred)
		}
		return fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, but git has its branch checked out at %s (moved outside af, or placed by an earlier restore that was cut off before it could record it) — move it back to %s and retry, or remove it with 'git worktree remove' and retry to rebuild it from the branch",
			title, source, moved, source)
	}
	// Nothing to rebuild from is a fact to report, not a rebuild to attempt: the
	// archive may have recorded that the branch was deleted too, and either way
	// the branch is re-checked here, since the user may have recreated it. A
	// probe that cannot answer falls through to the rebuild, which fails on its
	// own terms if the branch is in fact gone.
	if branch, exists, berr := instance.RecordedBranchExists(); berr == nil && !exists {
		return fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone and its branch %s no longer exists either, so there is nothing to rebuild it from; the session stays archived — recreate the branch (for example 'git branch %s <commit>') and retry",
			title, source, branch, branch)
	}
	if err := instance.RebuildAbsentArchivedWorktree(dest); err != nil {
		wrapped := fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, and rebuilding it from branch %s at %s failed; the session stays archived with its branch kept: %w",
			title, source, instance.GetBranch(), dest, err)
		if perr := m.persistUnderAdmission(repoID, instance); perr != nil {
			return errors.Join(wrapped, fmt.Errorf("could not persist the archived row: %w", perr))
		}
		return wrapped
	}
	m.info().Printf("restore of session %q: archived worktree %s was gone; rebuilt it from branch %s at %s", title, source, instance.GetBranch(), dest)
	afterRestoreWorktreePlaced()
	return nil
}

// afterRestoreWorktreePlaced is a test seam that runs once a rebuild or
// adoption has proved and pinned the restored directory, before the restore
// tail reconfirms it. Production never reassigns it.
var afterRestoreWorktreePlaced = func() {}

// adoptRebuiltArchivedWorktree re-aims the restoring row at preferred, where git
// has its branch checked out, once the adoption proof holds; see
// rebuildAbsentArchivedWorktree. The record takes af's own spelling of the
// path, the one the crashed rebuild would have recorded; moved is git's, used
// only to name the checkout in the refusal.
func (m *Manager) adoptRebuiltArchivedWorktree(repoID, title string, instance *session.Instance, source, moved, preferred string) error {
	if err := instance.AdoptRebuiltArchivedWorktree(preferred); err != nil {
		wrapped := fmt.Errorf(
			"cannot restore session %q: its archived worktree %s is gone, and git has its branch checked out at %s, where restore rebuilds it, but af could not prove that checkout is this session's own (inspect %s; if it is the session's work, retry, otherwise remove it with 'git worktree remove' and retry to rebuild it from the branch): %w",
			title, source, moved, moved, err)
		if perr := m.persistUnderAdmission(repoID, instance); perr != nil {
			return errors.Join(wrapped, fmt.Errorf("could not persist the archived row: %w", perr))
		}
		return wrapped
	}
	m.info().Printf("restore of session %q: archived worktree %s was gone; adopted the rebuild of branch %s an earlier restore left at %s", title, source, instance.GetBranch(), preferred)
	afterRestoreWorktreePlaced()
	return nil
}

// reconfirmPlacedRestoreWorktree re-checks, at a restore boundary, that the
// directory a rebuild or adoption proved is still the one at the recorded path
// (#5102). On a mismatch the git layer has already fenced the record; a
// directory that vanished is recorded as missing again. Either way the row is
// persisted before the refusal is returned, so the fence survives a restart.
func (m *Manager) reconfirmPlacedRestoreWorktree(repoID, title string, instance *session.Instance, boundary string) error {
	err := instance.ReconfirmPlacedWorktree()
	if err == nil {
		return nil
	}
	path := instance.GetWorktreePath()
	if errors.Is(err, os.ErrNotExist) {
		instance.SetWorktreeMissing(session.WorktreeMissingDeletedReason(path))
	}
	refusal := fmt.Errorf(
		"cannot restore session %q: the worktree af placed at %s is no longer the directory it verified there, so af stopped %s; the record is fenced and af will not start an agent in or remove that path — inspect %s, then retry: %w",
		title, path, boundary, path, err)
	if perr := m.persistUnderAdmission(repoID, instance); perr != nil {
		return errors.Join(refusal, fmt.Errorf("could not persist its worktree fence: %w", perr))
	}
	return refusal
}
