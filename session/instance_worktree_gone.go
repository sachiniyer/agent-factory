package session

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session/git"
)

// Instance-level steps for a tracked worktree that is no longer at its recorded
// path (#5102): the no-move archive teardown, adopting a move af made but never
// recorded, and re-aiming the record for a restore that rebuilds from the
// branch. The git-level proofs they rely on live in session/git
// (worktree_presence.go, worktree_adopt.go).

// ArchiveTeardownWorktreeGone is the archive teardown for a session whose
// tracked worktree was confirmed absent — a conclusive ENOENT with no af
// relocation outstanding (#5102). The tmux teardown is identical to
// ArchiveTeardownWithClaim's; the worktree step re-verifies the absence and
// performs no move, since there is nothing left to relocate and the branch is
// what restore rebuilds from. trustLiveGeneration carries the same lock contract.
//
// adopted selects the sibling route for a previous archive whose move landed
// before the daemon could record it: the record already names the archived
// location, and the worktree step re-verifies it is still present instead.
func (i *Instance) ArchiveTeardownWorktreeGone(beforeMove func() error, trustLiveGeneration, adopted bool) (hookErr, archiveErr error) {
	mode := teardownArchive{
		worktreeGone: !adopted, worktreeAdopted: adopted,
		beforeMove: beforeMove, hookErr: &hookErr, trustLiveGeneration: trustLiveGeneration,
	}
	archiveErr = i.teardownTabs(mode)
	return hookErr, archiveErr
}

// AdoptLandedArchiveMove re-aims an archiving session's record at dest once git
// proves an earlier archive's move landed there (#5102); see
// git.AdoptLandedWorktreeMove. The caller holds the OpArchiving fence.
func (i *Instance) AdoptLandedArchiveMove(dest string) error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot archive %q: instance has no worktree", i.Title)
	}
	return gw.AdoptLandedWorktreeMove(dest)
}

// LiveBranchCheckout reports where git has this session's branch checked out
// at a path that exists right now. live is false when git registers the branch
// nowhere, or only at a path that no longer exists — the stale, prunable entry
// a deletion outside af leaves behind. An error means the listing or the path
// could not be read, and nothing may be concluded from it.
func (i *Instance) LiveBranchCheckout() (path string, live bool, err error) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return "", false, fmt.Errorf("session %q has no worktree", i.Title)
	}
	return liveBranchCheckout(gw)
}

func liveBranchCheckout(gw *git.GitWorktree) (string, bool, error) {
	registered, listed, err := gw.RegisteredPathForBranch()
	if err != nil || !listed {
		return "", false, err
	}
	if _, statErr := git.BoundedLstat(registered); errors.Is(statErr, os.ErrNotExist) {
		return "", false, nil
	} else if statErr != nil {
		return "", false, fmt.Errorf("cannot inspect %s, where git registers this session's branch: %w", registered, statErr)
	}
	return registered, true, nil
}

// ReconfirmAdoptedWorktreeLocation re-checks, by device and inode, that the
// directory an adoption proved is still the one at the recorded path; see
// git.ReconfirmAdoptedWorktree. Restore calls it immediately before the respawn
// launches an agent there (#5102).
func (i *Instance) ReconfirmAdoptedWorktreeLocation() error {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("session %q has no worktree", i.Title)
	}
	return gw.ReconfirmAdoptedWorktree()
}

// AdoptLandedRestoreMove is restore's counterpart: when the archived worktree
// is gone, it asks git where this session's branch is checked out. A registered
// location that exists on disk, at a path restore itself could have chosen, is
// where an earlier restore's move landed before the daemon could record it, and
// the record is re-aimed there after the same proof (#5102). adopted is false
// when git registers the branch nowhere that exists — including the stale,
// prunable entry an outside deletion leaves — which is the genuine-deletion case
// the caller rebuilds from the branch. A live checkout of the branch anywhere
// else is the user's, not af's: adopting it would hand it to kill's cleanup, and
// rebuilding past it would fail on git's one-checkout rule, so that refuses.
func (i *Instance) AdoptLandedRestoreMove(repoPath, title, branch string) (path string, adopted bool, err error) {
	i.mu.RLock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced); err != nil {
		i.mu.RUnlock()
		return "", false, err
	}
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return "", false, fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	registered, live, err := liveBranchCheckout(gw)
	if err != nil || !live {
		return "", false, err
	}
	placement, err := git.IsRestorePlacement(repoPath, title, branch, registered)
	if err != nil {
		return "", false, fmt.Errorf("cannot tell whether %s, where git has this session's branch checked out, is af's restore location: %w", registered, err)
	}
	if !placement {
		return "", false, fmt.Errorf("git has this session's branch checked out at %s, which is not a location af restores to, so af will not adopt it — remove that checkout (git worktree remove) to let restore rebuild the session's worktree, or kill the session", registered)
	}
	if err := gw.AdoptLandedWorktreeMove(registered); err != nil {
		return "", false, fmt.Errorf("git registers this session's branch at %s, but it could not be adopted as the restored worktree: %w", registered, err)
	}
	return registered, true, nil
}

// RepointAbsentWorktreeForRestore is the restore-side step for an archived row
// whose worktree was deleted outside af (#5102): there is nothing to move back,
// so the record is re-aimed at dest and the respawn's RebuildFromExistingBranch
// recreates the worktree there from the kept branch. It requires the held
// restore fence, like RestoreArchivedWorktreeHeldFencedWithClaim, and has no
// claim to preserve on refusal because the gone route never took one.
func (i *Instance) RepointAbsentWorktreeForRestore(dest string) error {
	i.mu.RLock()
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionRestoreArchivedFenced); err != nil {
		i.mu.RUnlock()
		return err
	}
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return fmt.Errorf("cannot restore %q: instance has no worktree", i.Title)
	}
	return gw.RepointAbsentWorktreePath(dest)
}
