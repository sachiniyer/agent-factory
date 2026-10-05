package session

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session/git"
)

// The archive teardown's two no-move worktree steps (#5102). Both routes reach
// handleWorktree having decided, before any pane was torn down, that there is
// nothing to relocate — so each re-establishes that decision at the use
// boundary rather than trusting an answer from before teardown.

// confirmGoneForArchive is the worktreeGone step. The daemon chose the route
// from a conclusive ENOENT taken before pane teardown, so re-confirm it here. A
// path that reappeared in the meantime has bytes to preserve and belongs to the
// moving route; an unanswerable lstat refuses closed, and stateUnknown keeps the
// record recoverable so a retry can decide again.
func confirmGoneForArchive(gw *git.GitWorktree, title string) (teardownState, error) {
	path := gw.GetWorktreePath()
	if _, statErr := git.BoundedLstat(path); statErr == nil {
		return stateKnown, fmt.Errorf("archive %q: worktree %s reappeared before the move step; refusing the missing-worktree archive route — retry to relocate it", title, path)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return stateUnknown, fmt.Errorf("archive %q: could not confirm worktree %s absent: %w", title, path, statErr)
	}
	return stateKnown, nil
}

// reconfirmAdoptedForArchive requires the adopted worktree to still be the
// directory git proved, by device and inode rather than by name. A swap or an
// unanswerable check is stateUnknown: ReconfirmAdoptedWorktree has already
// fenced the record, so recovery will not start an agent in, or clean up, what
// may be unrelated files.
func reconfirmAdoptedForArchive(gw *git.GitWorktree, title string) (teardownState, error) {
	if err := gw.ReconfirmAdoptedWorktree(); err != nil {
		if errors.Is(err, git.ErrRelocateStateUnknown) {
			return stateUnknown, fmt.Errorf("archive %q: %w", title, err)
		}
		return stateKnown, fmt.Errorf("archive %q: adopted worktree %s is no longer usable: %w", title, gw.GetWorktreePath(), err)
	}
	return stateKnown, nil
}
