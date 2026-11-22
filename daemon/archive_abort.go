package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// abortRequestedArchiveToLost rolls a REQUESTED archive whose teardown did not
// finish back to Lost, and fences the automatic restore loop off the row
// (#7752).
//
// AbortArchiveToLost alone hands the row to the Lost-restore loop, which
// re-spawns the agent in place on its next tick. That answers the wrong
// question: the user asked for this session to STOP. A self-archive whose
// teardown refused — the requester itself read as a survivor (#7752) — came
// back about a second later, running the agent the archive was meant to
// shelve. The durable give-up is the same terminal field the loop writes when
// its own budget runs out, so lostSessionWantsRestore refuses it, it survives
// a daemon restart, and the row names the failure.
//
// It is a fence, not a tombstone. A status poll that observes this same
// runtime alive clears it (the kill never took, so nothing was torn down), an
// explicit restore clears it, and BeginArchive still admits a Lost row with no
// op in flight, so retrying the archive is the way out.
//
// The caller persists afterwards with its own error handling; the fence rides
// that write. Only a REQUESTED archive's refused teardown or rollback belongs
// here: a CancelArchive site never touched the runtime.
func (m *Manager) abortRequestedArchiveToLost(instance *session.Instance, teardownErr error) {
	_ = instance.Transition(session.AbortArchiveToLost())
	m.fenceRequestedArchiveRestore(instance, teardownErr)
}

// fenceRequestedArchiveRestore installs the restore-loop fence on a row a
// REQUESTED archive left Lost (#7752, #7754): the archive's own teardown
// failure above, and undoCommittedArchive's rollback of a committed archive
// the user asked for. Either way the row must never re-spawn the agent the
// user asked to stop — not on the next loop tick, and not after a daemon
// restart.
//
// The kind marks the durable failure as an archive refusal rather than the
// restore loop's own give-up so the row and API detail can say what actually
// happened; the gate itself is unchanged (attempts+error still make
// LostRestoreGaveUp), which is what keeps an older daemon — one that never
// heard of the kind — reading the row as fenced.
//
// As with the abort above this is a fence, not a tombstone: observing the
// same runtime alive clears it, an explicit restore clears it, and retrying
// the archive is the way out.
func (m *Manager) fenceRequestedArchiveRestore(instance *session.Instance, cause error) {
	if instance.SetLostRestoreFailureKind(1, fmt.Errorf("archive did not finish — retry the archive, or restore the session to resume it: %w", cause), session.LostRestoreFailureArchiveRefused) {
		m.info().Printf("archive of session %q did not finish; automatic restore is fenced off it until the archive is retried or the session is restored", instance.Title)
	}
}

// archiveAbortedDetail is the parenthetical every refused-archive error carries
// in place of the old "its agent will be restored in place": since #7752 the
// loop does not restore it, and the caller's next move is one of these two.
const archiveAbortedDetail = "it stays Lost and will not restart on its own — retry the archive, or restore the session to resume it"
