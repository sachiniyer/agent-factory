package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ToInstanceData converts an Instance to its serializable form
func (i *Instance) ToInstanceData() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.toInstanceDataLocked()
}

// toInstanceDataLocked is the shared body. Caller holds i.mu (read or write).
// CreatedAt and UpdatedAt are copied unchanged; reading does not mutate a session.
func (i *Instance) toInstanceDataLocked() InstanceData {
	data := InstanceData{
		ID:     i.ID,
		TaskID: i.TaskID,
		Title:  i.Title,
		Path:   i.Path,
		Branch: i.Branch,
		// Serialize the two-axis state plus the legacy composed Status. Liveness is
		// the daemon truth; InFlightOp rides daemon snapshots so secondary TUIs can
		// cold-start into archive/restore operations without lossy Status
		// reconstruction (#1436). Disk writers scrub InFlightOp before persistence.
		Status:                   i.statusLocked(),
		Liveness:                 i.liveness,
		InFlightOp:               i.inFlightOp,
		LifecycleAction:          lifecycleActionFor(i.ID, i.Title, i.liveness, i.inFlightOp, i.startupStateUnknown, i.userKilled, i.pendingAccountSwap != nil, i.unreachableDataLocked() != nil, !i.prune.prunedAt.IsZero()),
		CanKill:                  canKillFor(i.ID, i.inFlightOp),
		CanHandoff:               i.canHandoffLocked(),
		CurrentAgent:             i.currentAgentNameLocked(),
		IsRoot:                   IsReservedTitle(i.Title),
		ModelChange:              agentModelChangeForLiveness(i.agentModelChange, i.liveness),
		ArchiveWarning:           i.archiveWarning,
		CallbackUnavailable:      i.callbackUnavailable,
		AgentConfigSkipped:       append([]string(nil), i.agentConfigSkipped...),
		LostRestoreFailure:       cloneLostRestoreFailure(i.lostRestoreFailure),
		LostRestoreWait:          cloneLostRestoreWait(i.lostRestoreWait),
		RebuildTeardownPending:   i.rebuildTeardownPending,
		Unreachable:              i.unreachableDataLocked(),
		LastPromptAttemptAt:      i.lastPromptAttemptAt,
		LastPromptDeliveryStatus: i.lastPromptDeliveryStatus,
		LastPaneChurnAt:          i.lastPaneChurnAt,
		Height:                   i.Height,
		Width:                    i.Width,
		CreatedAt:                i.CreatedAt,
		UpdatedAt:                i.UpdatedAt,
		ArchivedAt:               i.archivedAt,
		ArchiveNonce:             i.prune.nonce,
		PrunedAt:                 i.prune.prunedAt,
		PruneIntent:              clonePruneIntent(i.prune.intent),
		Program:                  i.Program,
		RuntimeProgram:           i.runtimeProgram,
		RuntimePID:               i.runtimePID,
		RuntimeStartID:           i.runtimeStartID,
		Account:                  i.Account,
		Host:                     i.Host,
		BranchUnpushed:           i.branchUnpushed,
		ArchivedWorkspace:        cloneArchivedWorkspace(i.archivedWorkspace),
		AccountAgent:             i.accountAgent,
		AccountAutoSelected:      i.accountAutoSelected,
		AccountLimitObservations: append([]AccountLimitObservationData(nil), i.accountLimitObservations...),
		PendingAccountSwap:       cloneAccountSwapData(i.pendingAccountSwap),
		Prompt:                   i.Prompt,
		PendingHandoffMission:    i.pendingHandoffMission,
		HandoffDeliveryStatus:    i.handoffDeliveryStatus,
		UserKilled:               i.userKilled,
		StartupStateUnknown:      i.startupStateUnknown,
		RootRecreateContext:      i.rootRecreateContext,
		Attention:                cloneAttention(i.attention),
	}
	data.IdleReason = IdleReasonFor(data)
	data.RuntimeCleanupStateUnknown = i.runtimeCleanupStateUnknown
	data.archivePushCompleted = i.archivePushCompleted

	if i.backend != nil {
		data.BackendType = i.backend.Type()
		// Project the per-kind tab verdict rather than leaving clients to infer one
		// from BackendType (#3060). Computed here, on the snapshot every surface
		// reads, so the TUI, the web UI and the API all get the same answer.
		data.TabKinds = tabKindAllowances(i.capabilitiesLocked())
		// NOT Capabilities.TabManagement any more. tabMutationTarget dropped that gate
		// when the first off-box tab was admitted (#3062), so projecting it would tell
		// clients to hide close/rename/reorder for tabs the daemon now accepts —
		// the drift this projection exists to prevent, running backwards. Archived is
		// the remaining refusal and is checked by its own path on both sides.
		rosterMutable := true
		data.TabRosterMutable = &rosterMutable
		tabManagement := i.capabilitiesLocked().TabManagement
		data.TabManagement = &tabManagement
		if provider, ok := i.backend.(runtimeCleanupProvider); ok {
			// Stage the exact off-box teardown identity privately on every snapshot.
			// ForStorage publishes it for a UserKilled tombstone, an unknown cleanup
			// outcome, and now ordinary sandbox rows (so a restart can rebuild the
			// handle and a later kill reaps for real) — including the
			// persistKillTombstone copy that flips the kill flag after this read.
			data.runtimeCleanup = provider.runtimeCleanupData()
		}
		// Stage the inert-at-kill marker privately on every snapshot of a sandbox
		// session whose off-box runtime POSITIVELY owes no teardown. The two
		// conditions are deliberately independent and BOTH must hold:
		//
		//   - owesNoRuntimeTeardownLocked(i.backend): positive evidence the backend
		//     itself carries no off-box cleanup identity. This is the EARNED part. A
		//     sandbox backend rebuilt by restoreRuntimeCleanup carries the cleanup
		//     handle it restored from disk (its runtimeCleanupData() is non-nil),
		//     and that handle means a runtime is still owed a reap — staging the
		//     marker from a nil runtimeTeardown ALONE would launder that still-live
		//     runtime into a no-op success and orphan it (the original f58ca97f
		//     wedge fix did exactly that, because reload produced a handle-less
		//     inert backend for every sandbox row regardless of whether its runtime
		//     survived the restart).
		//   - !i.runtimeCleanupStateUnknown: the cleanup state is KNOWN, not
		//     unknown. This is what distinguishes a discharged reap (the marker is
		//     earned — the reap answered definitively and latched, so the runtime
		//     is gone) from a rejected or unknown cleanup handle that fail-closed
		//     (a reload installs unavailableRuntimeCleanup and arms
		//     runtimeCleanupStateUnknown, which must NOT stage the marker — its
		//     reload keeps the record retained for a real retry instead of admitting
		//     a no-op delete). The previous gate read `i.runtimeTeardown == nil` as
		//     the "no reap is owed" signal, but the ArchiveSandbox known-failure
		//     branch (clearDischargedRuntimeCleanup) discharges the backend's
		//     cleanup identity after a latched definite reap while deliberately
		//     retaining the in-memory teardown closure for the abort-to-Lost retry
		//     (reapRemoteRuntimeForReplacement reaps through it). That closure is a
		//     stale re-entrant path for an already-reaped runtime, not a live reap
		//     obligation, so a kill tombstone written in that state would otherwise
		//     carry neither RuntimeCleanup nor the marker and fail closed on a
		//     crash-after-tombstone reload — even though the reap already answered.
		//     Reading the cleanup state instead of the teardown pointer admits that
		//     discharged shape while a genuinely unknown cleanup (the sentinel
		//     teardown a rejected handle installs, or markRuntimeCleanupStateUnknown)
		//     still refuses the marker, because its reload must keep the record
		//     retained for a real retry.
		//
		// ForStorage publishes the staged marker only for a UserKilled tombstone,
		// where it records that the kill was committed against a rebuild that owed
		// no off-box reap and the tombstone therefore carries no RuntimeCleanup
		// handle on purpose. The marker is the durable token FromInstanceData reads
		// to admit that "nothing owed" shape — the sandbox analog of the local
		// branch's worktreeReaped admission (#2691) — so a handle-less tombstone
		// that survives a daemon restart can still complete its pending record
		// delete instead of installing a permanent fail-closed teardown. A local
		// backend never reaches here: it has no off-box runtime, and its reaped
		// shape is read from its worktree paths.
		if isSandboxBackendType(data.BackendType) && owesNoRuntimeTeardownLocked(i.backend) && !i.runtimeCleanupStateUnknown {
			marker := true
			data.killedWithoutLiveRuntime = &marker
		}
	}

	// Persist the usage-limit reset time only while the session is actually
	// limit-blocked (#1146). Gating on the liveness keeps a recovered session
	// from carrying a stale reset time to disk or into the snapshot — the
	// in-memory field lingers after ClearLimitReached but is never serialized.
	if i.liveness == LiveLimitReached {
		data.LimitResetAt = i.limitResetAt
		data.LimitAgent = i.limitAgent
		data.LimitAccount = i.limitAccount
	}

	// Unlike the reset time above, this is NOT gated on a liveness: whether the run
	// is still in flight is meaningful in every state (#1892), and gating it would
	// reintroduce the bug it fixes — a session whose run is live must read as active
	// whether it is Running, limit-parked, mid-archive, or Lost.
	data.TaskRunActive = i.taskRunActive
	// Not gated on taskRunActive like the held flags below it: an interrupted
	// run has already spent its active marker, and the interruption is exactly
	// the fact startup reconcile needs after a restart (#4222 review).
	data.TaskRunInterrupted = i.taskRunInterrupted
	data.TaskRunInterruptedAt = i.taskRunInterruptedAt
	data.TaskRunFailurePending = i.taskRunFailurePending
	data.BacklogItem = i.BacklogItem
	data.CreatedBySandbox = i.createdBySandbox
	data.CreatedByOperator = i.createdByOperator
	data.BacklogRunActive = i.backlogRunActive
	data.BacklogRunEndedAt = i.backlogRunEndedAt
	gate := i.runGateActiveLocked()
	data.TaskRunIdleEdgeHeld = gate && i.taskRunIdleEdgeHeld
	data.TaskRunTurnGateHeld = gate && i.taskRunTurnGateHeld
	if gate {
		// Same gating as the held flag: a finished run's last turn observation
		// is not evidence for anything, and persisting it would let a later run
		// on this row inherit a boundary it never crossed.
		data.TaskRunTurnObservedAt = i.taskRunTurnObservedAt
		data.TaskRunPromptAttemptAt = i.taskRunPromptAttemptAt
		data.TaskRunPromptBound = i.taskRunPromptBound
	}

	// An archived row cannot owe its own teardown — reaching Archived IS the
	// discharge. Any other state may legitimately carry the obligation across a
	// restart, and must (#4162): the completion edge that armed it cannot
	// re-fire, so dropping the marker here would lose it permanently.
	if i.liveness != LiveArchived {
		data.PendingOnComplete = i.owedOnComplete
	}

	// Persist each tab so the full local agent+shell tab list survives a restart
	// (Sachin's hard requirement for #930): on reload FromInstanceData restores
	// each local tab's tmux session by its exact persisted name. An off-box
	// session's fixed Agent tab has no daemon-side tmux, so it serializes with an
	// empty TmuxName and is re-seeded when the AgentServer is launched.
	// Staged-but-undrained rows are persisted in their OWN field. A sandbox recovery
	// that fails BEFORE Launch leaves them only in pendingMetadataTabs, and both
	// failure paths persist whatever this returns — so dropping them loses the tab
	// permanently. Appending them to Tabs is equally wrong: Tabs reserves index 0
	// for the agent, and a failed recovery leaves it empty, so a staged web tab
	// would land in that slot (#3062).
	data.PendingTabs = append(data.PendingTabs, i.pendingMetadataTabs...)
	for _, tab := range i.Tabs {
		data.Tabs = append(data.Tabs, i.tabDataLocked(tab))
	}
	// An archived off-box row is inert, but the web rail still renders its
	// metadata-only tabs as restore placeholders. After a daemon restart those
	// rows live in pendingMetadataTabs, outside the ordered roster by design; make
	// them visible only in the snapshot projection, behind an implicit agent row.
	// ForStorage restores snapshotStorageTabs before writing instances.json, so
	// durability keeps using PendingTabs and never reintroduces the index-0 bug.
	if len(i.pendingMetadataTabs) > 0 {
		data.snapshotTabsProjected = true
		data.snapshotStorageTabs = data.Tabs
		data.Tabs = append([]TabData(nil), data.Tabs...)
		if len(data.Tabs) == 0 {
			data.Tabs = append(data.Tabs, TabData{Name: "agent", Kind: TabKindAgent})
		}
		for _, td := range i.pendingMetadataTabs {
			if !isRestorableRow(td) {
				data.Tabs = append(data.Tabs, td)
			}
		}
	}

	// Cleanup handles for tabs whose roster removal is durable but whose tmux
	// teardown was never confirmed (#2669). Projected alongside Tabs, and from the
	// same critical section, so a snapshot can never show a tab as both gone and
	// untracked: whichever side of the close the read lands on, exactly one of the
	// two lists names the tmux session.
	if len(i.pendingTabCleanup) > 0 {
		data.PendingTabCleanup = append([]TabCleanupData(nil), i.pendingTabCleanup...)
	}
	data.PendingRemoteTabClose = i.remoteCloseDataLocked()

	// Keep writing the legacy single TmuxName field (set from the agent tab) for
	// one release: a binary rolled back to before #930 PR 2 still finds the
	// agent session by its exact name, and old readers ignore the new Tabs list.
	if ts := i.tmuxLocked(); ts != nil {
		data.TmuxName = ts.SanitizedName()
	}
	if len(i.Tabs) > 0 {
		data.AgentConversation = conversationDataPtr(i.Tabs[0].Conversation)
	} else if i.reportedConversation.HasID() {
		// A reloaded off-box row has no agent tab until Launch seeds one, but it
		// still holds the conversation its agent-server reported (#5826): writing
		// the row back must not drop the resume handle.
		data.AgentConversation = conversationDataPtr(i.reportedConversation)
	}

	// Only include worktree data if gitWorktree is initialized
	if i.gitWorktree != nil {
		branchCreatedByUs := i.gitWorktree.BranchCreatedByUs()
		externalWorktree := i.gitWorktree.IsExternalWorktree()
		startupStateUnknown := i.startupStateUnknown
		operation := archiveWarningOperation(i.liveness)
		worktreePath, recovery, hasRecovery, hasArchiveReport, archiveWarning :=
			i.gitWorktree.ProjectionSnapshot(operation)
		data.archiveReportSource = i.gitWorktree
		data.archiveReportPending = hasArchiveReport
		if hasArchiveReport {
			data.ArchiveWarning = archiveWarning
		}
		// ExternalWorktree is true for in-place sessions (`af sessions create
		// --here`, which attach to the repo's own working tree) and for
		// instances persisted by the pre-#930-PR-3 create-on-existing-worktree
		// feature. Cleanup() honors it by skipping removal of the user-owned
		// worktree+branch. (BranchCreatedByUs is independent — it also flips
		// false on the normal path when Setup reuses an existing branch; see
		// git/worktree_ops.go setupFromExistingBranch.)
		data.Worktree = GitWorktreeData{
			RepoPath:          i.gitWorktree.GetRepoPath(),
			WorktreePath:      worktreePath,
			SessionName:       i.Title,
			BranchName:        i.gitWorktree.GetBranchName(),
			BaseCommitSHA:     i.gitWorktree.GetBaseCommitSHA(),
			ExternalWorktree:  externalWorktree,
			BranchCreatedByUs: &branchCreatedByUs,
			// Empty until a hook actually entered a scope, and omitempty then keeps
			// it out of the record entirely — so "no scope" stays indistinguishable
			// from a pre-#3650 record, which is what makes absence mean today's
			// behaviour for both.
			HookScopeUnitPrefix: i.gitWorktree.HookScopeUnitPrefix(),
			// Durable record state, not a projection: the poll that maintains it
			// runs only in the daemon, so a restart must come back still knowing
			// the worktree was deleted outside af until a probe proves otherwise
			// (#5102).
			Missing:       i.worktreeMissing,
			MissingReason: i.worktreeMissingReason,
		}
		if hasRecovery {
			data.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{
				State:                       recovery.State,
				AlternatePath:               recovery.AlternatePath,
				IdentityKnown:               recovery.IdentityKnown,
				Device:                      recovery.Device,
				Inode:                       recovery.Inode,
				FileType:                    recovery.FileType,
				CleanupGeneration:           recovery.CleanupGeneration,
				OriginalExternalWorktree:    &externalWorktree,
				OriginalBranchCreatedByUs:   &branchCreatedByUs,
				OriginalStartupStateUnknown: &startupStateUnknown,
			}
		}
	} else if isSandboxBackendType(data.BackendType) {
		// An off-box session owns no local worktree, but it still belongs to the
		// repo its create was scoped to. The pending-create row publishes the
		// repo's canonical IdentityPath as Worktree.RepoPath (manager_create);
		// carrying the same value on the settled projection keeps repo-scoped
		// consumers — the web rail's project filter and the project switcher —
		// able to attribute the session, and keeps the pending→settled hand-off
		// stable when IdentityPath differs from the operational workspace Path
		// (a bare repository's linked-worktree registration). Without it a remote
		// session matched NO project's scope and the web could not render or keep
		// it selected at all (#1933). No Path fallback: consumers treat a nonempty
		// RepoPath as already canonical (api/sessionRepoID hashes it verbatim), so
		// publishing the operational workspace under a bare+linked registration
		// would move the row to a bogus project — for a record that never carried
		// an identity, empty stays honest and matches the pre-#1933 projection.
		data.Worktree.RepoPath = i.repoIdentityPath
	}
	if i.backend != nil {
		data.Placement = i.placementLocked(i.backend, data.BackendType, data.Worktree.WorktreePath)
	}

	return noteHostLayout(data)
}

