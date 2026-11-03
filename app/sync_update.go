package app

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// updateInstanceFromSnapshot reconciles an existing row's tab list and
// metadata to the snapshot IN PLACE (same pointer, so view state survives).
// Returns whether anything changed.
func (m *home) updateInstanceFromSnapshot(inst *session.Instance, d session.InstanceData) bool {
	changed := false
	if inst.ReconcilePlacementSnapshot(d.Placement) {
		changed = true
	}
	if inst.ReconcileTabManagementSnapshot(d.TabManagement) {
		changed = true
	}
	// A combined agent+account handoff replaces the process behind the same row
	// and tmux address. Reconcile every source CurrentAgentName consults before
	// later action predicates choose which agent's accounts to load.
	if inst.ReconcileAgentRuntimeSnapshot(d) {
		changed = true
	}
	// Account identity and its pending delivery obligation can change while the
	// row stays Running/Ready. Mirror them before action predicates are evaluated,
	// so Retry addresses the daemon's transaction instead of stale local state.
	if inst.ReconcileAccountHandoffSnapshot(d.Account, d.AccountAgent, d.AccountAutoSelected, d.PendingAccountSwap) {
		changed = true
	}
	if inst.ReconcilePendingHandoffSnapshot(d.PendingHandoffMission, d.HandoffDeliveryStatus) {
		changed = true
	}
	// Startup-unknown is a fail-closed, monotonic outcome for this live runtime.
	// Marking it locally also clears the stale started binding, matching a cold
	// materialization of the same snapshot.
	if d.StartupStateUnknown && !inst.StartupStateUnknown() {
		inst.MarkStartupStateUnknown()
		changed = true
	}
	// Mirror the daemon's durable mechanical evidence BEFORE the liveness
	// transition below (#5221 review): the task-run completion gate consults it
	// inside the transition, and a Ready snapshot can carry a turn observation
	// the edge needs to see. The run marker itself is mirrored outright — a
	// client that missed the snapshot carrying the evidence (always possible:
	// the final Ready snapshot scrubs the window fields with the run) cannot
	// re-derive the end locally, so the daemon's decision is authoritative.
	if inst.ReconcileIdleEvidence(d.LastPromptAttemptAt, d.LastPromptDeliveryStatus, d.LastPaneChurnAt,
		d.TaskRunPromptAttemptAt, d.TaskRunTurnObservedAt) {
		changed = true
	}
	if inst.ReconcileTaskRunState(d.TaskRunActive, d.TaskRunIdleEdgeHeld, d.TaskRunTurnGateHeld) {
		changed = true
	}
	if inst.ReconcileBacklogRunState(d.BacklogRunActive) {
		changed = true
	}
	changed = inst.ReconcileBacklogArchiveSnapshot(d.BacklogArchive) || changed
	// The outage flag is daemon truth on every snapshot: this is the reconcile
	// path for rows the snapshot updates IN PLACE, where FromInstanceData never
	// runs (#5571 W1.3).
	if inst.ReconcileUnreachable(d.Unreachable) {
		changed = true
	}
	// Mirror the daemon's authoritative LIVENESS onto the row (#960 PR 5, #1195):
	// the daemon poll computes Running/Ready/Lost/Archived/LimitReached (the #935
	// liveness) and the TUI renders it. Applied UNCONDITIONALLY — daemon liveness
	// always wins — which is what makes #1187 structurally impossible: a
	// locally-archiving row still receives the terminal Archived instead of being
	// stranded. The op axis is reconciled separately below so a liveness write
	// never clobbers a local optimistic marker by accident.
	lv := snapshotLiveness(inst.GetLiveness(), d)
	if inst.GetLiveness() != lv {
		_ = inst.Transition(session.ObserveLiveness(lv))
		changed = true
	}
	// The agent's attention request is the same class of projection-only axis
	// (#5461): it appears on `attention --self` and clears on the daemon's own
	// rules while liveness stays Ready/Running, so an already-open rail must
	// mirror it here or the row only learns of it at the next rebuild. It is
	// applied AFTER the liveness transition on purpose: a Ready→Running churn
	// transition the daemon deliberately marks attention-preserving
	// (settleChurnedLiveness) would otherwise let noteAttentionLivenessLocked
	// clear the just-mirrored request locally once its idle spell passes the
	// ten-second answer window, dropping the marker until the next poll
	// restores it. Daemon truth last — client-side clear rules may not
	// override a snapshot that still carries the request.
	if inst.ReconcileAttention(d.Attention) {
		changed = true
	}
	// A kill tombstone is durable while an operation marker is process-local.
	// Adopt it monotonically and clear any old marker under one session lock, so
	// a same-session row cannot briefly project a killed handoff as Loading. An
	// older snapshot with UserKilled=false cannot roll an adopted tombstone back.
	if inst.ReconcileUserKilledSnapshot(d.UserKilled) {
		changed = true
	}
	snapshotOp := d.InFlightOp
	tombstoned := d.UserKilled || inst.UserKilled()
	if tombstoned {
		snapshotOp = session.OpNone
	}
	// A retained tombstone can already be Lost/Archived before the user retries
	// its teardown. Those terminal liveness values therefore do not prove THIS
	// retry completed: success removes the record, while the RPC completion
	// clears the optimistic fence on failure. Preserve that local OpKilling until
	// one of those determinate outcomes arrives; the snapshot op still stays
	// scrubbed because no daemon process owns it after the tombstone commit.
	preserveKillRetry := tombstoned && inst.GetInFlightOp() == session.OpKilling
	if !preserveKillRetry && m.reconcileSnapshotOp(inst, snapshotOp, lv) {
		changed = true
	}
	// Mirror the usage-limit reset time (#1146) alongside the liveness. It's
	// display-only metadata that rides LiveLimitReached — which composes to Ready,
	// so the liveness compare above is what surfaces the [limit] badge; this only
	// keeps its "resets <t>" suffix fresh. Set it on its own axis (not via
	// SetLimitReached) so the liveness stays applied unconditionally above. When
	// the daemon has moved the session off LimitReached, the reset field is left
	// inert: LimitResetAt/ToInstanceData both gate on the liveness, so a stale
	// value can never leak to the badge or to disk.
	if lv == session.LiveLimitReached {
		if curReset, _ := inst.LimitResetAt(); !curReset.Equal(d.LimitResetAt) {
			inst.SetLimitResetAt(d.LimitResetAt)
			changed = true
		}
	}
	// Mirror the daemon's live model diagnostic on its own projection axis. It
	// can appear or clear while liveness stays Ready/Running, so folding it into
	// the status compare above would leave the TUI stale until an unrelated event.
	if inst.ReconcileAgentModelChange(d.ModelChange) {
		changed = true
	}
	// The incomplete-archive notice is another projection-only axis. Automatic
	// Lost recovery can add it while liveness stays Lost/Running, and retained-tree
	// cleanup can clear it without changing status, so same-row polls must mirror
	// both transitions explicitly.
	if inst.ReconcileArchiveWarning(d.ArchiveWarning) {
		changed = true
	}
	// The callback-absence reason is the same class of axis: a sandbox create
	// stamps it while liveness stays Ready/Running, and it never flips a status
	// on its own (#5571 W3.1).
	if inst.ReconcileCallbackUnavailable(d.CallbackUnavailable) {
		changed = true
	}
	// The skipped agent-config list is the same class again (#6337): a
	// (re)provision or an account swap rewrites it while liveness stays
	// Ready/Running, and a placement that lands every file clears it.
	if inst.ReconcileAgentConfigSkipped(d.AgentConfigSkipped) {
		changed = true
	}
	// The setup-hook failure note is the same class again (#6667): a provision
	// stamps it while liveness stays Ready/Running.
	if inst.ReconcileSetupHookFailed(d.SetupHookFailed) {
		changed = true
	}
	// Automatic restore can exhaust its attempts while liveness stays Lost. Mirror
	// that durable terminal axis independently so an already-open TUI surfaces the
	// reason immediately rather than only after cold-start materialization.
	if inst.ReconcileLostRestoreFailure(d.LostRestoreFailure) {
		changed = true
	}
	// The environmental wait is the same axis at the non-terminal end: it
	// appears while the loop defers a retry and disappears when an attempt
	// goes through or the failure turns structural (#5295). A row whose
	// tombstone was already adopted keeps it cleared: a delayed pre-kill
	// snapshot still carries the armed wait while its UserKilled=false
	// leaves the monotonic tombstone untouched, so applying the snapshot
	// value here would re-arm a wait no restore can ever consume — the
	// same lie the tombstone commit itself retires (#5295 review).
	wait := d.LostRestoreWait
	if tombstoned {
		wait = nil
	}
	if inst.ReconcileLostRestoreWait(wait) {
		changed = true
	}
	// Same shape for the re-created-root notice (#2629): it appears when the
	// daemon heals a root and clears when any client opens that session's pane,
	// both while the liveness sits at Ready. Reconciling it here is what makes an
	// already-open rail drop the note once the user has actually looked, instead
	// of showing it until the next restart.
	if inst.ReconcileRootRecreateContext(d.RootRecreateContext) {
		changed = true
	}
	// Backends without user-managed tabs (the off-box docker/ssh/sandbox/hook
	// runtimes) have a tab list their runtime owns: the agent tab, plus any
	// metadata-only external web tabs the daemon admits (#3062). There is no
	// local tmux session behind those for the snapshot to reconcile against. Skipping is a
	// no-op rather than a behavior change: ReconcileTabsFromData already returns
	// early for an instance with no local worktree, which is every sandbox
	// instance (#1874).
	{
		// Capture the slot→identity list before the tab reconcile mutates it: an
		// out-of-band tab change (another client, `af sessions tab-delete` /
		// `tab-rename`, daemon-side) must apply the SAME pane close/rebind
		// semantics as the TUI `w` kill, or an open pane is left showing a
		// shifted/stale tab — or, keyed by name, HIJACKED onto a different tab
		// that reused the freed name (#1088 + #960 + #1886/#1905). The stable id
		// in each key is what lets a pane follow its own tab across a rename or
		// close+recreate.
		oldKeys := paneTabKeys(inst)
		tabsChanged, err := inst.ReconcileTabsFromData(d.Tabs)
		if err != nil {
			log.WarningLog.Printf("failed to reconcile tabs for %q from snapshot: %v", d.Title, err)
		}
		if tabsChanged {
			changed = true
		}
		// Re-bind the roster's bindings whenever the tab IDENTITY list moved —
		// NOT merely when the reconcile reported a visible change. The two are
		// not the same: an id adoption is deliberately silent, and a binding
		// holding an id that just left the roster must still be repaired.
		// Gating this on tabsChanged left panes on dead ids whenever the roster
		// change didn't register as "visible" (#1886).
		if !sameTabSlotKeys(oldKeys, paneTabKeys(inst)) {
			// The tree's selection is the OTHER binding into that roster, and it
			// needs the SAME identity remap — a reorder lands entirely here (no
			// local action, #1813), and the clamps only keep the index in range,
			// which a permutation never violates, so an index-keyed selection
			// silently becomes a different tab.
			//
			// Resolve it BEFORE the pane reconcile's relayout: relayout re-reads
			// the sidebar (the footer's row-verb target), and a rebuild while
			// the cursor still holds its pre-shrink flat index drifts it onto the
			// NEXT row — the following session's instance row when the dropped
			// tab was the last child — which pushSelection commits as the new
			// display selection, leaving the remap below no session to repair
			// (#5298).
			if m.remapActiveTabForTabs(inst, oldKeys, sameSessionTabs) {
				// An in-place roster update never re-sorts the projection, so the
				// sidebar indices are current and can follow the remapped tab now.
				m.sidebar.SyncCursorToActiveTab()
				changed = true
			}
			if m.reconcilePanesForTabs(inst, oldKeys, sameSessionTabs) {
				m.relayout()
			}
		}
	}
	return changed
}
