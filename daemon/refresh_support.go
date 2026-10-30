package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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
// A sandbox-backed (docker/ssh/hook) row is the one shape whose raw form must
// mirror its materialized form on Started, which is absent from InstanceData.
// FromInstanceData loads a non-archived sandbox row inert (started stays false)
// and rewrites its liveness to LiveLost, so holdsTaskRunSlot releases it:
// Activity is Terminal for LiveLost and canAutoRestoreLostSession returns false
// (ValidateRuntimeAction refuses a !Started session). The raw arm has no Started
// to read and no rewritten liveness, so a sandbox row delegates to
// releasesLostSandboxGhost, which replays the loader's fence restoration and
// liveness rewrite through session.LoadedActivity and releases for the same rows.
// Without this a sandbox row that ghosts on a materialization failure (broken
// worktree or relocation-recovery record, or a row persisted LiveRunning before
// the restart that made it a ghost) wedges max_concurrent_runs forever: no
// in-memory Instance exists to run the lifecycle edge that would clear
// TaskRunActive, the same unrecoverable wedge class the terminal-marker guards
// below prevent.
//
// The sandbox delegation runs INSTEAD of the raw StartupStateUnknown guard
// below. A pending account swap or fenced handoff projects StartupStateUnknown=true
// to fence an older binary, and the loader restores that projected value before
// deciding; the raw projected value would short-circuit here and release a row
// whose materialized form holds (its pending transaction is still in flight), so
// the sandbox arm must let LoadedActivity restore the fence first. The raw guards
// below apply to non-sandbox rows. ForStorage projects StartupStateUnknown=true
// onto a local row mid-account-swap too — the projection has no backend gate
// (projectPendingAccountSwapForPreviousRelease) — and ClassifyActivity checks
// PendingAccountSwap BEFORE StartupStateUnknown regardless of backend type, so
// the materialized form of a local row mid-swap is ActivityPending and HOLDS the
// slot. The local arm mirrors that precedence: the PendingAccountSwap term below
// holds a mid-swap row before the projected StartupStateUnknown guard, or a local
// ghost that fails to materialize while a swap is in flight would slip past the cap
// — and the undercount never self-corrects, because refreshDaemonInstances rebuilds
// ghostTaskRuns from scratch on every call and re-evaluates this arm against the
// same on-disk row. The local backend is the only one that can commit an account
// swap (SupportsAutomaticAccountSwap gates on backend.Type() == "local"), so this
// is the one raw arm the term is needed on.
//
// RebuildTeardownPending is the same class of projected fence: ForStorage writes
// StartupStateUnknown=true to fence an older binary from a row whose stale-pane
// teardown is still owed (session/rebuild_teardown_storage.go), and the row's
// materialized form HOLDS the slot — a Lost pending row is revived by the
// lost-restore loop's own retry (canAutoRestoreLostSession admits the owner) and
// a limit-parked one classifies ActivityPending — so a local ghost that fails to
// materialize with the obligation armed must stay counted too, or the watcher
// admits a replacement run over the very teardown this row still owes (#5371
// review).
//
// The handoff projection has the identical no-backend-gate shape. ForStorage runs
// projectPendingHandoffForPreviousRelease right after the account-swap
// projection, and it sets StartupStateUnknown=true on ANY row whose
// PendingHandoffMission carries an ambiguous delivery verdict (the
// PromptCouldNotConfirm/PromptSentUnverified list, or the missing evidence a
// legacy record carries, which restoreMissingHandoffMissionEvidence normalizes
// to PromptCouldNotConfirm before the projection runs). The local
// backend runs agent handoffs (Capabilities().Handoff is true), so the projection
// reaches local rows mid-mission, and a local ghost persisted mid-mission is
// released by the raw StartupStateUnknown guard for the same reason a mid-swap
// ghost is: the projected value short-circuits before any reader restores it. The
// handoffFence term below holds those rows. A current reader restores the fence
// (RestoreHandoffRollbackFence) and, for a local row whose deferred mission left
// the run's idle edge held, the loaded form keeps its persisted liveness — the
// local backend does not rewrite liveness the way the sandbox loader does — so a
// stored LiveRunning classifies ActivityPending and a recoverable LiveLost is held
// by canAutoRestoreLostSession; the raw arm must hold the same on-disk shape or
// undercount the cap. An ambiguous verdict deliberately does NOT reconstruct
// OpReplacing (pendingHandoffMissionNeedsFence admits only
// PromptNotDelivered/Delivered), so the hold is the mission's own idle edge, not a
// reconstructed op; a row without a pending mission, or a fence-requiring verdict
// the projection leaves alone, relies on the existing StartupStateUnknown term
// exactly as before. The trailing UserKilled and IdleReasonRestoreGaveUp guards
// still release a tombstoned or unrecoverable row even when the fence is up, so a
// handoff ghost the loaded form would release through those markers is not wedged.
//
// The fence re-raises the hold ONLY for the shape the loaded form would hold after
// restoring the fence, so a ghost the materialized form releases is not wedged on
// the cap forever (no in-memory Instance exists to run the lifecycle edge that
// would clear TaskRunActive for a row that failed to materialize). Two shapes
// diverge from a blind "any ambiguous mission holds" and must NOT re-raise:
//
//  1. A row whose startup was already unknown BEFORE the handoff. The projection
//     captures the original in HandoffOriginalStartupStateUnknown; when that
//     original was already true, the loaded form restores StartupStateUnknown=true
//     and ClassifyActivity returns ActivityTerminal (the startup-unknown return is
//     before the liveness switch), so the materialized form releases its slot. A
//     blind handoffFence would hold it anyway and, with no Instance to clear
//     TaskRunActive, wedge the capped task forever. The fence is therefore gated on
//     the marker proving the original state was false — the one shape the
//     projection actually raised from false to true and the loaded form restores to
//     a liveness that can still be active.
//
//  2. A row whose loaded form releases on the liveness axis after the fence is
//     restored. The fence restores StartupStateUnknown=false and reconstructs no op
//     (an ambiguous verdict never rebuilds OpReplacing), so the loaded form's hold
//     is its liveness alone: a stored LiveRunning is ActivityPending and a
//     recoverable LiveLost is held by canAutoRestoreLostSession, but a LiveReady
//     row classifies ActivityIdle (no op, the run went idle) and a LiveArchived
//     row is ActivityTerminal with no restore path, so the loaded form releases
//     both. A blind handoffFence would hold them and wedge the cap. The fence
//     therefore mirrors the loaded form's activity (handoffFenceLoadedFormHolds,
//     which replays FromInstanceData's fence restoration and liveness
//     classification through LoadedActivity) rather than treating every ambiguous
//     mission as active.
func rawTaskRunHoldsSlot(item session.InstanceData) bool {
	if item.TaskID == "" || !item.TaskRunActive {
		return false
	}
	if session.IsSandboxBackendType(item.BackendType) {
		return !releasesLostSandboxGhost(item)
	}
	// Hold the same ambiguous-handoff shape the loader holds: the projection
	// sets StartupStateUnknown=true with no backend gate, so the raw arm must
	// not short-circuit on the projected value before the fence a current
	// reader would restore. The rollback marker
	// (HandoffOriginalStartupStateUnknown proving the original state was false)
	// is the gate, not the verdict list: the marker is set ONLY by
	// projectPendingHandoffForPreviousRelease, which fires for an ambiguous
	// delivery verdict, so its presence proves a compatible writer projected
	// the handoff fence — including a newer release's future ambiguous verdict
	// this binary has no constant for, which the marker still carries. The raw
	// arm therefore honors the marker without requiring the current binary to
	// recognize the verdict, matching the LoadedActivity re-raise for an
	// unrecognized nonempty verdict.
	//
	// The marker gate (shape 1 above) bypasses the StartupStateUnknown guard
	// only when the rollback marker proves the original state was false; a row
	// whose startup was already unknown restores to a terminal loaded form and
	// must not be held. The loaded-form gate (shape 2) holds only the liveness
	// the loaded form would hold after restoring the fence, so an idle
	// LiveReady or a non-restorable LiveArchived row the loaded form releases
	// is not wedged. The trailing IdleReasonRestoreGaveUp guard still releases
	// a LiveLost/LiveDead row the restore loop gave up on, mirroring the
	// canAutoRestoreLostSession arm the loaded form uses for Lost.
	handoffFence := item.PendingHandoffMission != "" &&
		item.HandoffOriginalStartupStateUnknown != nil &&
		!*item.HandoffOriginalStartupStateUnknown &&
		handoffFenceLoadedFormHolds(item)
	return (!item.StartupStateUnknown || item.PendingAccountSwap != nil || item.RebuildTeardownPending || handoffFence) && !item.UserKilled &&
		session.IdleReasonFor(item) != session.IdleReasonRestoreGaveUp
}

