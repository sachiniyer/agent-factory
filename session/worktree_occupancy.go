package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// CheckWorktreeOccupants reports processes still working inside a workspace that
// is about to be deleted or moved, for a teardown whose marker evidence was
// BLIND (#2998).
//
// # When it applies
//
// Only after a session vanished with no pane ever observed. There, tmux has
// forgotten the ancestry and the AF_SESSION scan is the whole evidence — and that
// scan is vacuous for a session that never exported a marker (tmux < 3.2, or a
// pre-marker build), reporting the same empty result whether a descendant
// escaped or not. A cwd inside the workspace needs no marker and is the one
// signal that still works there.
//
// # Call it ONCE, after every tab is closed
//
// Tabs of one instance share a worktree, so a scan run while a sibling is still
// live reports that sibling as an occupant and refuses a teardown that was about
// to close it anyway. Both callers run this after their close loop, never inside
// it.
//
// # It reports; it never kills
//
// A match proves OCCUPANCY, not ownership: an operator's shell in the worktree is
// indistinguishable from an escaped agent child. The error names the pids and
// leaves the workspace intact and the record retryable, so the decision stays
// with the operator.
//
// # An unreadable process table is UNSAFE, not empty
//
// It returns the error. On this branch the marker sweep already refuses when the
// process table cannot be read, so swallowing it here would make the newer check
// weaker than the one it supplements — and would let a workspace be deleted on a
// check that never ran. An empty path means no workspace is in play and is the
// one silent nil.
func CheckWorktreeOccupants(worktreePath string) error {
	if worktreePath == "" {
		return nil
	}
	occupants, err := proctree.OccupantsOfDir(worktreePath)
	if err != nil {
		return fmt.Errorf("cannot establish whether anything is still working inside %s, "+
			"so it is not safe to remove: %w", worktreePath, err)
	}
	if len(occupants) == 0 {
		return nil
	}
	return fmt.Errorf("%d process(es) are still working inside %s — %s; they carry no agent-factory "+
		"marker, so ownership cannot be proven and they are reported rather than killed",
		len(occupants), worktreePath, proctree.DescribeOccupants(occupants))
}