// RestoreRelocationRecoveryOriginals reverses the rollback-safe ownership and
// lifecycle projection made by ForStorage before a current reader interprets a
// recovery record. Normal instance loading and daemon ghost cleanup share this
// boundary so neither can read compatibility fields as deletion authority.
func (data InstanceData) RestoreRelocationRecoveryOriginals() (InstanceData, error) {
	recovery := data.Worktree.RelocationRecovery
	if recovery == nil {
		return data, nil
	}
	recoveryClone := *recovery
	data.Worktree.RelocationRecovery = &recoveryClone
	recovery = &recoveryClone
	// First, so the shape check below never mistakes a real stall projected
	// through the worktree-missing fence for the bare fence itself.
	if err := restoreMissingFencedState(recovery); err != nil {
		return InstanceData{}, err
	}
	state, err := runtimeRelocationRecoveryState(recovery)
	if err != nil {
		return InstanceData{}, err
	}
	originalExternal := recovery.OriginalExternalWorktree
	originalBranch := recovery.OriginalBranchCreatedByUs
	originalStartup := recovery.OriginalStartupStateUnknown
	if recovery.CleanupLifecycle != "" {
		originalExternal = recovery.CleanupOriginalExternalWorktree
		originalBranch = recovery.CleanupOriginalBranchCreatedByUs
		originalStartup = recovery.CleanupOriginalStartupStateUnknown
	}
	if originalExternal == nil || originalBranch == nil || originalStartup == nil {
		return InstanceData{}, fmt.Errorf("rollback safety metadata is missing")
	}
	data.Worktree.ExternalWorktree = *originalExternal
	branchCreatedByUs := *originalBranch
	data.Worktree.BranchCreatedByUs = &branchCreatedByUs
	data.StartupStateUnknown = *originalStartup

	// Return a canonical current-reader record. Ghost checkpoint writers persist
	// this row directly, so leaving the previous release's safe ownership values
	// in Original* would make the next ForStorage projection forget the real owner.
	recovery.State = state
	recovery.CleanupLifecycle = ""
	recovery.OriginalExternalWorktree = cloneBoolPointer(originalExternal)
	recovery.OriginalBranchCreatedByUs = cloneBoolPointer(originalBranch)
	recovery.OriginalStartupStateUnknown = cloneBoolPointer(originalStartup)
	recovery.CleanupOriginalExternalWorktree = nil
	recovery.CleanupOriginalBranchCreatedByUs = nil
	recovery.CleanupOriginalStartupStateUnknown = nil
	// The worktree-missing rollback projection is not a relocation record; with
	// the ownership values restored above, it reads back as the flag it stands
	// for (#5102). An older binary that rewrote the row dropped the flag and its
	// reason but kept this record, which is why the shape, not Missing, decides.
	if isWorktreeMissingRollbackFence(recovery) {
		data.Worktree.RelocationRecovery = nil
		data.Worktree.Missing = true
		if data.Worktree.MissingReason == "" {
			data.Worktree.MissingReason = WorktreeMissingDeletedReason(data.Worktree.WorktreePath)
		}
	}
	return data, nil
}

