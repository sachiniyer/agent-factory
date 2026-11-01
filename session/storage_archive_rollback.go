package session

import "github.com/sachiniyer/agent-factory/session/git"

// The archive-rollback projections: how a record's worktree facts become the
// git layer's rollback fence, and back. Split from storage.go, which holds the
// record shapes themselves.

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func archiveRollbackFence(data InstanceData) *git.ArchiveRollbackFence {
	fence := &git.ArchiveRollbackFence{
		OriginalStartupStateUnknown: data.StartupStateUnknown,
		OriginalExternalWorktree:    data.Worktree.ExternalWorktree,
		OriginalBranchCreatedByUs:   cloneBoolPointer(data.Worktree.BranchCreatedByUs),
		RelocationRecoveryProjected: true,
	}
	if recovery := data.Worktree.RelocationRecovery; recovery != nil {
		fence.OriginalRelocationRecovery = &git.ArchiveRollbackRelocationRecovery{
			State:                              recovery.State,
			CleanupLifecycle:                   recovery.CleanupLifecycle,
			AlternatePath:                      recovery.AlternatePath,
			IdentityKnown:                      recovery.IdentityKnown,
			Device:                             recovery.Device,
			Inode:                              recovery.Inode,
			FileType:                           recovery.FileType,
			CleanupGeneration:                  recovery.CleanupGeneration,
			CleanupOriginalExternalWorktree:    cloneBoolPointer(recovery.CleanupOriginalExternalWorktree),
			CleanupOriginalBranchCreatedByUs:   cloneBoolPointer(recovery.CleanupOriginalBranchCreatedByUs),
			CleanupOriginalStartupStateUnknown: cloneBoolPointer(recovery.CleanupOriginalStartupStateUnknown),
			OriginalExternalWorktree:           cloneBoolPointer(recovery.OriginalExternalWorktree),
			OriginalBranchCreatedByUs:          cloneBoolPointer(recovery.OriginalBranchCreatedByUs),
			OriginalStartupStateUnknown:        cloneBoolPointer(recovery.OriginalStartupStateUnknown),
		}
	}
	return fence
}

func archiveRollbackRelocationRecovery(recovery *git.ArchiveRollbackRelocationRecovery) *GitWorktreeRelocationRecoveryData {
	if recovery == nil {
		return nil
	}
	return &GitWorktreeRelocationRecoveryData{
		State:                              recovery.State,
		CleanupLifecycle:                   recovery.CleanupLifecycle,
		AlternatePath:                      recovery.AlternatePath,
		IdentityKnown:                      recovery.IdentityKnown,
		Device:                             recovery.Device,
		Inode:                              recovery.Inode,
		FileType:                           recovery.FileType,
		CleanupGeneration:                  recovery.CleanupGeneration,
		CleanupOriginalExternalWorktree:    cloneBoolPointer(recovery.CleanupOriginalExternalWorktree),
		CleanupOriginalBranchCreatedByUs:   cloneBoolPointer(recovery.CleanupOriginalBranchCreatedByUs),
		CleanupOriginalStartupStateUnknown: cloneBoolPointer(recovery.CleanupOriginalStartupStateUnknown),
		OriginalExternalWorktree:           cloneBoolPointer(recovery.OriginalExternalWorktree),
		OriginalBranchCreatedByUs:          cloneBoolPointer(recovery.OriginalBranchCreatedByUs),
		OriginalStartupStateUnknown:        cloneBoolPointer(recovery.OriginalStartupStateUnknown),
	}
}

func archiveReportKillFence(report git.ArchiveReport) *GitWorktreeRelocationRecoveryData {
	tree := report.RetainedTrees[0]
	fence := report.RollbackFence
	originalExternal := fence.OriginalExternalWorktree
	originalBranch := cloneBoolPointer(fence.OriginalBranchCreatedByUs)
	originalStartup := fence.OriginalStartupStateUnknown
	return &GitWorktreeRelocationRecoveryData{
		// A previous reader refuses explicit kill while any recovery record is
		// unresolved, but its restore path consumes an identity-qualified
		// AlternatePath as a worktree candidate. Retained trees can be snapshots
		// from an older archive cycle, so never publish their pathname here. A
		// claim-stale record with no alternate and the retained source's identity
		// cannot match the cross-filesystem published archive and therefore makes
		// both kill and restore fail closed. Current readers remove this synthetic
		// record through RollbackFence before reconstructing the worktree.
		State:                       git.RelocationRecoveryClaimStale,
		IdentityKnown:               true,
		Device:                      tree.Device,
		Inode:                       tree.Inode,
		FileType:                    tree.FileType,
		OriginalExternalWorktree:    &originalExternal,
		OriginalBranchCreatedByUs:   originalBranch,
		OriginalStartupStateUnknown: &originalStartup,
	}
}

