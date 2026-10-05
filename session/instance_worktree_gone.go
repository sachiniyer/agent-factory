package session

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session/git"
)

// Instance-level steps for archiving a session whose tracked worktree is no
// longer at its recorded path (#5102): the no-move archive teardown, adopting a
// move af made but never recorded, and asking git where the branch lives. The
// git-level proofs they rely on live in session/git (worktree_presence.go,
// worktree_adopt.go).

// ArchiveTeardownWorktreeGone is the archive teardown for a session whose
// tracked worktree was confirmed absent — a conclusive ENOENT with no af
// relocation outstanding (#5102). The tmux teardown is identical to
// ArchiveTeardownWithClaim's; the worktree step re-verifies the absence and
// performs no move, since there is nothing left to relocate and the branch is
// kept. trustLiveGeneration carries the same lock contract.
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
