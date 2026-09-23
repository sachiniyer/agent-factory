package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

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

// loadAllRepoInstancesForRefresh is the loader refreshDaemonInstances reads
// every repo's instances.json through. It must be the reporting-skips form:
// the omitting form, config.LoadAllRepoInstances, drops a repo it could not
// read, and the polling refresh used to read that absence as "no longer
// corrupt" and clear a startup-skipped repo from the skip set — serving a
// truncated snapshot as complete again (#4783). A package-level variable so a
// test can stage a read failure the file system cannot stage deterministically:
// a persistently unreadable file already aborts the refresh in the migrator,
// so the reachable shape is a file that reads there and fails here. It also
// reports repos whose instances.json is missing, which load as "[]" but were
// not read, so they must not count as re-read either.
var loadAllRepoInstancesForRefresh = config.LoadAllRepoInstancesReportingMissing

// refreshLocked rebuilds the manager's instance map from disk under m.mu. A
// marked on_complete row that re-materializes here re-arms its owed teardown,
// the same as at restore (#4162).
func (m *Manager) refreshLocked() error {
	refreshed, ghosts, skipped, reread, err := refreshDaemonInstances(m.instances)
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
	// Trim repaired repos from the startup skip set without ever adding a
	// mid-life-corrupted one. A startup-skipped repo drops out only when this
	// poll re-read and parsed its instances.json, so list/get/whoami stop
	// refusing the now-complete snapshot (#603 closed over the wire); one that is
	// still corrupt, now unreadable, or absent from disk stays skipped (#4783). A
	// repo that newly fails mid-life keeps its prior in-memory rows via the
	// re-hydration above, so its sessions stay in the snapshot and it is
	// correctly NOT reported as skipped until the daemon restarts and re-runs
	// startup. Guarded by m.mu (refreshLocked's caller holds it).
	m.skippedRepos = retainStillSkipped(m.skippedRepos, skipped, reread)
	m.registerLoadRuntimeSettlementsLocked(owed)
	m.armOwedTaskLifecyclesLocked()
	return nil
}
