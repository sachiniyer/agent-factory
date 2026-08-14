package git

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// cleanupWorktreeStat is a seam for proving that a known-stalled workspace is
// rejected before Cleanup touches its path. Production uses a bounded Lstat so
// a final-component symlink is inspected, never followed onto a stalled mount.
var cleanupWorktreeStat = BoundedLstat

// probeCleanupWorktreePath makes Cleanup's initial existence check three-valued
// and no-follow. An unresponsive FUSE/NFS target behind a final-component
// symlink must not wedge Cleanup while the caller holds the session operation
// lock, and an inconclusive deadline is not absence.
func (r *cleanupRun) probeCleanupWorktreePath() (bool, error) {
	info, statErr := cleanupWorktreeStat(r.g.worktreePath)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return true, nil
		}
		r.unknown = true
		refusal := fmt.Errorf(
			"refusing to inspect or clean up worktree %s: the final path component is a symbolic link; leaving it and the session record in place",
			r.g.worktreePath,
		)
		r.errs = append(r.errs, refusal)
		return false, errors.Join(r.errs...)
	}

	if errors.Is(statErr, context.DeadlineExceeded) {
		r.unknown = true
		refusal := fmt.Errorf(
			"cannot establish whether worktree path %s exists: %w",
			r.g.worktreePath, statErr,
		)
		r.errs = append(r.errs, refusal)
		return false, errors.Join(r.errs...)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		// Preserve Cleanup's answered-error behavior: callers decide whether a
		// later bounded Git answer or the archived-path postcondition settles
		// the operation. Only a probe deadline is unknown at this boundary.
		r.errs = append(r.errs, fmt.Errorf("failed to check worktree path: %w", statErr))
	}
	return false, nil
}
