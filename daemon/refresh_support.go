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
//
// A sandbox-backed (docker/ssh/hook) LiveLost row is the one shape whose raw form
// must mirror its materialized form on Started, which is absent from InstanceData.
// FromInstanceData loads a non-archived sandbox row inert (started stays false),
// so holdsTaskRunSlot releases it: Activity is Terminal for LiveLost and
// canAutoRestoreLostSession returns false (ValidateRuntimeAction refuses a
// !Started session). The raw arm has no Started to read, so it consults
// session.LostSandboxRecord directly — the same storage predicate the loader
// uses to decide the inert load — and releases for the same rows. Without this
// term a sandbox row that ghosts on a materialization failure (broken worktree
// or relocation-recovery record) wedges max_concurrent_runs forever: no
// in-memory Instance exists to run the lifecycle edge that would clear
// TaskRunActive, the same unrecoverable wedge class the terminal-marker guards
// below prevent.
func rawTaskRunHoldsSlot(item session.InstanceData) bool {
	return item.TaskID != "" && item.TaskRunActive &&
		!item.StartupStateUnknown && !item.UserKilled &&
		session.IdleReasonFor(item) != session.IdleReasonRestoreGaveUp &&
		!session.LostSandboxRecord(item)
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

// refreshRepoFileCache is the daemon's per-repo instances.json cache (#5169).
// It fuses what used to be two full passes over every file on every poll tick
// — the MigrateAllRepoInstancesForDaemonLoad sweep and the
// LoadAllRepoInstancesReportingMissing read — into a stat-gated load: a file
// whose signature is unchanged since it was last parsed is served from cache
// without being read, validated, normalized, or migrated at all, so a
// steady-state tick is O(number of repo files), not O(total records).
var refreshRepoFileCache = config.NewRepoInstancesFileCache()

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
// not read, so they must not count as re-read either — and the per-repo file
// signatures, which is what refreshRowOutcomes keys its parse-outcome cache on.
var loadAllRepoInstancesForRefresh = func() (config.RepoInstancesPollResult, error) {
	return refreshRepoFileCache.LoadAll()
}

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
	// poll re-read and parsed its instances.json INTO a fully loadable row set,
	// so list/get/whoami stop refusing the now-complete snapshot (#603 closed
	// over the wire); one that is still corrupt, now unreadable, absent from
	// disk, or parses-but-any-row-unloadable stays skipped, its reason rewritten
	// to rows-failed-to-load when the file itself parsed (#4783, #4812, #4876).
	// A repo that newly fails mid-life keeps its prior in-memory rows via the
	// re-hydration above, so its sessions stay in the snapshot and it is correctly
	// NOT reported as skipped until the daemon restarts and re-runs
	// startup. Guarded by m.mu (refreshLocked's caller holds it).
	m.skippedRepos = retainStillSkipped(m.skippedRepos, skipped, reread)
	m.registerLoadRuntimeSettlementsLocked(owed)
	m.armOwedTaskLifecyclesLocked()
	return nil
}
