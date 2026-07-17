package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
// teardownErr is whatever Instance.Kill returned, and ONLY an unknown-STATE error
// blocks — see session.TeardownStateUnknown for why the distinction is the whole
// design and not a detail.
//
// Blocking on any non-nil error was wrong and inverted the goal (#1917 round 5): a
// remote session whose sandbox teardown SUCCEEDED but whose in-sandbox /kill call
// failed still returns an error, and refusing on it made the finisher retry a dead
// endpoint forever against a workspace that no longer exists — the tombstone could
// never clear, not even by explicit retry, until a daemon restart reloaded an inert
// backend. Safe-by-default must not become stuck-by-default.
//
// A refusal is returned as an ERROR wrapping the teardown's own cause, not as a
// quiet (false, nil): callers must not read a refusal as "nothing to delete". That
// distinction is what arms reapDeadRoot's backoff and what makes KillSession report
// something actionable rather than success.
func (m *Manager) deleteSessionRecord(repoID, title, stableID string, teardownErr error) (bool, error) {
	if session.TeardownStateUnknown(teardownErr) {
		return false, fmt.Errorf("refusing to delete the record for session %q: its teardown did not complete safely, so its workspace is still on disk and this record is the only handle left on it: %w", title, teardownErr)
	}
	if teardownErr != nil {
		// The teardown TOLD us something went wrong, but not that the workspace's
		// fate is unknown. The record may go; the error is the caller's to report.
		log.WarningLog.Printf("session %q: teardown reported an error that does not leave its workspace state unknown; deleting the record as normal: %v", title, teardownErr)
	}
	storage, err := session.NewStorage(config.LoadState(), repoID)
	if err != nil {
		return false, err
	}
	return storage.DeleteInstanceByStableID(title, stableID)
}