// handoffFenceLoadedFormHolds reports whether a local row whose handoff rollback
// fence restored its original startup state would still hold its task-run slot
// through the materialized form's activity. It is the liveness term of the
// handoffFence: the fence restores StartupStateUnknown=false and reconstructs no
// op (an ambiguous verdict never rebuilds OpReplacing), so the loaded form's hold
// is its liveness alone. LoadedActivity replays FromInstanceData's fence
// restoration and liveness classification so the raw arm sees the same Activity a
// materialized Instance would — ActivityPending for a stored LiveRunning (the
// local backend does not rewrite liveness) or a row whose liveness a newer
// release wrote this binary does not recognize (ClassifyActivity fails it closed
// onto ActivityPending), and ActivityIdle for a LiveReady row whose run went idle.
//
// The one loaded-form hold LoadedActivity does not report is a recoverable
// LiveLost row: ClassifyActivity calls LiveLost terminal, but holdsTaskRunSlot
// keeps counting it through canAutoRestoreLostSession while the restore loop is
// still reviving it. The trailing IdleReasonRestoreGaveUp guard in
// rawTaskRunHoldsSlot is the raw arm's mirror of that arm — it releases a LiveLost
// row the loop gave up on — so the fence holds a LiveLost/LiveDead row the loop is
// still reviving (EffectiveLiveness folds Dead into Lost the same way the loader
// does) and the trailing guard releases the unrecoverable half. A LiveArchived row
// is ActivityTerminal with no restore path, so LoadedActivity reports it terminal
// and EffectiveLiveness is not LiveLost, and the fence does not re-raise it.
//
// The LiveLost fallback additionally gates on the loader's relocation
// eligibility: a local LiveLost row whose relocation-recovery record still
// represents a pending relocation loads inert (Started=false), so the restore
// loop refuses it and the loaded form releases — the fence must not re-raise a
// hold for it, or the ghost wedges the cap. A row whose restored record has no
// relocation recovery is the one the loader starts, so it is the one shape the
// fallback re-raises.
//
// RestoreArchiveRollbackFence runs before any relocation decision, the same
// order FromInstanceData and LoadedActivity use. ForStorage replaces
// Worktree.RelocationRecovery with the synthetic archive kill fence
// (archiveReportKillFence) and preserves the real recovery only in
// ArchiveReport.RollbackFence; without restoring the fence first, the relocation
// checks below would decode the synthetic fence as a real pending relocation.
// For a LiveLost row whose original recovery was nil, the synthetic fence
// survives RestoreRelocationRecoveryOriginals (it carries the retained tree's
// identity, so isWorktreeMissingRollbackFence does not recognize it) and the
// LiveLost fallback releases a ghost the loaded restore loop would count,
// allowing max_concurrent_runs to be exceeded; if the archived original is
// malformed, the synthetic fence decodes where the real one would not, and the
// LoadedActivityPending early return can hold an unmaterializable ghost forever.
// Restoring the archive fence first recovers the real recovery (or nil) the way
// FromInstanceData does, so the relocation checks see the same record the loader
// does.
//
// A row whose relocation-recovery record cannot be decoded at all (for example,
// missing rollback-safety originals) is unmaterializable: FromInstanceData
// rejects it at the same RestoreRelocationRecoveryOriginals boundary, so no
// Instance or restore loop can ever clear its TaskRunActive. LoadedActivity
// returns ActivityPending for such a non-sandbox row (it cannot read a loaded
// form that does not exist), so the LoadedActivityPending early return would
// hold the slot and wedge the cap on a ghost no in-memory lifecycle edge can
// release. The undecodable guard at the top releases that shape before
// LoadedActivity is consulted, mirroring the loader's rejection.
//
// A row whose relocation-recovery record decodes but is runtime-invalid is
// equally unmaterializable: FromInstanceData installs it through
// GitWorktree.RestoreRelocationRecovery, which rejects shapes a rollback-safe
// projection preserves but the worktree cannot install (a move_unknown with an
// empty alternate path or missing identity, a claim_stale with no identity, a
// cleanup state with no generation, or an alternate path that collides with the
// worktree's own). LoadedActivity does not run that boundary, so a
// non-sandbox row whose record decodes but cannot install reports ActivityPending;
// the LoadedActivityPending early return would hold its slot and, with no
// Instance to run the lifecycle edge that clears TaskRunActive, wedge
// max_concurrent_runs forever. The runtime-installable guard below mirrors
// GitWorktree.RestoreRelocationRecovery's state-shape and path-collision
// checks on the restored record, so the fence releases the same unmaterializable
// shape the loader rejects.
func handoffFenceLoadedFormHolds(item session.InstanceData) bool {
	// Restore the archive rollback fence before inspecting relocation, the same
	// order FromInstanceData and LoadedActivity restore it. Without this, the
	// relocation checks below would read the synthetic archive kill fence
	// ForStorage publishes in Worktree.RelocationRecovery instead of the real
	// recovery the fence preserved in ArchiveReport.RollbackFence, and release or
	// hold a ghost the loaded form does not.
	item = item.RestoreArchiveRollbackFence()
	// A row whose relocation-recovery record cannot be decoded is unmaterializable:
	// FromInstanceData rejects it at this same RestoreRelocationRecoveryOriginals
	// boundary, so no Instance or restore loop can ever clear its TaskRunActive.
	// LoadedActivity returns ActivityPending for such a non-sandbox row because it
	// cannot read a loaded form that does not exist, so the LoadedActivityPending
	// early return below would hold the slot and, with no lifecycle edge to release
	// it, wedge max_concurrent_runs forever on a ghost no in-memory Instance can
	// clear. Check the loader's restoration boundary before accepting
	// LoadedActivityPending and release this unmaterializable shape.
	restored, err := item.RestoreRelocationRecoveryOriginals()
	if err != nil {
		return false
	}
	item = restored
	// A row whose relocation-recovery record decodes but is runtime-invalid is
	// equally unmaterializable: FromInstanceData installs it through
	// GitWorktree.RestoreRelocationRecovery, which rejects shapes the rollback
	// metadata preserves but the worktree cannot install (a move_unknown with an
	// empty alternate path or missing identity, a claim_stale with no identity, a
	// cleanup state with no generation, or an alternate path that collides with
	// the worktree's own). LoadedActivity does not run that boundary, so a
	// non-sandbox row whose record decodes but cannot install reports
	// ActivityPending; the LoadedActivityPending early return would hold its slot
	// and, with no Instance to run the lifecycle edge that clears TaskRunActive,
	// wedge max_concurrent_runs forever. Mirror the loader's install check on the
	// restored record before accepting LoadedActivityPending and release this
	// unmaterializable shape.
	if !relocationRecoveryInstallable(item.Worktree.RelocationRecovery, item.Worktree.WorktreePath) {
		return false
	}
	if activity, _ := session.LoadedActivity(item); activity == session.ActivityPending {
		return true
	}
	// canAutoRestoreLostSession's restore-eligibility term: a LiveLost/LiveDead
	// row the loop is still reviving holds; the trailing IdleReasonRestoreGaveUp
	// guard in rawTaskRunHoldsSlot releases the half the loop gave up on.
	//
	// Gate on the loader's relocation/started eligibility rather than effective
	// liveness alone. A local LiveLost row whose relocation-recovery record
	// decodes but still represents a pending relocation the worktree restore
	// later rejects (or any row whose restored record still carries a
	// relocation recovery) loads inert with Started=false, so
	// ValidateRuntimeAction(RuntimeActionRecoverLost) refuses it and
	// canAutoRestoreLostSession returns false — the loaded form releases.
	// Equating every LiveLost row with the restore loop would hold such a ghost
	// and, with no Instance to run the lifecycle edge that clears TaskRunActive,
	// wedge max_concurrent_runs forever. RestoreRelocationRecoveryOriginals is
	// the same boundary the loader uses; it clears the worktree-missing rollback
	// fence (a stall projection, not a pending relocation) but leaves a real
	// pending relocation in place, so a row whose restored record has no
	// relocation recovery is the one the loader starts and the restore loop can
	// still revive — the only shape the fence re-raises a hold for.
	return session.EffectiveLiveness(item) == session.LiveLost &&
		item.Worktree.RelocationRecovery == nil
}

