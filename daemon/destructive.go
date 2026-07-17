package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The one choke point for deleting a session's record (#1917).
//
// Deleting the record is the LAST destructive act of a kill and the most
// consequential: the record is the only handle the user — and the daemon's own
// retry — has on a session's tmux and worktree. Drop it while the teardown's
// outcome is unknown and the workspace is orphaned with nothing pointing at it,
// permanently.
//
// This existed three times (KillSession, finishUserKill, reapDeadRoot), each
// hand-gating the same rule, and the review found one of them ungated every round
// — reapDeadRoot was still log-and-delete after two "exhaustive" audits. The gate
// is not something three call sites should each remember. Routing them through one
// function means a fourth caller cannot forget, because there is nowhere else to
// call.
//
// teardownErr is whatever Instance.Kill returned. Kill already swallows everything
// tmux and git ANSWER for (see teardownKill), so a non-nil error here means the
// teardown could not complete SAFELY — a pane whose liveness tmux never confirmed,
// or a worktree removal cut off mid-delete. Both mean the workspace is still there.
//
// Returns deleted=false with a nil error when the delete was correctly skipped, so
// callers can distinguish "refused" from "failed".
func (m *Manager) deleteSessionRecord(repoID, title, stableID string, teardownErr error) (bool, error) {
	if teardownErr != nil {
		return false, fmt.Errorf("refusing to delete the record for session %q: its teardown did not complete safely, so its workspace is still on disk and this record is the only handle left on it: %w", title, teardownErr)
	}
	storage, err := session.NewStorage(config.LoadState(), repoID)
	if err != nil {
		return false, err
	}
	return storage.DeleteInstanceByStableID(title, stableID)
}
