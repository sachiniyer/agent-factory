package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
)

// InstanceData represents the serializable data of an Instance
type InstanceData struct {
	// ID is the instance's stable identity (#1195), minted at NewInstance and
	// used as the reconcile identity key. omitempty + additive: records written
	// before #1195 simply have no id, and the reconcile falls back to
	// title+CreatedAt for them (rollforward, mirroring the BranchCreatedByUs
	// precedent).
	ID string `json:"id,omitempty"`
	// TaskID is the id of the task whose delivery spawned this session, empty for
	// a user-created one (#1892). It is the daemon-owned association between a
	// task delivery and its session: the watch-task concurrency limit counts a
	// task's in-flight sessions by this field, never by a title prefix. A prefix
	// scan cannot do the job — nextAvailableTitleLocked auto-suffixes a taken base
	// to "<base>-2", which is indistinguishable from a session a user named
	// "<base>-2" themselves, and from a task whose name is another's prefix.
	// omitempty + additive: records written before #1892 simply have no task_id
	// and count against no limit (rollforward, mirroring the ID precedent above).
	TaskID string `json:"task_id,omitempty"`
	Title  string `json:"title"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Status is the legacy single-axis status int (#1195). Still written for one
	// release for rollback safety and read as the fallback source for records
	// that predate the `liveness` field. New code should read Liveness.
	Status Status `json:"status"`
	// Liveness is the daemon-owned health axis (#1195), the new canonical
	// persisted state. omitempty + additive: records written before #1195 have
	// no `liveness` key and decode to LivenessUnset, signaling FromInstanceData
	// to fall back to the legacy `status` int (rollforward).
	Liveness Liveness `json:"liveness,omitempty"`
	// InFlightOp is the transient operation axis (#1195/#1436) carried by the
	// daemon Snapshot so secondary TUIs can reconstruct non-round-trippable ops
	// exactly (OpArchiving vs OpKilling; OpRestoring vs plain Lost). It is scrubbed
	// at disk write/load boundaries: in-flight operations are process-local and
	// must not be resurrected after a daemon restart.
	InFlightOp InFlightOp `json:"in_flight_op,omitempty"`
	// LifecycleAction is a projection-only capability shared by the TUI and web
	// (#2234): "archive", "restore", or omitted when the row has no safe
	// lifecycle target (creating or id-less). It is derived from live state by
	// ToInstanceData and scrubbed by ForStorage; instances.json must not preserve
	// a UI decision that can go stale across restart.
	LifecycleAction LifecycleAction `json:"lifecycle_action,omitempty"`
	// CanKill is the independent projection-only teardown capability. It is true
	// for any stable, non-creating row, including StartupStateUnknown: that state
	// vetoes runtime reuse but must remain explicitly removable. Like
	// LifecycleAction, this is derived live and scrubbed before disk persistence.
	CanKill bool `json:"can_kill,omitempty"`
	// CanHandoff is the projection-only agent-swap capability shared by the TUI and
	// web (#2013): true when this session's agent can be handed off in place — a
	// local-worktree backend (Capabilities().Handoff) in a runtime state that admits
	// the swap (ValidateRuntimeAction(RuntimeActionHandoff)). It is derived live by
	// ToInstanceData from the SAME two predicates the TUI's handoff gate reads
	// (app/handle_handoff.go), so a browser — which cannot run those Go predicates —
	// renders the daemon's decision instead of re-deriving the rule. Scrubbed before
	// disk persistence like LifecycleAction/CanKill.
	CanHandoff bool `json:"can_handoff,omitempty"`
	// CurrentAgent is the agent enum this session is treated AS
	// (session.CurrentAgentName). Projection-only, carried so a client's handoff
	// picker can exclude the running agent exactly as the daemon's same-agent guard
	// does (session/handoff.go) — filtering on Program instead would drift from the
	// guard on a wrapper-script session. Derived live and scrubbed before disk.
	CurrentAgent string `json:"current_agent,omitempty"`
	// IsRoot is the projection-only reserved-root decision shared by the TUI and web
	// (#2513): the daemon's own session.IsReservedTitle applied to the title. It is
	// projected so the web pins root to the top of the rail (and draws the
	// demarcation rule) by CONSUMING the daemon's decision rather than
	// re-implementing IsReservedTitle in TypeScript against a duplicated title
	// constant — the exact one-concept-two-representations drift #2513 called out.
	// Derived live by ToInstanceData and scrubbed before disk like CanKill/CanHandoff.
	IsRoot bool `json:"is_root,omitempty"`
	// ModelChange is the projection-only agent diagnostic carried to the CLI,
	// TUI, and web row. It is derived from the live runtime's Observation and
	// scrubbed by ForStorage so a resolved or replaced process cannot inherit a
	// stale warning after daemon restart.
	ModelChange *AgentModelChange `json:"model_change,omitempty"`
	// IdleReason is the smallest mechanically established explanation for a
	// non-working row (#3168). It is derived from the evidence fields below and
	// never from pane wording. Projection-only: ForStorage scrubs it, while live
	// snapshots and daemonless list fallback recompute it with IdleReasonFor.
	IdleReason IdleReason `json:"idle_reason,omitempty"`
	// LostRestoreFailure persists why automatic recovery stopped across daemon
	// restarts until an explicit runtime replacement succeeds.
	LostRestoreFailure *LostRestoreFailure `json:"lost_restore_failure,omitempty"`
	// LastPromptAttemptAt orders the most recent actual prompt send against later
	// pane churn. Callers capture it before sending so churn racing the delivery is
	// still known to be later. Persisted across daemon restarts.
	LastPromptAttemptAt time.Time `json:"last_prompt_attempt_at,omitzero"`
	// LastPromptDeliveryStatus is the closed observation returned by the delivery
	// path. sent-unverified and could-not-confirm remain uncertainty (#3162), never
	// failed delivery.
	LastPromptDeliveryStatus PromptDeliveryStatus `json:"last_prompt_delivery_status,omitempty"`
	// LastPaneChurnAt is when the daemon most recently observed Observation.Updated.
	// It proves bytes changed, not who produced them or what they meant.
	LastPaneChurnAt time.Time `json:"last_pane_churn_at,omitzero"`
	// TaskRunActive records whether this session's task run is still in flight
	// (#1892) — true from creation, false once the agent goes idle or startup
	// settles terminal-unknown. It is the one fact the watch-task concurrency cap
	// counts, and it is stored rather than
	// re-derived because every neighbouring signal answers a different question:
	// Lost cannot tell a finished run from an interrupted one, and an in-flight op
	// means the DAEMON is busy (archiving a completed session is teardown, not
	// work). Both of those, read as "is the run in flight", let a run that already
	// finished reclaim a cap slot and park a task's events behind it.
	//
	// Persisted because an outage that loses sessions is the same event that
	// restarts the daemon, so an in-memory answer would be gone exactly when it is
	// needed. omitempty + additive: a record written without it decodes to false —
	// the session is treated as finished and holds no slot. That is the safe
	// direction for the one-time upgrade window (a daemon replaced mid-run reads its
	// in-flight sessions as done and may admit one extra event, which self-heals as
	// they finish); defaulting true would let a fleet of completed sessions load as
	// active and wedge a capped task permanently.
	TaskRunActive bool `json:"task_run_active,omitempty"`
	// LimitResetAt is the parsed usage-limit reset time (#1146), display-only:
	// written (and carried in the daemon snapshot to the read-only TUI) only for a
	// LiveLimitReached row so the sidebar [limit] badge can show "resets <t>" and
	// survive a restart, and so PR3's auto-resume scheduler can read it. omitempty
	// drops it for every normal session; additive + rollforward, mirroring the
	// Liveness precedent.
	LimitResetAt time.Time `json:"limit_reset_at,omitempty"`
	// LimitAccount is the identity that produced this limit observation. It may
	// differ from Account while a durably selected replacement is still starting.
	LimitAccount string    `json:"limit_account,omitempty"`
	Height       int       `json:"height"`
	Width        int       `json:"width"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Prompt       string    `json:"prompt,omitempty"`
	// PendingHandoffMission is a rendered takeover brief whose incoming runtime
	// has been established but whose delivery has not been durably confirmed.
	// Unlike Prompt, it is an at-least-once recovery marker and is cleared after
	// the exact mission lands (or is transferred to the usage-limit retry path).
	PendingHandoffMission string `json:"pending_handoff_mission,omitempty"`
	// PendingAccountSwap is the committed identity change whose replacement
	// runtime still needs the in-session notice and stored task delivered.
	PendingAccountSwap *AccountSwapData `json:"pending_account_swap,omitempty"`

	Program string `json:"program"`
	// Account is the credential account this session's provider panes use (#3051).
	// Persisted because the identity a session runs as must survive a daemon
	// restart and an archive/restore: a session that silently reverted to the
	// ambient account would spend the wrong quota while still displaying the
	// account it was created with.
	Account string `json:"account,omitempty"`
	// AccountAutoSelected is true only when af's opt-in limit scheduler chose the
	// account. Missing/false preserves every pre-#3127 account as an explicit pin.
	AccountAutoSelected bool `json:"account_auto_selected,omitempty"`
	// UserKilled is the kill-intent tombstone (#1108): persisted by
	// Manager.KillSession before teardown begins. Present only in the crash
	// window between tombstone write and record deletion — a surviving
	// tombstoned record means "finish this kill", never "restore this".
	UserKilled bool `json:"user_killed,omitempty"`
	// StartupStateUnknown retains a create that crossed the launch boundary but
	// whose runtime could not be confirmed. Unlike UserKilled, it does NOT commit
	// an automatic teardown: retrying the same uncertain binding could mistake a
	// differently stored tmux name for absence and delete a live workspace
	// (#2207). Additive + omitempty keeps older records unchanged.
	StartupStateUnknown bool      `json:"startup_state_unknown,omitempty"`
	TmuxName            string    `json:"tmux_name,omitempty"`
	Tabs                []TabData `json:"tabs,omitempty"`
	// PendingTabCleanup retains the tmux identity of tabs whose removal from Tabs
	// is already durable but whose teardown could not be confirmed (#2669). It is
	// the tab-scoped analogue of RuntimeCleanup, and it exists because CloseTab
	// commits the shrunken roster BEFORE killing tmux: without it, a kill-session
	// that times out (or answers while the session still exists) would drop the
	// closed tab's only tmux identity, leaking that process untracked forever and
	// letting a later same-named tab derive the same tmux name and collide with
	// the survivor. Entries are cleanup handles, never tabs — nothing renders or
	// respawns from them. Additive + omitempty on the TabData.ID rollforward
	// precedent: records written before this field simply have none.
	PendingTabCleanup []TabCleanupData `json:"pending_tab_cleanup,omitempty"`
	// AgentConversation mirrors the Agent tab's provider conversation id for
	// API/CLI consumers. The per-tab source of truth is TabData.Conversation.
	AgentConversation *AgentConversationData `json:"agent_conversation,omitempty"`
	// RootRecreateContext is the one-shot note a re-created root agent carries
	// when it did not demonstrably come back on its prior conversation (#2629):
	// the rails render it on the row so a root that lost its history is
	// discoverable in `af`, not only in the application log.
	//
	// PERSISTED, unlike the projection-only diagnostics above — ForStorage
	// deliberately leaves it alone. A root that came back amnesiac is still
	// amnesiac after a daemon restart, and the restart is a likely part of the
	// same outage; scrubbing it would erase the notice in exactly the situation
	// that produced it. It is cleared instead by acknowledgement — the first
	// time a client opens the session's pane. Additive + omitempty on the
	// TaskRunActive rollforward precedent: a record written before this field
	// decodes to no note.
	RootRecreateContext RootRecreateContext `json:"root_recreate_context,omitempty"`
	Worktree            GitWorktreeData     `json:"worktree"`
	PRInfo              PRInfoData          `json:"pr_info,omitempty"`
	BackendType         string              `json:"backend_type,omitempty"`
	// TabKinds is the daemon's own answer, per tab kind, to "may this session gain
	// one of these" — Capabilities.RefuseTabKind projected onto the snapshot.
	//
	// It exists so a CLIENT never re-derives that rule. The web UI used to compute
	// it in TypeScript from BackendType, which agreed with the daemon only by
	// coincidence and would disagree the moment a refusal lifted (#3060). Projecting
	// the verdict means an affordance and the call behind it cannot drift: whatever
	// RefuseTabKind decides is what the UI offers, and a kind that becomes available
	// off-box (#3062, #3054) needs no client change at all.
	//
	// Each entry carries the daemon's OWN refusal text rather than a client-invented
	// one, because a user told "not supported" cannot tell a kind that could work
	// from one that genuinely cannot.
	// PendingTabs are rows restored from this record whose workspace did not
	// survive and which have not been drained onto the roster yet. They are a
	// SEPARATE field, not folded into Tabs, because Tabs has an ordering contract:
	// index 0 is the agent. A recovery that fails before Launch leaves Tabs empty,
	// so emitting a staged web tab there would put it in the agent's slot — clients
	// would render it as the unclosable agent, and daemon mutations would report it
	// missing because the row lives only in the staging area (#3062).
	PendingTabs []TabData          `json:"pending_tabs,omitempty"`
	TabKinds    []TabKindAllowance `json:"tab_kinds,omitempty"`
	// snapshotTabsProjected marks an archived snapshot whose visible Tabs roster
	// was synthesized from PendingTabs. The private storage copy lets ForStorage
	// restore the ordered durable roster exactly, so a UI projection can never put
	// a staged web row back into the agent's index-0 persistence contract.
	snapshotTabsProjected bool
	snapshotStorageTabs   []TabData
	// TabRosterMutable is Capabilities.TabManagement projected: whether this
	// session's tab ROSTER may be mutated (rename, reorder). It is a different
	// question from either creating a kind or closing a tab, and it has a different
	// answer — tabMutationTarget still gates on TabManagement — so a client that
	// reuses a create-or-close verdict for it offers controls the daemon refuses.
	// A POINTER so that false survives the wire. With a plain bool, omitempty
	// erased exactly the verdict that matters: a backend allowing a metadata-only
	// kind while keeping TabManagement false — the forward-compatibility case this
	// projection exists for (#3062) — serialized to nothing, and a client cannot
	// tell "the daemon said no" from "the daemon is too old to say", so it falls
	// back to the create verdict and offers a rename tabMutationTarget rejects.
	// nil therefore means only "not projected", and ForStorage sets it back to nil
	// so a derived verdict still never reaches instances.json.
	TabRosterMutable *bool `json:"tab_roster_mutable,omitempty"`
	// RuntimeCleanupStateUnknown is the independent retention marker for a sandbox
	// teardown whose completion could not be determined. The next restore must
	// retry RuntimeCleanup before it can safely provision a replacement. It is not
	// a UserKilled tombstone: the session remains wanted after cleanup is settled.
	RuntimeCleanupStateUnknown bool `json:"runtime_cleanup_state_unknown,omitempty"`
	// RuntimeCleanup is written for a committed UserKilled tombstone or the
	// unknown-cleanup state above. It is the durable identity needed to resume
	// off-box teardown after a daemon restart; ordinary snapshots keep it nil and
	// stage the live handle in the private field below until ForStorage reaches a
	// retention boundary.
	RuntimeCleanup *RuntimeCleanupData `json:"runtime_cleanup,omitempty"`
	runtimeCleanup *RuntimeCleanupData
	// ArchiveWarning is the bounded live projection of an incomplete archive. It
	// may ride snapshots and lifecycle events; the full report is storage-only so
	// a large unreadable tree cannot turn every status response into megabytes.
	ArchiveWarning string `json:"archive_warning,omitempty"`
	// ArchiveReport makes a deliberately incomplete archive discoverable across
	// daemon restarts and at restore time. It lives beside the session record,
	// never inside the copied tree where a user path could collide with it. Live
	// projections stage only the source below until ForStorage requests a clone.
	ArchiveReport *git.ArchiveReport `json:"archive_report,omitempty"`
	// archiveReportSource stages the live worktree, not its unbounded report.
	// ForStorage asks it for one atomic path/recovery/report snapshot only when a
	// durable write is actually requested; ordinary JSON projections ignore it.
	archiveReportSource  *git.GitWorktree
	archiveReportPending bool
}