// relocationRecoveryInstallable reports whether GitWorktree.RestoreRelocationRecovery
// would accept the restored relocation-recovery record — the runtime boundary
// FromInstanceData enforces after RestoreRelocationRecoveryOriginals decodes the
// rollback metadata. RestoreRelocationRecoveryOriginals only reverses the
// rollback-safe projection; it does not validate the record's runtime shape (a
// move_unknown with an empty alternate path or missing identity, a claim_stale
// with no identity, a cleanup state with no generation, or a path that collides
// with the worktree's own), so a row whose record decodes can still fail to
// materialize at GitWorktree.RestoreRelocationRecovery. Such a row has no
// Instance and no restore loop to clear TaskRunActive, so handoffFenceLoadedFormHolds
// releases it instead of wedging the cap on a ghost the loaded form would never
// hold. The state-shape and path-collision mirrors below are the same boundary
// GitWorktree.RestoreRelocationRecovery enforces, restricted to the fields a
// restored InstanceData carries. A nil recovery is installable (FromInstanceData
// skips the install block when there is no record to install).
func relocationRecoveryInstallable(recovery *session.GitWorktreeRelocationRecoveryData, worktreePath string) bool {
	if recovery == nil {
		return true
	}
	state := recovery.State
	identityKnown := recovery.IdentityKnown
	if state == "" {
		// normalizeRecovery: a record without an explicit state is a legacy
		// ambiguous move with a known identity, the same default
		// GitWorktree.RestoreRelocationRecovery applies before validating it.
		state = sessiongit.RelocationRecoveryMoveUnknown
		identityKnown = true
	}
	switch state {
	case sessiongit.RelocationRecoveryMoveUnknown:
		if recovery.AlternatePath == "" || !identityKnown {
			return false
		}
	case sessiongit.RelocationRecoveryClaimStale:
		if !identityKnown {
			return false
		}
	case sessiongit.RelocationRecoveryCleanupReady, sessiongit.RelocationRecoveryCleanupFinalizing:
		if !identityKnown || recovery.CleanupGeneration == "" {
			return false
		}
	case sessiongit.RelocationRecoveryStalled:
		// A read-only probe may time out before any identity can be captured.
	case sessiongit.RelocationRecoveryCleanupStalled:
		// An identity-unknown stall is accepted (and promoted to cleanup_ready);
		// an identity-qualified stall requires a generation to retry the exact
		// claimed archive.
		if identityKnown && recovery.CleanupGeneration == "" {
			return false
		}
	default:
		return false
	}
	if recovery.AlternatePath != "" && recovery.AlternatePath == worktreePath {
		return false
	}
	return true
}

