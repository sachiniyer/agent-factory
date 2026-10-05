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
// from a conclusive ENOENT taken before pane teardown, so re-confirm it here.
//
// Anything but a fresh ENOENT is stateUnknown AND a fenced record. A path that
// reappeared holds a directory nobody has verified; an unanswerable lstat holds
// who knows what. Either way the daemon's failure path drops the row to Lost,
// and Lost recovery would otherwise respawn the agent into that directory the
// moment it sees a path that exists. FenceUnverifiedWorktree installs the
// identity-unknown stalled record respawn and cleanup already refuse on, so
// nothing starts there until a retried archive re-resolves the path and decides
// again (#5102).
func confirmGoneForArchive(gw *git.GitWorktree, title string) (teardownState, error) {
	path := gw.GetWorktreePath()
	_, statErr := git.BoundedLstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return stateKnown, nil
	}
	var cause error
	if statErr == nil {
		cause = fmt.Errorf("worktree %s reappeared before the move step; refusing the missing-worktree archive route", path)
	} else {
		cause = fmt.Errorf("could not confirm worktree %s absent: %w", path, statErr)
	}
	return stateUnknown, fmt.Errorf("archive %q: %w", title, gw.FenceUnverifiedWorktree(cause))
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
