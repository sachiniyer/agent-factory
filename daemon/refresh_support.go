package daemon

import (
	"errors"
	"reflect"

	"github.com/sachiniyer/agent-factory/session"
)

// worktreeInventoryState preserves the persisted lanes that the daemon could
// not materialize, plus any repo whose persisted roster could not be observed
// at all. A skipped required row is still part of branch correlation: absence
// from m.instances is unknown, never evidence that a collision was repaired.
type worktreeInventoryState struct {
	unmaterialized []session.InstanceData
	incompleteRepo map[string]error
	globalErr      error
}

func (s worktreeInventoryState) incompleteFor(repoID string) error {
	return errors.Join(s.globalErr, s.incompleteRepo[repoID])
}

func (m *Manager) setWorktreeInventoryLocked(next worktreeInventoryState) {
	if sameWorktreeInventoryState(m.worktreeInventory, next) {
		return
	}
	m.worktreeInventory = next
	m.worktreeInventoryVersion++
}

func sameWorktreeInventoryState(left, right worktreeInventoryState) bool {
	if !reflect.DeepEqual(left.unmaterialized, right.unmaterialized) || errorText(left.globalErr) != errorText(right.globalErr) {
		return false
	}
	if len(left.incompleteRepo) != len(right.incompleteRepo) {
		return false
	}
	for repoID, leftErr := range left.incompleteRepo {
		if errorText(leftErr) != errorText(right.incompleteRepo[repoID]) {
			return false
		}
	}
	return true
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

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
var fromInstanceDataForRefresh = session.FromInstanceData

// persistLegacyInstanceID is the durable half of daemon-load ID backfill. A
// seam keeps the unknown-outcome branch testable: if this write cannot be
// confirmed, refresh must not materialize the legacy row under an ephemeral ID.
var persistLegacyInstanceID = persistInstanceData