// releasesLostSandboxGhost reports whether a row that failed to materialize is a
// sandbox ghost whose materialized form releases its task-run slot, so the raw arm
// releases it too instead of wedging the cap on an unloadable row. It is the
// sandbox term of rawTaskRunHoldsSlot, scoped to the rows whose materialized form
// actually releases: a sandbox row whose loaded activity is terminal.
//
// A pending-handoff variant whose delivery still owns an in-flight obligation
// (PromptNotDelivered/PromptDelivered) reconstructs OpReplacing on load, so its
// live arm holds and this returns false. An ambiguous delivery
// (PromptCouldNotConfirm, or the missing evidence a legacy record carries) does
// NOT reconstruct the fence; the loaded sandbox is inert/terminal and releases,
// so the raw arm must agree. A pending account swap restores its fence and holds,
// so this returns false for it too. session.LoadedActivity mirrors
// LifecycleView.Activity() composed with FromInstanceData's full fence
// restoration, InFlightOp reconstruction, and inert sandbox liveness rewrite, so
// the raw arm sees the same verdict a loaded Instance would — including a row
// persisted LiveRunning before the restart that ghosts, which the loader rewrites
// to LiveLost. Gating on IsSandboxBackendType rather than LostSandboxRecord lets
// that mid-run row reach LoadedActivity instead of being held on its stored
// (not-yet-rolled-forward) liveness.
func releasesLostSandboxGhost(item session.InstanceData) bool {
	if !session.IsSandboxBackendType(item.BackendType) {
		return false
	}
	activity, _ := session.LoadedActivity(item)
	return activity == session.ActivityTerminal
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
// — the standalone in-place migration sweep (now the cache's migrateAndExtract
// cold path) and the LoadAllRepoInstancesReportingMissing read — into a
// stat-gated load: a file whose signature is unchanged since it was last parsed
// is served from cache without being read, validated, normalized, or migrated
// at all, so a steady-state tick is O(number of repo files), not O(total records).
var refreshRepoFileCache = config.NewRepoInstancesFileCache()

// loadAllRepoInstancesForRefresh is the loader refreshDaemonInstances reads
// every repo's instances.json through. It must be the reporting-skips form:
// the omitting form, config.LoadAllRepoInstances, drops a repo it could not
// read, and the polling refresh used to read that absence as "no longer
// corrupt" and clear a startup-skipped repo from the skip set — serving a
// truncated snapshot as complete again (#4783). A package-level variable so a
// test can stage a read failure the file system cannot stage deterministically:
// a persistently unreadable file already aborts the refresh in the file cache's
// loadRepoFile, so the reachable shape is a file that reads there and fails here. It also
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
	// A repo whose instances.json vanished was preserved above; write its live
	// sessions' records back so the preservation ends (#6048).
	m.restoreMissingRecordsLocked(refreshed)
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
