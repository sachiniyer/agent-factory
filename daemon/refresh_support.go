package daemon

import "github.com/sachiniyer/agent-factory/session"

// isLegacyTransientGhost identifies disposable projections, not sessions.
// Older TUIs could strand Loading/Deleting rows on disk and every create path
// deliberately treats them as overwritable (#551). Backfilling through
// ForStorage would settle their transient status and turn the ghost into a real
// title claim. Durable recovery markers are the exception, matching
// Storage.SaveInstances: those rows represent work the daemon still owes.
func isLegacyTransientGhost(item session.InstanceData) bool {
	return (item.Status == session.Loading || item.Status == session.Deleting) &&
		item.PendingHandoffMission == "" && !item.RuntimeCleanupStateUnknown
}

// rawTaskRunHoldsSlot is the storage-only counterpart of holdsTaskRunSlot for
// rows refreshDaemonInstances cannot materialize. Terminal markers must release
// capacity here too because no Instance exists to run their lifecycle edge.
func rawTaskRunHoldsSlot(item session.InstanceData) bool {
	return item.TaskID != "" && item.TaskRunActive &&
		!item.StartupStateUnknown && !item.UserKilled &&
		session.IdleReasonFor(item) != session.IdleReasonRestoreGaveUp
}

// fromInstanceDataForRefresh is the entry point refreshDaemonInstances uses
// to materialize a session.Instance from a persisted on-disk entry. It is a
// package-level variable so tests can observe (or substitute) the call —
// see TestManagerCreateSessionAtomicWithRefresh, which uses it to detect
// whether refresh ever raced CreateSession and tried to construct a
// duplicate Instance from disk.
var fromInstanceDataForRefresh = func(repoID string, data session.InstanceData) (*session.Instance, error) {
	return session.FromInstanceDataWithLoadRuntimeCheckpoint(data, func(closed session.InstanceData) error {
		return persistInstanceData(repoID, closed)
	})
}

// persistLegacyInstanceID is the durable half of daemon-load ID backfill. A
// seam keeps the unknown-outcome branch testable: if this write cannot be
// confirmed, refresh must not materialize the legacy row under an ephemeral ID.
var persistLegacyInstanceID = persistInstanceData

func (m *Manager) refreshLocked() error {
	refreshed, ghosts, taskRunSequence, err := refreshDaemonInstances(m.instances)
	if err != nil {
		return err
	}
	owed := persistLoadRuntimeReplacements(refreshed)
	m.attachCredentialsToAll(refreshed)
	m.instances = refreshed
	// Replaced wholesale, never merged: the ghost set is a projection of what is on
	// disk RIGHT NOW (#1892). A row that starts loading again must stop being a
	// ghost, or its slot would be held twice — once by the ghost and once by the
	// instance it became.
	m.ghostTaskRuns = ghosts
	if taskRunSequence > m.taskRunSequence {
		m.taskRunSequence = taskRunSequence
	}
	m.registerLoadRuntimeSettlementsLocked(owed)
	return nil
}