// ForStorage returns data suitable for instances.json. InstanceData is also the
// daemon Snapshot payload, so it can carry transient in-flight operation state;
// disk persistence must not.
func (d InstanceData) ForStorage() InstanceData {
	if d.placedMachine != nil {
		d.PlacedMachine, d.placedMachine = d.placedMachine, nil
	}
	if d.snapshotTabsProjected {
		d.Tabs = d.snapshotStorageTabs
		d.snapshotTabsProjected = false
		d.snapshotStorageTabs = nil
	}
	reportDetached := false
	if d.archiveReportSource != nil {
		path, recovery, hasRecovery, report := d.archiveReportSource.PersistenceSnapshot()
		d.Worktree.WorktreePath = path
		d.Worktree.RelocationRecovery = nil
		if hasRecovery {
			externalWorktree := d.Worktree.ExternalWorktree
			branchCreatedByUs := cloneBoolPointer(d.Worktree.BranchCreatedByUs)
			startupStateUnknown := d.StartupStateUnknown
			d.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{
				State: recovery.State, AlternatePath: recovery.AlternatePath,
				IdentityKnown: recovery.IdentityKnown, Device: recovery.Device,
				Inode: recovery.Inode, FileType: recovery.FileType,
				CleanupGeneration:           recovery.CleanupGeneration,
				OriginalExternalWorktree:    &externalWorktree,
				OriginalBranchCreatedByUs:   branchCreatedByUs,
				OriginalStartupStateUnknown: &startupStateUnknown,
			}
		}
		if report.Empty() {
			d.ArchiveReport = nil
		} else {
			report.RollbackFence = archiveRollbackFence(d)
			d.ArchiveReport = &report
			reportDetached = true
		}
	}
	lv := livenessFromData(d)
	d.Status = composeStatus(lv, OpNone)
	d.Liveness = lv
	d.InFlightOp = OpNone
	d.LifecycleAction = LifecycleActionNone
	d.CanKill = false
	d.CanHandoff = false
	d.CurrentAgent = ""
	d.IsRoot = false
	d.ModelChange = nil
	d.IdleReason = IdleReasonNone
	// Derived from the backend on every snapshot, exactly like CanKill above, so
	// persisting them stores a stale answer that a restart would recompute anyway —
	// and these carry long, versioned refusal PROSE, which would sit in every
	// instances.json row and be read back as fact by an older binary.
	d.TabKinds = nil
	d.TabRosterMutable = nil
	d.Placement = nil
	d.TabManagement = nil
	d.ArchiveWarning = ""
	// A live-probe outage is exactly the class of transient observation the
	// fields above are scrubbed for: re-derived by the restarted daemon's own
	// probes, never inherited (#5571 W1.3).
	d.Unreachable = nil
	// The compatibility projection must capture original values before either it
	// or the relocation fence below overwrites them. The live archiveReportSource
	// block above already stamps the fence pre-projection; on the disk-reload path
	// (archiveReportSource == nil) the recapture below would otherwise run after
	// projectPending{AccountSwap,Handoff}ForPreviousRelease set
	// d.StartupStateUnknown = true and snapshot the projected value as the
	// "original". Snapshot the fence now and reuse it at recapture, exactly as the
	// live block does. Guarded to the recapture path so the live path pays nothing.
	// Before the pending projections below overwrite StartupStateUnknown: the
	// worktree-missing fence records the row's real ownership values.
	missingRowHasRealRecovery := d.Worktree.Missing && d.Worktree.RelocationRecovery != nil
	if d.Worktree.Missing && d.Worktree.RelocationRecovery == nil {
		d.Worktree.RelocationRecovery = worktreeMissingRollbackFence(d)
	}
	var fenceSnapshot *git.ArchiveRollbackFence
	if d.archiveReportSource == nil && d.ArchiveReport != nil && !d.ArchiveReport.Empty() && d.ArchiveReport.RollbackFence == nil {
		fenceSnapshot = archiveRollbackFence(d)
	}
	// A missing row that already carries a real record keeps it, but a record
	// the previous release would resolve by accepting whatever is at the path is
	// written in the fence's unresolvable form
	// (fenceMissingRowRecoveryForPreviousRelease). After the snapshot above,
	// which current readers restore from and so must hold the real record.
	if missingRowHasRealRecovery {
		recovery := *d.Worktree.RelocationRecovery
		fenceMissingRowRecoveryForPreviousRelease(&recovery)
		d.Worktree.RelocationRecovery = &recovery
	}
	d = d.restoreMissingHandoffMissionEvidence()
	d = d.restoreMissingAccountSwapMissionEvidence()
	d = d.projectPendingAccountSwapForPreviousRelease()
	d = d.projectPendingHandoffForPreviousRelease()
	d = d.projectRebuildTeardownForPreviousRelease()
	// Older binaries ignore ArchiveReport, but the previous release understands
	// the inert/ownership fields and relocation recovery. Together they refuse
	// restore and explicit kill instead of publishing an incomplete tree or
	// deleting the report's row.
	if d.ArchiveReport != nil && !d.ArchiveReport.Empty() {
		report := *d.ArchiveReport
		if !reportDetached {
			report = d.ArchiveReport.Clone()
		}
		if report.RollbackFence == nil {
			report.RollbackFence = fenceSnapshot
		}
		d.ArchiveReport = &report
		d.StartupStateUnknown = true
		d.Worktree.ExternalWorktree = true
		branchCreatedByUs := false
		d.Worktree.BranchCreatedByUs = &branchCreatedByUs
		// v1.0.235 already understands relocation recovery and refuses an
		// explicit kill while one is unresolved. Project a read-only stall through
		// that admission path so rollback cannot turn ExternalWorktree's cleanup
		// no-op into permission to delete the row which owns the retained trees.
		// Current readers remove this synthetic latch from RollbackFence before
		// reconstructing the worktree.
		d.Worktree.RelocationRecovery = archiveReportKillFence(report)
	}
	projectRelocationRecoveryForPreviousRelease(d.Worktree.RelocationRecovery)
	if d.Worktree.RelocationRecovery != nil {
		// v1.0.228 predates relocation_recovery and ignores unknown JSON fields.
		// Project the unresolved row through safety fields that version already
		// understands: it loads inert, treats the checkout as user-owned, and
		// cannot delete its branch. The recovery record carries the original
		// values so current readers remove this rollback fence on load.
		d.StartupStateUnknown = true
		d.Worktree.ExternalWorktree = true
		branchCreatedByUs := false
		d.Worktree.BranchCreatedByUs = &branchCreatedByUs
	}
	switch {
	case lv == LiveArchived:
		// Archived rows have already reaped their runtime, so retaining a teardown
		// identity here would only preserve unused credentials until row deletion,
		// except a preserved unpushed workspace (below).
		d.RuntimeCleanupStateUnknown = false
		d.KilledWithoutLiveRuntime = false
		if d.keepsPreservedWorkspaceHandle() {
			// The exception: an archive that left its workspace on the host still owes
			// the reap of it, and a restart must not lose the handle that names what
			// af made (#5867). A fresh snapshot staged the private field; a reloaded
			// row carries only the public one.
			if d.runtimeCleanup != nil {
				d.RuntimeCleanup = d.runtimeCleanup.clone()
			}
		} else {
			d.RuntimeCleanup = nil
		}
	case !d.UserKilled && !d.RuntimeCleanupStateUnknown:
		// Cleanup credentials/identities have no reason to live in ordinary LOCAL
		// session records — a local reaped-worktree path proves nothing extra, and
		// the field is operator-private. Kill intent and an unknown teardown
		// outcome are the two durable retention boundaries there.
		//
		// Sandbox backends (docker/ssh/hook) are the exception, added retroactively:
		// their off-box runtime SURVIVES a daemon restart, but the ordinary-
		// snapshot reload produces an INERT backend whose runtimeTeardown is nil —
		// a shape indistinguishable in memory from a row that never owed a reap.
		// Without the persisted handle, that reload cannot rebuild a real
		// teardown, a kill committed against it would stage the
		// KilledWithoutLiveRuntime marker from that handle absence alone (an
		// unearned vouch that "nothing was owed"), and the record delete would
		// orphan a still-live remote workspace with no remaining pointer.
		// Persist the staged handle here so a restart can restore it and the kill
		// can reap for real; the marker is then staged only when a backend
		// positively owes nothing (see toInstanceDataLocked). The field stays
		// subject to bugreport redaction; tombstoned rows already carried it,
		// this widens the on-disk set to ordinary sandbox rows.
		if isSandboxBackendType(d.BackendType) {
			// A fresh snapshot staged the private field; publish it. A reloaded
			// row carries only the public RuntimeCleanup (the private field is
			// lost on JSON unmarshal), so the runtimeCleanup!=nil guard only
			// fires on a fresh snapshot; the reloaded public field is preserved
			// untouched the same way the tombstone default case does.
			if d.runtimeCleanup != nil {
				// Fence the previous release's read of this row. Before this
				// release, ForStorage cleared RuntimeCleanup on the ordinary
				// branch and FromInstanceData rebuilt a teardown only for a
				// UserKilled tombstone or an unknown-cleanup row. A binary rolled
				// back to that release would therefore load this ordinary sandbox
				// row inert (no handle to rebuild) and either erase the handle on
				// its next checkpoint or stage KilledWithoutLiveRuntime from the
				// nil teardown on a kill — the orphan this PR reaps. Publish the
				// handle under a projected RuntimeCleanupStateUnknown=true so the
				// previous release's FromInstanceData rebuilds the teardown and
				// its ForStorage default branch preserves the handle on reload,
				// and save the real false in OriginalRuntimeCleanupStateUnknown so
				// a current reader restores it (RestoreRuntimeCleanupRollbackFence)
				// and treats the row as the ordinary session it is. The previous
				// release ignores the additive field and keeps the projected value,
				// which is the safe direction: its default branch is the retention
				// path this handle already rode on UserKilled/unknown rows.
				if d.OriginalRuntimeCleanupStateUnknown == nil {
					original := false
					d.OriginalRuntimeCleanupStateUnknown = &original
				}
				d.RuntimeCleanupStateUnknown = true
				d.RuntimeCleanup = d.runtimeCleanup.clone()
			}
		} else {
			d.RuntimeCleanup = nil
		}
		d.KilledWithoutLiveRuntime = false
	default:
		// A kill tombstone or an unknown-cleanup retention row. Publish the staged
		// cleanup handle only when one was staged on a fresh snapshot; a reloaded
		// row carries only the public field, and the nil-private guard preserves it
		// exactly as the previous three-way switch did.
		if d.runtimeCleanup != nil {
			d.RuntimeCleanup = d.runtimeCleanup.clone()
		}
		// The inert-at-kill marker rides only a UserKilled tombstone. An
		// unknown-cleanup row never carries it: its unknown cleanup is armed only
		// when a teardown handle exists, so there is no "nothing owed" shape to
		// admit. Publish a freshly-staged marker; otherwise preserve a marker
		// reloaded from disk (the private field is lost on JSON unmarshal) so the
		// next load still reads it.
		switch {
		case d.killedWithoutLiveRuntime != nil:
			d.KilledWithoutLiveRuntime = *d.killedWithoutLiveRuntime
		case !d.UserKilled:
			d.KilledWithoutLiveRuntime = false
		}
	}
	// Never let the private staging pointer escape a storage projection. Loaded
	// tombstones have only RuntimeCleanup set and therefore preserve it above.
	d.runtimeCleanup = nil
	d.killedWithoutLiveRuntime = nil
	d.archiveReportSource = nil
	d.archiveReportPending = false
	return d
}