// AccountSwapData is the durable identity boundary for an automatic swap.
// From may be empty for the ambient identity; the pointer's presence, rather
// than either string, is the recovery obligation.
type AccountSwapData struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// IsRemoteHook reports whether this serialized record is a remote hook session,
// reading the persisted BackendType discriminator. It centralizes the raw-data
// remote check (#1592 Phase 1 PR3) so daemon logic that iterates []InstanceData
// — where no backend is reconstructed and Capabilities() is unavailable — never
// hard-codes the "remote" magic string. The load-time factory
// (NewInstanceFromData) remains the one place that maps the discriminator to a
// concrete backend.
func (d InstanceData) IsRemoteHook() bool {
	return d.BackendType == "remote"
}

// UsesLocalTmux reports whether this persisted row belongs to the in-process
// local backend and therefore claims a repo-scoped tmux name. Empty is the
// pre-backend-discriminator legacy encoding and also means local. Keeping this
// decoding beside BackendType prevents daemon admission from growing its own
// backend-name list.
func (d InstanceData) UsesLocalTmux() bool {
	return d.BackendType == "" || d.BackendType == "local"
}

// RestoreArchiveRollbackFence removes the previous-release safety projection
// from a persisted row. FromInstanceData uses it before reconstructing an
// Instance; storage-only cleanup paths use it before manually reconstructing a
// GitWorktree. Keeping that decoding here prevents a current daemon from
// mistaking its own old-reader fence for the session's real ownership.
func (d InstanceData) RestoreArchiveRollbackFence() InstanceData {
	if d.ArchiveReport == nil || d.ArchiveReport.RollbackFence == nil {
		return d
	}
	fence := d.ArchiveReport.RollbackFence
	d.StartupStateUnknown = fence.OriginalStartupStateUnknown
	d.Worktree.ExternalWorktree = fence.OriginalExternalWorktree
	d.Worktree.BranchCreatedByUs = cloneBoolPointer(fence.OriginalBranchCreatedByUs)
	if fence.RelocationRecoveryProjected {
		d.Worktree.RelocationRecovery = archiveRollbackRelocationRecovery(fence.OriginalRelocationRecovery)
	}
	return d
}