// FromInstanceData creates a new Instance from serialized data
func FromInstanceData(data InstanceData) (*Instance, error) {
	data = noteHostLayout(data.RestoreArchiveRollbackFence())
	// ForStorage publishes an ordinary sandbox row's RuntimeCleanup under a
	// projected RuntimeCleanupStateUnknown=true so a binary rolled back to the
	// preceding release rebuilds and preserves the handle instead of loading the
	// row inert and orphaning the runtime. Restore the real value before any
	// cleanup-state decision reads it; the preceding release ignores the fence
	// field, so a current reader sees the ordinary row it is.
	data = data.RestoreRuntimeCleanupRollbackFence()
	var err error
	data, err = data.RestoreRelocationRecoveryOriginals()
	if err != nil {
		return nil, fmt.Errorf("failed to restore worktree relocation recovery: %w", err)
	}
	// Rollback fences unwind in REVERSE projection order — each restore writes
	// back the original its projector captured, so the LAST projector's restore
	// must run first or it would overwrite the real value with a projected one.
	// ForStorage projects account-swap, then handoff, then rebuild-teardown.
	data = data.RestoreRebuildTeardownRollbackFence()
	data = data.RestoreHandoffRollbackFence()
	data = data.RestoreAccountSwapRollbackFence()
	data = data.restoreMissingHandoffMissionEvidence()
	data = data.restoreMissingAccountSwapMissionEvidence()
	id := data.ID
	if id == "" {
		// Legacy records predate stable session identity. Materialized instances
		// must still have an identity so every caller can use ID-first actions;
		// the daemon load path durably backfills this before calling us.
		id = NewInstanceID()
	}
	// Resolve the liveness axis via the shared rollforward (#1108/#1195): prefer
	// the new `liveness` field, fall back to the legacy `status` int for records
	// written before #1195, and load a persisted Dead as Lost — recovery-
	// eligible, which is what makes sessions stranded by an outage under an old
	// build restorable after an upgrade. A tombstoned record keeps its (Lost)
	// status; the daemon finishes its teardown rather than restoring it.
	liveness := livenessFromData(data)
	limitAccount, accountLimitObservations := AccountLimitEvidenceFromData(data)
	limitAgent := limitAgentFromData(data, limitAccount, accountLimitObservations)
	// Resolve the in-flight-op axis from the snapshot payload, falling back to
	// the legacy status for old daemons/records. A persisted record is always
	// settled (disk writers scrub this field and SaveInstances skips
	// Loading/Deleting), but a SNAPSHOT can catch archive/restore in progress and
	// must round-trip the exact op (#1436).
	inFlightOp := inFlightOpFromData(data)
	// PendingHandoffMission is the one operation marker that IS durable: it says
	// an irreversible runtime swap completed but its takeover brief did not. Disk
	// still scrubs the generic op enum, then this specific proof reconstructs the
	// replacement fence — but only for the verdicts that still own an in-flight
	// obligation the daemon resolves itself (#4429): positive non-delivery (the
	// automatic replay) and the delivered crash window (the recovery settle).
	//
	// An ambiguous verdict loads without the fence. The send path writes its
	// could-not-confirm attempt marker only after readiness has proved the
	// incoming runtime, and before the composer is touched. A crash inside the
	// readiness wait therefore reloads the verdict that admitted the attempt,
	// fence included. An ambiguous verdict can also come from a readiness
	// failure, which a fenced attempt records together with startup-unknown
	// (refused below either way), or from a legacy record that had no verdict.
	// However it arose, the row is in the same position: nothing resends the
	// mission automatically, and both operator exits prove the runtime before
	// acting — confirm probes the pane, and retry probes a startup-unknown row
	// and then re-runs readiness. Rebuilding the fence would only make the row
	// inert, which is the wedge this issue fixes.
	//
	// A kill tombstone outranks every process-local op, including one carried by
	// a live snapshot; startup-unknown likewise prevents synthesizing a
	// replacement fence. Both terminal markers must retain an explicit teardown
	// handle rather than load as an in-flight replacement.
	if data.UserKilled {
		inFlightOp = OpNone
	} else if data.PendingHandoffMission != "" && !data.StartupStateUnknown && inFlightOp == OpNone &&
		pendingHandoffMissionNeedsFence(data.HandoffDeliveryStatus) {
		inFlightOp = OpReplacing
	}
	// Legacy records retain their last save time. Only truly missing timestamps
	// fall back to creation; serialization never invents activity.
	if data.UpdatedAt.IsZero() {
		data.UpdatedAt = data.CreatedAt
	}
	taskRunAttemptAt := taskRunPromptAttemptAtFromData(data)
	instance := &Instance{
		ID:         id,
		TaskID:     data.TaskID,
		Title:      data.Title,
		Path:       data.Path,
		Branch:     data.Branch,
		liveness:   liveness,
		inFlightOp: inFlightOp,
		// Carried across the restart (#1892). An outage that loses sessions is the
		// same event that restarts the daemon, so this fact has to come back from
		// disk or the cap would re-decide it from a Lost state that cannot tell a
		// finished run from an interrupted one.
		taskRunActive:        data.TaskRunActive,
		BacklogItem:          data.BacklogItem,
		createdBySandbox:     data.CreatedBySandbox,
		createdByOperator:    data.CreatedByOperator,
		backlogRunActive:     data.BacklogRunActive && data.BacklogItem != 0,
		backlogRunEndedAt:    data.BacklogRunEndedAt,
		taskRunInterrupted:   data.TaskRunInterrupted && !data.TaskRunActive,
		taskRunInterruptedAt: data.TaskRunInterruptedAt,
		taskRunIdleEdgeHeld:  runGateActiveInData(data) && data.TaskRunIdleEdgeHeld,
		taskRunTurnGateHeld:  runGateActiveInData(data) && data.TaskRunTurnGateHeld,
		// Gate on the run still being in flight, mirroring the write side and
		// the held flag: a finished run's stale observation cannot release the
		// next run's delivery window.
		taskRunTurnObservedAt:    taskRunTurnObservedAtFromData(data),
		taskRunPromptAttemptAt:   taskRunAttemptAt,
		taskRunPromptBound:       data.TaskRunActive && data.TaskRunPromptBound,
		limitResetAt:             data.LimitResetAt,
		limitAgent:               limitAgent,
		limitAccount:             limitAccount,
		accountLimitObservations: accountLimitObservations,
		agentModelChange:         agentModelChangeForLiveness(data.ModelChange, liveness),
		archiveWarning:           data.ArchiveWarning,
		callbackUnavailable:      data.CallbackUnavailable,
		agentConfigSkipped:       append([]string(nil), data.AgentConfigSkipped...),
		lostRestoreFailure:       lostRestoreFailureFromData(data.LostRestoreFailure),
		lostRestoreWait:          lostRestoreWaitFromData(data.LostRestoreWait),
		rebuildTeardownPending:   data.RebuildTeardownPending,
		unreachableSince:         unreachableSinceFromData(data.Unreachable),
		unreachableErr:           unreachableErrFromData(data.Unreachable),
		lastPromptAttemptAt:      data.LastPromptAttemptAt,
		lastPromptDeliveryStatus: data.LastPromptDeliveryStatus,
		lastPaneChurnAt:          data.LastPaneChurnAt,
		Height:                   data.Height,
		Width:                    data.Width,
		CreatedAt:                data.CreatedAt,
		UpdatedAt:                data.UpdatedAt,
		archivedAt:               data.ArchivedAt,
		prune:                    pruneState{nonce: data.ArchiveNonce, prunedAt: data.PrunedAt, intent: clonePruneIntent(data.PruneIntent)},
		Program:                  data.Program,
		runtimeProgram:           data.RuntimeProgram,
		runtimePID:               data.RuntimePID,
		runtimeStartID:           data.RuntimeStartID,
		Account:                  data.Account,
		Host:                     data.Host,
		branchUnpushed:           data.BranchUnpushed,
		archivedWorkspace:        cloneArchivedWorkspace(data.ArchivedWorkspace),
		placement:                clonePlacement(data.Placement),
		projectedTabManagement:   data.TabManagement != nil && *data.TabManagement,
		accountAgent:             data.AccountAgent,
		accountAutoSelected:      data.AccountAutoSelected,
		pendingAccountSwap:       cloneAccountSwapData(data.PendingAccountSwap),
		Prompt:                   data.Prompt,
		pendingHandoffMission:    data.PendingHandoffMission,
		handoffDeliveryStatus:    data.HandoffDeliveryStatus,
		userKilled:               data.UserKilled,
		startupStateUnknown:      data.StartupStateUnknown,
		// Survives the restart on purpose (#2629): a root that came back amnesiac
		// is still amnesiac, and a daemon restart is a likely part of the same
		// outage. An unrecognized value from a newer binary loads as-is and
		// renders nothing, which is the rollforward-safe direction.
		rootRecreateContext: data.RootRecreateContext,
		attention:           cloneAttention(data.Attention),
	}
	// The restart twin of the live edge (#5278): a run persisted mid-flight
	// whose prompt boundary reloads unbound AND whose last send verdict is a
	// proven non-delivery had its prompt demonstrably never land — its initial
	// delivery failed. End it failed here: left open, the row holds its
	// concurrency slot forever on a gate that can never arm, and a Ready→Ready
	// observation could still be claimed as its completion. A tombstoned,
	// archived, or startup-unknown row keeps its own outcome instead.
	if instance.taskRunActive && taskRunAttemptAt.IsZero() &&
		data.LastPromptDeliveryStatus == PromptNotDelivered &&
		!data.TaskRunPromptBound &&
		!instance.userKilled && !instance.startupStateUnknown && liveness != LiveArchived {
		instance.taskRunActive = false
		instance.taskRunIdleEdgeHeld = false
		instance.taskRunTurnGateHeld = false
		instance.taskRunFailed = true
		instance.taskRunFailedStamp = true
		instance.taskRunFailurePending = true
	}
	// The restart twin of the durable flag (#5646): the failure edge fired and
	// persisted TaskRunActive=false before the daemon stamped the task, so the
	// re-derivation above (which needs the run still active) cannot see it.
	// Re-arm the stamp; the daemon's write is a compare-and-set and idempotent.
	if data.TaskRunFailurePending && !instance.userKilled && liveness != LiveArchived {
		instance.taskRunFailed = true
		instance.taskRunFailedStamp = true
		instance.taskRunFailurePending = true
	}
	instance.dropAttentionIfStoppedLocked()
	instance.runtimeCleanupStateUnknown = data.RuntimeCleanupStateUnknown
	// The pending on_complete obligation rides the restart so the daemon can
	// re-drive the teardown it could not finish (#4162). An archived row cannot
	// owe its own teardown — mirroring the serialize gate — so a marker that
	// somehow reached one is dropped rather than armed.
	if liveness != LiveArchived {
		instance.owedOnComplete = data.PendingOnComplete
	}
	worktreeReaped := false
	restoredRelocationRecovery := false

	// Pick backend based on persisted BackendType.
	switch {
	case isSandboxBackendType(data.BackendType):
		// A sandbox session (docker/ssh/sandbox/hook, #1592 Phase 4 PR6/PR7) has no
		// daemon-side worktree or tmux to reconstruct — its workspace lived in a
		// container/remote/user-provisioned sandbox. The load itself is inert: an
		// ssh/sandbox row that persisted a runtime identity is reattached afterwards
		// by restoreInstances (W1.5); any other row relies on its pushed branch on
		// origin. Rebuild only an INERT sandbox backend so the row still classifies as its runtime
		// (Type/Capabilities) for archive + restore; the live runtime is
		// re-provisioned on restore (re-running launch_cmd for hook), never
		// reconstructed here.
		instance.backend = newInertSandboxBackend(data.BackendType)
		// Carry the recorded repo identity so the reloaded row keeps projecting
		// the same Worktree.RepoPath its writer stamped (#1933) — records from
		// before the field existed leave it empty and Path stands in.
		instance.repoIdentityPath = data.Worktree.RepoPath
		// A metadata-only tab needs nothing this branch cannot rebuild: a web tab is
		// a name and a URL, binding no tmux and reading no worktree. Dropping it made
		// off-box admission non-durable — the tab vanished at the next daemon restart
		// with nothing erroring, which is the harder failure to notice (#3062).
		//
		// STAGED, not appended: see pendingMetadataTabs. Writing into Tabs here would
		// stop Launch seeding the agent tab and leave a web tab in index 0.
		// From both sources: rows that were on the roster when the record was written,
		// and rows a previous recovery staged but never drained.
		instance.pendingMetadataTabs = append(metadataTabsFrom(data, instance.backend.Capabilities()), data.PendingTabs...)
		// The recorded conversation survives the reload (#5826): the agent tab is
		// seeded by the next Launch, so the handle waits in reportedConversation,
		// is sent on that launch, and is written back by every checkpoint meanwhile.
		instance.reportedConversation = persistedConversationOf(data)
		// A kill tombstone is a durable promise to FINISH teardown after a restart;
		// an unknown cleanup outcome is the same obligation without terminal kill
		// intent. An ordinary sandbox row carries a persisted RuntimeCleanup handle
		// (ForStorage publishes it for sandbox backends) precisely so this branch
		// can rebuild a real teardown after a restart — its off-box runtime
		// SURVIVES the restart, and without the handle a later kill would stage an
		// unearned KilledWithoutLiveRuntime marker from the inert backend's nil
		// runtimeTeardown alone and orphan the still-live runtime. Rebuild only
		// that teardown handle — no endpoint, tunnel, or live sandbox client is
		// dialled during load. The same handle now also carries the session's
		// reattach identity (#5571 W1.1): remote_addr, agent_server_version and
		// token_ref travel inside it, so the runtime-identity key in AF home can
		// re-derive the bearer token a restart needs without the token itself
		// ever touching the row. Archived sandboxes were already reaped before
		// becoming Archived, so deleting their record needs no remote cleanup
		// handle; the archived ForStorage branch scrubs RuntimeCleanup, so the
		// handle here is naturally nil for those rows — except a preserved
		// workspace (below).
		hadPersistedCleanupHandle := isSandboxBackendType(data.BackendType) && data.RuntimeCleanup != nil
		// A preserved unpushed archive keeps its handle on purpose (#5867): its
		// workspace is still on the host, and a kill of the row must reap it.
		preservedArchive := liveness == LiveArchived && hadPersistedCleanupHandle && data.keepsPreservedWorkspaceHandle()
		if preservedArchive || (liveness != LiveArchived && (data.UserKilled || data.RuntimeCleanupStateUnknown || hadPersistedCleanupHandle)) {
			backend, teardown, cleanupErr := restoreRuntimeCleanupWithAccount(data.Title, data.BackendType, data.RuntimeCleanup,
				restoredAccount{Name: data.Account, Agent: data.AccountAgent, Program: data.Program})
			if cleanupErr != nil {
				if data.UserKilled && data.RuntimeCleanup == nil && data.KilledWithoutLiveRuntime {
					// Inert-at-kill tombstone: the kill was committed against an inert
					// rebuild whose off-box runtime owed no teardown (nil runtimeTeardown),
					// so the tombstone carries no RuntimeCleanup handle on purpose. The
					// marker written at commit time distinguishes this current-binary
					// "nothing owed" shape from a legacy/malformed tombstone. Admit it the
					// way the local branch's worktreeReaped does (#2691): leave the inert
					// backend and a nil teardown, so finishUserKill's Kill runs the no-op
					// success path (deadRemoteAgentServer.Kill returns nil for a nil
					// teardown) and the pending record delete can proceed instead of
					// installing a permanent fail-closed teardown no retry, re-kill, or
					// restart can clear.
					instance.runtimeTeardown = nil
				} else {
					// Legacy/malformed retention records fail closed. Keeping the inert backend
					// preserves classification while this closure makes Kill return the
					// unknown-state sentinel, so finishUserKill retains the only record
					// instead of laundering a missing handle into no-op success.
					instance.runtimeTeardown = unavailableRuntimeCleanup(data.Title, data.BackendType, cleanupErr)
					// The inert backend carries no cleanup identity, so the next
					// checkpoint would otherwise publish no RuntimeCleanup AND clear the
					// unknown flag (an ordinary row reloads with both gone). A subsequent
					// restart would then load the row fully inert — runtimeTeardown nil
					// and a backend that positively owes nothing — stage the nothing-owed
					// marker, and let a no-op kill delete the only record pointing at a
					// runtime whose fate this first reload correctly could not determine.
					// Mark the cleanup state unknown durably so every later restart retries
					// this same fail-closed rebuild instead of laundering the rejected
					// handle into the orphan the marker exists to prevent.
					instance.runtimeCleanupStateUnknown = true
				}
			} else {
				instance.backend = backend
				instance.runtimeTeardown = teardown
			}
		}
	default:
		instance.backend = &LocalBackend{}

		// DESTRUCTION REQUIRES POSITIVE EVIDENCE (#1953). A missing
		// branch_created_by_us (written before the field landed 2026-04-17)
		// means we do not know who created the branch — and the only thing
		// this flag authorizes is `git branch -D`. Unknown provenance
		// therefore means KEEP: default to false.
		//
		// This inverts the original nil→true back-compat default, which
		// assumed every legacy record was a branch AF had created. It was
		// wrong for two legacy shapes that both persisted no flag:
		// attach-to-existing-worktree records (external_worktree=true,
		// 2026-03-03) and — with nothing external about them — any normal
		// linked worktree Setup built on a branch the user already had
		// (setupFromExistingBranch, 2025-07-23). Both are the user's
		// branches; the old default marked them AF-created and let reset and
		// kill/archive delete them.
		//
		// The cost is the opposite error: a legacy record whose branch AF
		// really did create is now never pruned, leaking an orphaned af-*
		// branch. That is deliberate — a leaked branch the user can see and
		// delete beats a deletion they cannot undo.
		branchCreatedByUs := false
		if data.Worktree.BranchCreatedByUs != nil {
			branchCreatedByUs = *data.Worktree.BranchCreatedByUs
		}

		// A completed local kill clears gitWorktree before deleting its durable
		// tombstone. If that delete fails, the shutdown checkpoint writes both
		// paths empty and the next daemon still needs the row so it can finish the
		// delete. Accept only that exact terminal shape: a live row or a partially
		// populated worktree remains unknown/corrupt and must not be treated as
		// proof that no worktree exists.
		worktreeReaped = data.UserKilled && data.Worktree.RepoPath == "" && data.Worktree.WorktreePath == ""
		// A startup-state-unknown record may carry only title/branch/name
		// evidence — a leftover whose worktree never registered. Its worktree
		// path is then legitimately empty, and failing here would make the row
		// unloadable: skipped with a warning on every refresh tick instead of
		// resting inert for inspection. Other row shapes keep the strict check —
		// a live or corrupt record's empty path must still fail loudly.
		if !worktreeReaped && !(data.StartupStateUnknown && data.Worktree.WorktreePath == "") {
			gw, err := git.NewGitWorktreeFromStorage(
				data.Worktree.RepoPath,
				data.Worktree.WorktreePath,
				data.Worktree.SessionName,
				data.Worktree.BranchName,
				data.Worktree.BaseCommitSHA,
				data.Worktree.ExternalWorktree,
				branchCreatedByUs,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to restore git worktree: %w", err)
			}
			if recovery := data.Worktree.RelocationRecovery; recovery != nil {
				recoveryState, stateErr := runtimeRelocationRecoveryState(recovery)
				if stateErr != nil {
					return nil, fmt.Errorf("failed to decode worktree relocation recovery: %w", stateErr)
				}
				if err := gw.RestoreRelocationRecovery(git.RelocationRecovery{
					State:             recoveryState,
					AlternatePath:     recovery.AlternatePath,
					IdentityKnown:     recovery.IdentityKnown,
					Device:            recovery.Device,
					Inode:             recovery.Inode,
					FileType:          recovery.FileType,
					CleanupGeneration: recovery.CleanupGeneration,
				}); err != nil {
					return nil, fmt.Errorf("failed to restore worktree relocation recovery: %w", err)
				}
				restoredRelocationRecovery = true
			}
			if data.ArchiveReport != nil {
				gw.RestoreArchiveReport(data.ArchiveReport.Clone())
			}
			// Name future scopes after this session, and carry back the handle to
			// any that a previous daemon generation left running (#3650). An empty
			// prefix — a legacy record, or one whose hooks never entered a scope —
			// leaves the sweep disabled, which is the pre-#3650 behaviour.
			gw.SetHookScopeSessionID(id)
			// Relocation recovery is a pending fence, not terminal state: the
			// adoption seam checks gw.HasUnresolvedRelocation without finishing
			// its journal. A later recovery pass may establish the owning path.
			gw.SetHookResumeDisabled(data.UserKilled || liveness == LiveArchived)
			gw.SetHookScopeUnitPrefix(data.Worktree.HookScopeUnitPrefix)
			instance.gitWorktree = gw
			instance.worktreeMissing = data.Worktree.Missing
			instance.worktreeMissingReason = data.Worktree.MissingReason
		}

		// Rebuild the instance's tab list from disk so every tab (agent + shell)
		// reconnects to its exact tmux session across an af/daemon restart — the
		// load-bearing #930 requirement. LocalBackend.Start(false) then restores
		// each tab's session.
		restoreLocalTabs(instance, data)
		// Carry unconfirmed tab-teardown handles across the restart (#2669). They
		// are restored NEXT TO the roster, never into it: the daemon's startup sweep
		// retries the kill, and a spawn reserves against them, but no tab is
		// rebuilt from a tombstone.
		instance.pendingTabCleanup = restoredTabCleanup(data.PendingTabCleanup)
	}
	// Pending off-box closes belong to the sandbox backends the branch above never
	// reaches (#5996): restored for every backend so a restart cannot drop the only
	// handle on a command still running in a sandbox.
	instance.pendingRemoteClose = restoredRemoteClose(data.PendingRemoteTabClose)

	// Every return below hands back a row no automatic restore can reach —
	// inert (started=false), Archived rather than Lost, or vetoed by
	// ValidateRuntimeAction — so a persisted environmental wait could never be
	// consumed by the Lost-restore loop and every client would keep reporting
	// "waiting on the environment" while nothing retries (#5295 review). The
	// wait means "an automatic retry is armed"; on an unreachable row that is
	// a lie, and an explicit restore starts a fresh episode anyway, so each
	// inert load drops it rather than mirror the stale record.
	inertReturn := func() *Instance {
		if instance.ClearLostRestoreWait() {
			// The wait was persisted, so clearing it only in memory leaves
			// the stale field in instances.json — a crash before the next
			// write would let the disk-fallback API re-derive a wait no
			// retry can consume. Enroll the load-runtime checkpoint so the
			// clearance is durable before the row installs (#5295 review).
			instance.markLoadRuntimeReplaced()
		}
		return instance
	}

	// The local runtime and worktree are already gone. Keep this terminal row
	// inert until the daemon retries only the failed record deletion; starting it
	// would synthesize a fresh conventional tmux name and make the retry target a
	// session that never belonged to this tombstone.
	if worktreeReaped {
		return inertReturn(), nil
	}

	// A relocation-recovery record says the persisted worktree path is not yet
	// authoritative. Keep every such local session inert on reload: Start(false)
	// would pass that path into tmux restore and could reconnect or respawn an
	// agent in a stale or replaced directory. An explicit bounded archive/restore
	// retry owns resolving the record before any runtime may use the path again.
	if restoredRelocationRecovery {
		return inertReturn(), nil
	}

	// An uncertain startup loads INERT. Re-running Start(false) would probe or
	// even re-spawn through the same tmux name whose identity was never confirmed,
	// converting a daemon restart into a second destructive guess. The restored
	// worktree and exact tmux binding above remain available for inspection and an
	// explicit user kill, while the daemon status loop leaves the record alone.
	if data.StartupStateUnknown {
		return inertReturn(), nil
	}

	// An automatic account choice is durable before its replacement starts. Keep
	// that crash-restored row inert but scheduler-eligible: status reconciliation
	// must not turn an absent replacement into Lost and route it through ordinary
	// conversation resume before the pending notice and task are delivered.
	if data.PendingAccountSwap != nil {
		instance.started = true
		return inertReturn(), nil
	}

	// An archived session (#1028) loads INERT: its tmux was torn down and its
	// worktree moved to the global archive dir at archive time, so there is
	// nothing to re-spawn or reconnect. Skipping Start leaves started=false and
	// no tmux binding, which is exactly what makes the status poll (skips
	// !Started), the Lost-restore loop (gates on ==Lost), and EnsureRootAgents
	// pass it by — the session sits quiescent until an explicit RestoreArchived.
	// This is also #970-consistent: a load must never itself un-archive a
	// session (no worktree move, no spawn) as a side effect. gitWorktree is
	// already bound above to the persisted (archived) path so restore knows
	// where the worktree currently lives; the Tabs list restored above is
	// tmux-less for the same reason (its TmuxName entries reference sessions
	// that no longer exist, and restoreLocalTabs only binds names, never spawns).
	if liveness == LiveArchived {
		return inertReturn(), nil
	}

	// A non-archived sandbox session (docker/ssh/sandbox/hook) cannot be driven on
	// load: the reattach (W1.5) is a separate step, run by restoreInstances after
	// the rows are published, and only for ssh/sandbox rows with a persisted
	// runtime identity. Everything else (docker, hook, pre-W1.1 rows, a failed
	// reattach) recovers by re-provisioning from the branch pushed to origin
	// (#1592 Phase 4 PR6). Load it INERT + Lost — started stays
	// false, so the status poll and the Lost-restore loop both pass it by (the
	// #1028 started=false fence) — and let an explicit restore re-provision a fresh
	// sandbox from the branch. Skipping Start also avoids driving the recursive
	// remote backend with no live agent-server client.
	if isSandboxBackendType(data.BackendType) {
		instance.mu.Lock()
		if instance.liveness != LiveLost {
			instance.assignLivenessLocked(LiveLost)
			instance.touchLocked()
		}
		// Attention rides this provisional Lost on purpose: the daemon reattaches
		// the row to the agent that is still running and waiting for the user
		// (ReattachSandbox). SettleAttentionAfterReattach drops it when that
		// reattach does not bring the row back to a live state.
		instance.mu.Unlock()
		return inertReturn(), nil
	}

	// A rebuild-teardown obligation means every tmux name this row binds was
	// last observed inside the deleted inode: Start(false) would reattach
	// them before the teardown owner retries, which is the stale-pane bind
	// the flag exists to prevent (#5371 review). Load the row materialized
	// but unrestored — started, so storage keeps it and whichever loop owns
	// its liveness stays eligible: the lost-restore loop for a Lost row, the
	// limit-resume scheduler for a limit-parked one (each requires Started),
	// and the status poll for anything else — a positive observation is held
	// at Lost by the pending gate until the teardown discharges.
	if data.RebuildTeardownPending {
		instance.mu.Lock()
		instance.started = true
		instance.touchLocked()
		instance.mu.Unlock()
		return instance, nil
	}

	if err := instance.Start(false); err != nil {
		if retainsInertInstance(err) {
			// A sibling probe or the live agent's in-place scope upgrade did not
			// establish a safe runtime boundary. Keep the row inert and explicitly
			// killable so storage retains every exact tmux cleanup handle.
			instance.MarkStartupStateUnknown()
			return inertReturn(), nil
		}
		if restoreDeferred(err) {
			// The tmux session is gone and its persisted worktree is missing:
			// the spawn-dir seam refused a respawn that tmux would have
			// silently placed in the server's cwd (#5172). Load the row Lost
			// (started, so SaveInstances keeps the record and
			// lostSessionWantsRestore admits it) instead of dropping it — the
			// daemon's lost-restore loop owns the rebuild-then-respawn path,
			// and it can only reach a row that materialized.
			instance.mu.Lock()
			instance.assignLivenessLocked(LiveLost)
			instance.started = true
			instance.dropAttentionIfStoppedLocked()
			instance.touchLocked()
			instance.mu.Unlock()
			return instance, nil
		}
		return nil, err
	}

	return instance, nil
}

// restoreTmuxSession constructs a tmux session for an exact persisted name. It
// is a package var (not a direct call) so restore-survival tests can inject
// mock-backed sessions and stay hermetic; production uses the real constructor.
var restoreTmuxSession = tmux.NewTmuxSessionFromSanitizedName