// ForClientRead converts a storage row into the same bounded shape emitted by a
// live daemon snapshot. Disk fallback callers must not expose the complete
// report or the compatibility-only ownership flags merely because the daemon is
// unavailable.
func (d InstanceData) ForClientRead() InstanceData {
	d = d.RestoreArchiveRollbackFence()
	if d.ArchiveReport != nil && !d.ArchiveReport.Empty() {
		d.ArchiveWarning = d.ArchiveReport.Warning(archiveWarningOperation(livenessFromData(d)))
	}
	d.ArchiveReport = nil
	d.archiveReportSource = nil
	d.archiveReportPending = false
	return d
}

func archiveWarningOperation(liveness Liveness) string {
	switch liveness {
	case LiveArchived:
		return "archive"
	case LiveLost:
		// A report can be installed before registration repair succeeds. Until
		// recovery settles, neither archive nor restore is known to have completed.
		return ""
	default:
		return "restore"
	}
}

// ForStorage returns data suitable for instances.json. InstanceData is also the
// daemon Snapshot payload, so it can carry transient in-flight operation state;
// disk persistence must not.
func (d InstanceData) ForStorage() InstanceData {
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
	d.ArchiveWarning = ""
	// The compatibility projection must capture original values before either it
	// or the relocation fence below overwrites them. Older binaries ignore
	// ArchiveReport, but the previous release understands the inert/ownership
	// fields and relocation recovery. Together they refuse restore and explicit
	// kill instead of publishing an incomplete tree or deleting the report's row.
	if d.ArchiveReport != nil && !d.ArchiveReport.Empty() {
		report := *d.ArchiveReport
		if !reportDetached {
			report = d.ArchiveReport.Clone()
		}
		if report.RollbackFence == nil {
			report.RollbackFence = archiveRollbackFence(d)
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
		// identity here would only preserve unused credentials until row deletion.
		d.RuntimeCleanupStateUnknown = false
		d.RuntimeCleanup = nil
	case !d.UserKilled && !d.RuntimeCleanupStateUnknown:
		// Cleanup credentials/identities have no reason to live in ordinary session
		// records. Kill intent and an unknown teardown outcome are the two durable
		// retention boundaries.
		d.RuntimeCleanup = nil
	case d.runtimeCleanup != nil:
		d.RuntimeCleanup = d.runtimeCleanup.clone()
	}
	// Never let the private staging pointer escape a storage projection. Loaded
	// tombstones have only RuntimeCleanup set and therefore preserve it above.
	d.runtimeCleanup = nil
	d.archiveReportSource = nil
	d.archiveReportPending = false
	return d
}

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

// TabData is the serializable form of a session.Tab. The full list is persisted
// (and restored by exact TmuxName) so every tab — agent and shell alike —
// reconnects to its tmux session across an af/daemon restart (#930). The field
// is omitempty + additive, mirroring the BranchCreatedByUs back-compat
// precedent: instances.json written before #930 PR 2 simply has no Tabs, and
// FromInstanceData synthesizes [agent, shell] from the legacy TmuxName/Program.
// TabKindAllowance is the projected verdict for one creatable tab kind.
type TabKindAllowance struct {
	// Kind is the `--kind` spelling, the same vocabulary the CLI validates against
	// (session.TabKindNameList), so a client switches on the name the user types.
	Kind string `json:"kind"`
	// Allowed is RefuseTabKind returning nil for this kind on this session.
	Allowed bool `json:"allowed"`
	// Reason is the daemon's refusal text, empty when allowed. Rendered verbatim by
	// clients; it names the requirement that is actually unmet (#3053).
	Reason string `json:"reason,omitempty"`
}

type TabData struct {
	// ID is the tab's stable identity (#1738), minted at creation and never
	// reused. It is the collision-proof key the PTY stream (?tab_id=) and the web
	// DnD/pane bindings address the tab by, so a reorder/close can't misroute.
	// omitempty + additive, mirroring the InstanceData.ID / BranchCreatedByUs
	// rollforward precedent: a record written before #1738 has no id, and
	// restoreLocalTabs backfills a fresh one on load.
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name"`
	Kind     TabKind `json:"kind"`
	Command  string  `json:"command,omitempty"`
	TmuxName string  `json:"tmux_name,omitempty"`
	// URL is the target of a TabKindWeb tab (the iframe/proxy address); empty for
	// every other kind. Surfaced in the snapshot so the web UI can iframe it and
	// so `af sessions get` shows the target.
	URL string `json:"url,omitempty"`
	// Conversation is the provider-specific conversation id for this tab, when
	// the underlying agent exposes a durable resume id. Omitted for legacy rows
	// and providers where af can only resume "latest".
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// Handoffs is the tab's append-only agent-swap ledger (#2013), oldest first.
	// omitempty + additive on the same rollforward precedent as ID and
	// Conversation: a record written before #2013 has none, which is
	// indistinguishable from a session that was never handed off — and those two
	// deserve the same treatment, so nothing has to be backfilled.
	Handoffs []AgentHandoff `json:"handoffs,omitempty"`
}

// TabCleanupData is one durable cleanup handle for a closed tab whose tmux
// teardown was never confirmed. It deliberately carries only what a retry needs
// — no Kind, Command, or URL — so it cannot be mistaken for, or restored as, a
// TabData: a tombstone that could round-trip into a tab would resurrect exactly
// the closed tab #2669 exists to keep buried.
type TabCleanupData struct {
	// TabID is the closed tab's stable id (#1738). Ids are never reused, so it
	// names the exact close this handle belongs to and keeps the retry's logs and
	// deduplication honest across restarts.
	TabID string `json:"tab_id,omitempty"`
	// TmuxName is the cleanup handle proper: the EXACT tmux session name the retry
	// must kill, and the token a later spawn must not re-derive. An entry with no
	// name would be untargetable, so CloseTab never records one.
	TmuxName string `json:"tmux_name"`
}

// PRInfoData represents the serializable data of a PRInfo
type PRInfoData struct {
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
	// Branch binds cached state to the exact ref used for the lookup. Legacy
	// records omit it and are therefore never trusted for destructive decisions.
	Branch string `json:"branch,omitempty"`
}

// GitWorktreeData represents the serializable data of a GitWorktree.
//
// BranchCreatedByUs indicates whether the session created the underlying
// branch itself (vs. reused a pre-existing one). It is serialized via a
// pointer so that "missing" (nil, for data written before this field was
// added) can be distinguished from an explicit false. Missing values are
// treated as true to preserve the prior behavior for sessions that existed
// before this flag was introduced.
type GitWorktreeData struct {
	RepoPath          string `json:"repo_path"`
	WorktreePath      string `json:"worktree_path"`
	SessionName       string `json:"session_name"`
	BranchName        string `json:"branch_name"`
	BaseCommitSHA     string `json:"base_commit_sha"`
	ExternalWorktree  bool   `json:"external_worktree,omitempty"`
	BranchCreatedByUs *bool  `json:"branch_created_by_us,omitempty"`
	// RelocationRecovery qualifies WorktreePath whenever a bounded lifecycle step
	// did not establish a safe outcome. Some states retain a second pathname;
	// every state blocks consumers until its owning retry resolves it.
	RelocationRecovery *GitWorktreeRelocationRecoveryData `json:"relocation_recovery,omitempty"`
}

type GitWorktreeRelocationRecoveryData struct {
	State git.RelocationRecoveryState `json:"state,omitempty"`
	// CleanupLifecycle carries cleanup-only states additively while State is
	// projected to claim_stale for previous releases which reject new enum values.
	CleanupLifecycle                   git.RelocationRecoveryState `json:"cleanup_lifecycle,omitempty"`
	AlternatePath                      string                      `json:"alternate_path"`
	IdentityKnown                      bool                        `json:"identity_known,omitempty"`
	Device                             uint64                      `json:"device"`
	Inode                              uint64                      `json:"inode"`
	FileType                           uint32                      `json:"file_type"`
	CleanupGeneration                  string                      `json:"cleanup_generation,omitempty"`
	CleanupOriginalExternalWorktree    *bool                       `json:"cleanup_original_external_worktree,omitempty"`
	CleanupOriginalBranchCreatedByUs   *bool                       `json:"cleanup_original_branch_created_by_us,omitempty"`
	CleanupOriginalStartupStateUnknown *bool                       `json:"cleanup_original_startup_state_unknown,omitempty"`
	OriginalExternalWorktree           *bool                       `json:"original_external_worktree,omitempty"`
	OriginalBranchCreatedByUs          *bool                       `json:"original_branch_created_by_us,omitempty"`
	OriginalStartupStateUnknown        *bool                       `json:"original_startup_state_unknown,omitempty"`
}

func projectRelocationRecoveryForPreviousRelease(recovery *GitWorktreeRelocationRecoveryData) {
	if recovery == nil {
		return
	}
	switch recovery.State {
	case git.RelocationRecoveryCleanupReady, git.RelocationRecoveryCleanupFinalizing:
		// The immediately preceding reader understands recovery but not this
		// additive lifecycle. Preserve the actual values for current readers, and
		// give the old reader ownership values which remain safe even after its
		// repo-gone restore consumes claim_stale and clears the record.
		recovery.CleanupOriginalExternalWorktree = cloneBoolPointer(recovery.OriginalExternalWorktree)
		recovery.CleanupOriginalBranchCreatedByUs = cloneBoolPointer(recovery.OriginalBranchCreatedByUs)
		recovery.CleanupOriginalStartupStateUnknown = cloneBoolPointer(recovery.OriginalStartupStateUnknown)
		safeExternal := true
		safeBranchOwned := false
		safeStartupUnknown := true
		recovery.OriginalExternalWorktree = &safeExternal
		recovery.OriginalBranchCreatedByUs = &safeBranchOwned
		recovery.OriginalStartupStateUnknown = &safeStartupUnknown
		recovery.CleanupLifecycle = recovery.State
		recovery.State = git.RelocationRecoveryClaimStale
	}
}

func runtimeRelocationRecoveryState(
	recovery *GitWorktreeRelocationRecoveryData,
) (git.RelocationRecoveryState, error) {
	if recovery.CleanupLifecycle == "" {
		return recovery.State, nil
	}
	if recovery.State != git.RelocationRecoveryClaimStale {
		return "", fmt.Errorf(
			"cleanup lifecycle %q requires a claim_stale compatibility state, got %q",
			recovery.CleanupLifecycle, recovery.State,
		)
	}
	if recovery.CleanupLifecycle != git.RelocationRecoveryCleanupReady &&
		recovery.CleanupLifecycle != git.RelocationRecoveryCleanupFinalizing {
		return "", fmt.Errorf("unknown cleanup relocation lifecycle %q", recovery.CleanupLifecycle)
	}
	return recovery.CleanupLifecycle, nil
}

// Storage handles saving and loading instances using the state interface.
// When repoID is set (TUI mode), operations are scoped to that repo.
// When repoID is empty (daemon mode), operations span all repos.
type Storage struct {
	state  config.InstanceStorage
	repoID string
}

// NewStorage creates a new storage instance.
// Pass a non-empty repoID for TUI (repo-scoped) mode, or "" for daemon (all-repo) mode.
func NewStorage(state config.InstanceStorage, repoID string) (*Storage, error) {
	return &Storage{
		state:  state,
		repoID: repoID,
	}, nil
}

// dedupeInstanceData collapses records that share a title, keeping the one
// with the newest UpdatedAt (ties keep the earliest occurrence, so in-memory
// records — which both save paths place ahead of disk-only records — win).
// Titles are unique per repo (the daemon's findTitleConflictLocked enforces
// this on create), so two same-title records in one repo's list are always
// the same logical session written twice (#808). Deduping at the save/load
// chokepoints prevents new duplicates from persisting and collapses any
// existing on-disk duplicate on the next clean save.
func dedupeInstanceData(data []InstanceData) []InstanceData {
	if len(data) < 2 {
		return data
	}
	index := make(map[string]int, len(data))
	out := make([]InstanceData, 0, len(data))
	for _, d := range data {
		if i, ok := index[d.Title]; ok {
			if d.UpdatedAt.After(out[i].UpdatedAt) {
				out[i] = d
			}
			continue
		}
		index[d.Title] = len(out)
		out = append(out, d)
	}
	return out
}

// SaveInstances persists the daemon's authoritative in-memory instances to
// disk, grouped by repo. As of #960 PR 4 the daemon is the SOLE writer of
// instances.json, so this is a straight marshal of the manager's per-repo
// state, NOT a merge: there is no competing full-list writer to reconcile
// against, so the old mergeInstancesWithDisk rule-zoo
// (#551/#766/#808/#819/#844/#959) is gone. With one writer a clobber is
// impossible by construction.
//
// Only repos with at least one persistable in-memory instance are rewritten;
// repos the daemon holds nothing for are left untouched — their records were
// already removed by the targeted DeleteInstance on kill, or were never loaded.
// Generic Loading/Deleting/non-started instances are skipped: their worktree is
// not yet populated (Loading) or is mid-teardown (Deleting), so FromInstanceData
// cannot restore them. Explicit durable retention markers override that legacy
// projection; in particular, a pending handoff names a live replacement and a
// staged archive report is the only durable handle to retained source trees.
//
// The targeted writers (appendInstanceData / persistInstanceData /
// DeleteInstance) keep the disk current on every mutation; this full save is the
// shutdown checkpoint. Records are deduped by title (#808) before marshaling.
// Because the manager's memory is the source of truth, the save deliberately
// does NOT read disk first: the file is overwritten with authoritative state, so
// a corrupt or momentarily-stale file on disk is simply replaced, not merged.
func (s *Storage) SaveInstances(instances []*Instance) error {
	// Group persistable in-memory instances by repo root. Prefer the worktree's
	// resolved repo path so we share a repo ID with the TUI even for a session
	// created from a symlinked path; fall back to Path for remote backends where
	// Worktree.RepoPath is empty. This mirrors the repo-root collection in
	// commands/reset.go's planFactoryReset (#667).
	grouped := make(map[string][]InstanceData)
	for _, inst := range instances {
		data := inst.ToInstanceData()
		status := data.Status
		pendingHandoff := data.PendingHandoffMission != ""
		unknownRuntimeCleanup := data.RuntimeCleanupStateUnknown
		unresolvedRelocation := data.Worktree.RelocationRecovery != nil
		archiveReportPending := data.archiveReportPending
		pendingTabs := len(data.PendingTabs) > 0
		durableRetention := pendingHandoff || unknownRuntimeCleanup ||
			unresolvedRelocation || archiveReportPending
		// A pending mission is a durable recovery obligation and therefore a
		// retention claim, not generic transient UI state. OpReplacing composes to
		// Loading, but dropping that row would erase the only handle to a live
		// incoming runtime. Explicit durable state outranks the lossy legacy status.
		// PendingTabs does NOT override Deleting: an explicit delete still wins over
		// preserving its UI metadata, so a crash cannot resurrect the session.
		if (status == Loading || status == Deleting) && !durableRetention {
			continue
		}
		// The !Started() skip drops transient never-started junk (a create that
		// hasn't run Start, a discarded duplicate). It must NOT drop an Archived
		// instance (#1028): archived sessions load deliberately inert
		// (started=false — tmux torn down, worktree relocated), yet the record is
		// the ONLY pointer to the relocated worktree. Dropping it on a wholesale
		// per-repo checkpoint save — triggered whenever ANY started instance in
		// the same repo is saved — would silently orphan the archived worktree.
		// (Lost is unaffected: it loads started=true, so it already survives.)
		//
		// TOMBSTONED, startup-unknown, runtime-cleanup-unknown, and unresolved
		// worktree-relocation instances are also kept (#1917/#2207/#3135). They are
		// started=false and not Archived while
		// their workspace may still be live: teardown could not confirm the pane
		// dead or finish a worktree removal, startup never established the runtime's
		// identity, or an off-box teardown did not establish whether its sandbox was
		// reaped. The record is deliberately RETAINED as that workspace's only
		// handle. Without this clause the next checkpoint triggered by any other
		// started session in the repo would silently drop it, undoing the retention
		// in a layer that never heard of it, and orphaning the very workspace the
		// retention exists to protect. Retention is a claim on this writer too.
		if !inst.Started() && status != Archived && !data.UserKilled &&
			!data.StartupStateUnknown && !durableRetention && !pendingTabs {
			continue
		}
		root := inst.GetRepoPath()
		if root == "" {
			root = inst.Path
		}
		rid := config.RepoIDFromRoot(root)
		grouped[rid] = append(grouped[rid], data.ForStorage())
	}

	for rid, group := range grouped {
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			jsonData, err := json.Marshal(dedupeInstanceData(group))
			if err != nil {
				return fmt.Errorf("failed to marshal instances for repo %s: %w", rid, err)
			}
			return s.state.SaveInstances(rid, jsonData)
		}); err != nil {
			return err
		}
	}

	return nil
}

// LoadInstances loads the list of instances from disk.
func (s *Storage) LoadInstances() ([]*Instance, error) {
	var allJSON map[string]json.RawMessage
	if s.repoID != "" {
		// TUI mode: load just this repo. Surface read errors so startup can
		// report "couldn't read your sessions" instead of silently showing
		// an empty list that looks like a fresh install (#766).
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return nil, err
		}
		allJSON = map[string]json.RawMessage{s.repoID: raw}
	} else {
		// Daemon mode: load all repos. Surface a directory-level read error so
		// the daemon reports "couldn't read your sessions" instead of silently
		// presenting an empty list that looks like a fresh install while live
		// sessions sit unreadable on disk (#868).
		all, err := s.state.GetAllInstances()
		if err != nil {
			return nil, err
		}
		allJSON = all
	}

	var instances []*Instance
	for _, jsonData := range allJSON {
		if jsonData == nil || string(jsonData) == "[]" || string(jsonData) == "null" {
			continue
		}
		var instancesData []InstanceData
		if err := json.Unmarshal(jsonData, &instancesData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
		}
		// Collapse duplicate records written before the dedup-on-save fix
		// (#808) so a dup-containing file yields one sidebar row per session
		// immediately, not just after the next save rewrites the file.
		instancesData = dedupeInstanceData(instancesData)
		for _, data := range instancesData {
			data = data.ForStorage()
			instance, err := FromInstanceData(data)
			if err != nil {
				// Instance's tmux session or worktree may have been
				// destroyed externally. Log and skip rather than
				// failing the entire load.
				log.WarningLog.Printf("skipping instance %q: %v", data.Title, err)
				continue
			}
			instances = append(instances, instance)
		}
	}

	return instances, nil
}

// DeleteInstance removes an instance from storage by filtering raw JSON
// directly, avoiding the need to reconstruct live Instance objects (which
// may fail if tmux/worktree has already been destroyed).
func (s *Storage) DeleteInstance(title string) error {
	deleted, err := s.DeleteInstanceByStableID(title, "")
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("instance not found: %s", title)
	}
	return nil
}

// InstanceDeleteLockTimeout bounds how long DeleteInstanceByStableID waits for
// the per-repo instances flock. A var so tests can shorten it; production never
// reassigns.
//
// The delete is the LAST step of a session kill, and the daemon runs it holding
// that session's kill guard, so an unbounded wait here does not just stall one
// write — it strands a session whose kill-intent tombstone is already on disk,
// leaving it undeletable for the daemon's whole lifetime (#1917). The budget is
// generous: this lock is held only across a read-modify-write of one small JSON
// file, so exceeding it means a peer is genuinely wedged, not merely slow.
var InstanceDeleteLockTimeout = 10 * time.Second

// DeleteInstanceByStableID removes an instance from storage only when the
// record still matches the stable session identity captured by the caller. A
// false nil result means a same-titled record exists but belongs to a different
// instance, so the caller must treat the delete as stale and leave it alone.
// Empty IDs are legacy-compatible and fall back to title matching.
//
// It takes the instances flock with a DEADLINE (config.WithFileLockTimeout), not
// the blocking WithFileLock every other Storage writer uses: a contended lock
// surfaces as a retryable config.ErrLockTimeout instead of parking the caller
// forever. See InstanceDeleteLockTimeout for why this writer in particular
// cannot afford an unbounded wait.
func (s *Storage) DeleteInstanceByStableID(title, id string) (bool, error) {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return false, pathErr
	}
	deleted := false
	sameTitleDifferentID := false
	if err := config.WithFileLockTimeout(path, InstanceDeleteLockTimeout, func() error {
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return err
		}
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			return fmt.Errorf("instance not found: %s", title)
		}

		var data []InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("failed to parse instances: %w", err)
		}

		filtered := make([]InstanceData, 0, len(data))
		found := false
		for _, d := range data {
			if d.Title == title {
				if stableIDMatches(d.ID, id) {
					found = true
					deleted = true
					continue
				}
				sameTitleDifferentID = true
			}
			filtered = append(filtered, d)
		}

		if !found {
			if sameTitleDifferentID {
				return nil
			}
			return fmt.Errorf("instance not found: %s", title)
		}

		out, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, out)
	}); err != nil {
		return false, err
	}
	return deleted, nil
}

func stableIDMatches(recordID, expectedID string) bool {
	return expectedID == "" || recordID == "" || recordID == expectedID
}

// LoadInstanceData reads and unmarshals instance data from disk without
// constructing live Instance objects (no tmux session restoration).
// Used for lightweight comparison against in-memory state.
func (s *Storage) LoadInstanceData() ([]InstanceData, error) {
	raw, err := s.state.GetInstances(s.repoID)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "[]" || string(raw) == "null" {
		return nil, nil
	}
	var data []InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
	}
	return dedupeInstanceData(data), nil
}

// DeleteAllInstances removes all stored instances
func (s *Storage) DeleteAllInstances() error {
	return s.state.DeleteAllInstances()
}
