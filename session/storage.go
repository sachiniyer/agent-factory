package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
)

// PendingOnCompleteData is the durable record of a declared on_complete
// teardown owed to a finished task run (#4162). It carries only WHAT is owed
// and when the obligation was taken: the verb is deliberately absent — it is
// re-resolved at drain time, so a deleted task still yields keep and an edited
// policy applies to whichever decision the drain reaches.
//
// FiledAt doubles as the adoption watermark: pane churn recorded after it is
// evidence that someone touched the session after the decision was owed, which
// the delivery counter cannot see when the input arrived through an attached
// tmux rather than an agent-server entry point.
type PendingOnCompleteData struct {
	TaskID  string    `json:"task_id"`
	FiledAt time.Time `json:"filed_at"`
}

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
	// The wire names its integer as `status_name` (session/state_names.go).
	Status Status `json:"status"`
	// Liveness is the daemon-owned health axis (#1195), the new canonical
	// persisted state. omitempty + additive: records written before #1195 have
	// no `liveness` key and decode to LivenessUnset, signaling FromInstanceData
	// to fall back to the legacy `status` int (rollforward). The wire names it as
	// `liveness_name`, applying that same fallback (session/state_names.go).
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
	// LostRestoreWait persists that automatic recovery is waiting on the
	// environment — the last failure was a resource limit external to the
	// session — across daemon restarts until a restore attempt succeeds, a
	// structural failure retires the wait, or a replacement lands (#5295). A
	// row that reloads INERT drops it on load instead: no automatic retry can
	// reach that row, so the record would report an armed retry forever.
	LostRestoreWait *LostRestoreWait `json:"lost_restore_wait,omitempty"`
	// Unreachable projects a live remote-probe outage (#5571 W1.3): the
	// daemon's observation stopped being answered at Since, and Error is the
	// last transport failure — collapsed, bounded, and token-redacted before it
	// was stored on the record, so it is safe to ride every client surface. It
	// is an observation flag, not a liveness: it coexists with Running/Ready/
	// Lost and suppresses the destructive lifecycle verbs for the flag's
	// duration. Projection-only like IdleReason above: ForStorage scrubs it so
	// a restarted daemon re-derives the claim from its own probes rather than
	// inheriting an outage it never observed.
	Unreachable *UnreachableData `json:"unreachable,omitempty"`

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
	// TaskRunIdleEdgeHeld records that the run's idle edge was held open because
	// a handoff mission was still owed (#4429), so the next idle observation
	// after the mission is retired ends the run.
	// Persisted for the same reason as TaskRunActive: the held edge is already
	// spent and nothing re-derives it. omitempty + additive: an older record
	// decodes to false, which holds nothing.
	//
	// This key keeps ONLY its original meaning (#5221 review): a daemon rolled
	// back to a release that knows just the mission hold reads it exactly as it
	// wrote it. The turn-gate hold lives in TaskRunTurnGateHeld so the older
	// binary drops — rather than releases — a marker it cannot evaluate.
	TaskRunIdleEdgeHeld bool `json:"task_run_idle_edge_held,omitempty"`
	// TaskRunTurnGateHeld records that the run's idle edge was held open because
	// the prompt had been attempted without turn evidence (#5219). Stored under
	// its own key for rollback safety: a previous release would misread a
	// turn-gate hold written into task_run_idle_edge_held as a resolved mission
	// hold and end the run on the next Ready → Ready tick. An old binary drops
	// this field and keeps the run open instead — the safe direction.
	TaskRunTurnGateHeld bool `json:"task_run_turn_gate_held,omitempty"`
	// TaskRunPromptAttemptAt is the run's OWN prompt boundary (#5221 review),
	// scoped apart from LastPromptAttemptAt so an interactive send cannot
	// re-arm a delivery window the agent already satisfied. The boundary moves
	// only while the window is unsatisfied. Persisted while the run is in
	// flight for the same reason as TaskRunTurnObservedAt: a restart must not
	// reopen — or wrongly satisfy — a window the daemon was still holding.
	TaskRunPromptAttemptAt time.Time `json:"task_run_prompt_attempt_at,omitzero"`
	// TaskRunTurnObservedAt is when the agent's own in-turn chrome was observed
	// for the run in flight (#5219) — the one pane signal a still-booting agent
	// cannot produce: its elapsed-timer status row had to tick, not just render.
	// Pane churn stays a weaker fact and cannot release the held edge alone.
	// Persisted for the same reason as TaskRunIdleEdgeHeld: a restart must not
	// reopen the delivery window an observation already closed. omitempty +
	// additive: an older record decodes to zero, which releases nothing.
	TaskRunTurnObservedAt time.Time `json:"task_run_turn_observed_at,omitzero"`
	// PendingOnComplete records an on_complete teardown owed to this session's
	// finished task run (#4162). The daemon files it BEFORE waiting on
	// post-worktree hooks, so a shutdown that drops the in-flight lifecycle
	// goroutine cannot lose the obligation the way that goroutine's knowledge
	// could: the completion edge spends task_run_active permanently, so nothing
	// re-derives it, and a row without the marker simply owes nothing.
	//
	// The marker is discharged — cleared and persisted — only by a decision:
	// the teardown committed, the session was adopted or replaced, the resolved
	// verb was keep, or the daemon deliberately left the session in place. A
	// teardown that merely FAILED keeps it; the next daemon generation retries.
	// omitempty + additive + rollforward, mirroring the TaskRunActive precedent.
	PendingOnComplete *PendingOnCompleteData `json:"pending_on_complete,omitempty"`
	// LimitResetAt is the parsed usage-limit reset time (#1146), display-only:
	// written (and carried in the daemon snapshot to the read-only TUI) only for a
	// LiveLimitReached row so the sidebar [limit] badge can show "resets <t>" and
	// survive a restart, and so PR3's auto-resume scheduler can read it. omitzero
	// drops it for every normal session (and for a limit row whose banner carried
	// no parseable reset time): omitempty never omits a struct, and a zero
	// time.Time would otherwise serialize as "0001-01-01T00:00:00Z" on every row,
	// inventing a reset date the web would render (#4506 review, mirroring the
	// TabExitData.At precedent); additive + rollforward, mirroring the Liveness
	// precedent.
	LimitResetAt time.Time `json:"limit_reset_at,omitzero"`
	// LimitAgent is the provider namespace for LimitAccount. It is always a
	// tmux.SupportedPrograms enum or empty; account labels are agent-scoped, so a
	// pending cross-agent replacement must retain both values.
	LimitAgent string `json:"limit_agent,omitempty"`
	// LimitAccount is the identity that produced this limit observation. It may
	// differ from Account while a durably selected replacement is still starting.
	LimitAccount string `json:"limit_account,omitempty"`
	// AccountLimitObservations retain quota walls after a successful identity
	// replacement clears the session's current limit liveness. Candidate
	// selection uses these per agent/account observations until a known reset
	// window expires; an unknown reset stays conservatively active.
	AccountLimitObservations []AccountLimitObservationData `json:"account_limit_observations,omitempty"`
	Height                   int                           `json:"height"`
	Width                    int                           `json:"width"`
	CreatedAt                time.Time                     `json:"created_at"`
	// UpdatedAt is the last session state mutation, preserved across saves and loads.
	UpdatedAt time.Time `json:"updated_at"`
	// ArchivedAt is when this session's archive committed — the durable anchor
	// `af sessions prune --older-than` measures from (#5136). It is a separate
	// field rather than UpdatedAt reused, because a record can keep mutating
	// after it is shelved (prune itself is one such mutation). omitempty +
	// additive: rows archived before this field existed carry no value and the
	// prune cutoff falls back to UpdatedAt, which only ever moves FORWARD —
	// never to a time before the real archive — so the fallback can only make a
	// legacy row look younger and therefore under-delete, never over-delete.
	ArchivedAt time.Time `json:"archived_at,omitzero"`
	Prompt     string    `json:"prompt,omitempty"`
	// PendingHandoffMission is a rendered takeover brief whose incoming runtime
	// has been established but whose delivery has not been durably confirmed.
	// Unlike Prompt, it is cleared only after the exact mission lands (or is
	// transferred to the usage-limit retry path). Automatic recovery also requires
	// the mission-scoped status below to prove that redelivery is safe.
	PendingHandoffMission string `json:"pending_handoff_mission,omitempty"`
	// HandoffDeliveryStatus belongs only to PendingHandoffMission. Missing
	// or ambiguous evidence fails closed; only not-delivered permits an automatic
	// retry because it proves this exact mission did not land.
	HandoffDeliveryStatus PromptDeliveryStatus `json:"pending_handoff_delivery_status,omitempty"`
	// HandoffOriginalStartupStateUnknown is a storage-only rollback fence. A
	// current reader removes it before interpreting the row; the previous release
	// ignores it and sees StartupStateUnknown, which prevents replaying an
	// ambiguous pending mission after a binary rollback.
	HandoffOriginalStartupStateUnknown *bool `json:"pending_handoff_original_startup_state_unknown,omitempty"`
	// PendingAccountSwap is the committed identity change whose replacement
	// runtime still needs the in-session notice and stored task delivered.
	PendingAccountSwap *AccountSwapData `json:"pending_account_swap,omitempty"`

	Program string `json:"program"`
	// RuntimeProgram is the override-resolved base command of the last agent
	// runtime whose launch positively succeeded. Empty on records written before
	// this evidence existed; callers must treat absence as unknown, never infer it
	// from Program's requested label.
	RuntimeProgram string `json:"runtime_program,omitempty"`
	// RuntimePID and RuntimeStartID are the pane-root process identity captured
	// at the same boundary that recorded RuntimeProgram (#5066): the pair
	// proctree.SameIdentity treats as one process instance (PID plus the
	// kernel's start stamp, which defeats pid reuse). On reattach a matching
	// pair proves the pane is still the launch af made, so the claim survives a
	// daemon restart; a mismatch or an unanswerable probe retires it. Zero on
	// records predating the evidence.
	RuntimePID     int    `json:"runtime_pid,omitempty"`
	RuntimeStartID uint64 `json:"runtime_start_id,omitempty"`
	// Account is the credential account this session's provider panes use (#3051).
	// Persisted because the identity a session runs as must survive a daemon
	// restart and an archive/restore: a session that silently reverted to the
	// ambient account would spend the wrong quota while still displaying the
	// account it was created with.
	Account string `json:"account,omitempty"`
	// AccountAutoSelected is true only when af's opt-in limit scheduler chose the
	// account. Missing/false preserves every pre-#3127 account as an explicit pin.
	AccountAutoSelected bool `json:"account_auto_selected,omitempty"`
	// AccountAgent is the agent namespace Account was selected in (#4430). It is
	// durable because program_overrides can later resolve Program's enum to a
	// different agent's command: re-deriving the namespace from that new command
	// would look the same label up in another agent's registry. Empty on records
	// older than the field — their selections could only have used the Program
	// enum's namespace, which is the reader-side fallback.
	AccountAgent string `json:"account_agent,omitempty"`
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
	// pr_info was removed; encoding/json ignores the extra key in old records.
	BackendType string `json:"backend_type,omitempty"`
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
	// OriginalRuntimeCleanupStateUnknown is a storage-only rollback fence for the
	// ordinary-sandbox RuntimeCleanup projection ForStorage writes so a binary
	// rolled back to the immediately preceding release does not load the row inert
	// and orphan its off-box runtime. That release's FromInstanceData only
	// restores a cleanup handle for a UserKilled tombstone or an unknown-cleanup
	// row, and its ForStorage clears RuntimeCleanup on the ordinary branch — so a
	// handle this release persists on an ordinary sandbox row would be erased on
	// the next checkpoint or staged as a nothing-owed kill. ForStorage therefore
	// publishes the handle on a row the previous release already treats as a
	// retention row (RuntimeCleanupStateUnknown=true), and carries the real false
	// here so a current reader restores it before interpreting the row. A previous
	// release ignores this additive field and keeps the projected value, which is
	// the safe direction: its FromInstanceData rebuilds the teardown and its
	// ForStorage default branch preserves the handle on reload. A current reader
	// removes it via RestoreRuntimeCleanupRollbackFence before any decision; the
	// private staging field below is never serialized.
	OriginalRuntimeCleanupStateUnknown *bool `json:"original_runtime_cleanup_state_unknown,omitempty"`
	runtimeCleanup                     *RuntimeCleanupData
	// KilledWithoutLiveRuntime is the inert-at-kill admission marker for a
	// sandbox kill tombstone (docker/ssh/hook/remote) whose commit found no live
	// off-box runtime to reap: the kill ran against an inert rebuild (nil
	// runtimeTeardown), so the tombstone carries no RuntimeCleanup handle on
	// purpose — "nothing on-box to reap", the sandbox analog of the local
	// branch's worktreeReaped admission (#2691).
	//
	// A current-binary inert-at-kill tombstone and a legacy pre-#2265 tombstone
	// are indistinguishable on disk without this marker: both persist a nil
	// RuntimeCleanup, and the load path could not tell "the kill owed nothing"
	// from "the binary could not store the handle it owed". The distinction is
	// therefore WRITTEN at commit time — toInstanceDataLocked sets the private
	// marker below when a sandbox backend owes no teardown — and READ at load
	// time by FromInstanceData, which admits the marker the way worktreeReaped
	// does and lets the pending record delete proceed instead of installing a
	// permanent fail-closed teardown no retry, re-kill, or restart can clear.
	//
	// Additive + omitempty + rollforward on the same precedent as UserKilled: a
	// record written before this field simply has no marker and fails closed,
	// which is the safe legacy direction (a wedge an older binary already
	// cannot unwedge). Published only for UserKilled tombstones by ForStorage,
	// so it never reaches an ordinary snapshot or disk row.
	KilledWithoutLiveRuntime bool `json:"killed_without_live_runtime,omitempty"`
	killedWithoutLiveRuntime *bool
	// archivePushCompleted is process-local evidence that the push belonging to
	// the CURRENT OpArchiving generation returned successfully. Branch cannot be
	// that evidence: restore deliberately preserves the previous archive branch.
	archivePushCompleted bool
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

// RestoreRuntimeCleanupRollbackFence removes the ordinary-sandbox projection
// ForStorage writes so a current reader interprets the row as the ordinary
// session it is, not the retention row a previous release needs to see. The
// previous release's FromInstanceData only rebuilds a teardown for a UserKilled
// tombstone or an unknown-cleanup row, so ForStorage publishes an ordinary
// sandbox row's RuntimeCleanup under a projected RuntimeCleanupStateUnknown=true
// and saves the real false here. A current reader restores the real value before
// deciding whether the row is unknown-cleanup or ordinary; a rolled-back reader
// ignores this additive field and treats the projected row as a retention row,
// rebuilding and preserving the handle instead of orphaning the runtime.
func (d InstanceData) RestoreRuntimeCleanupRollbackFence() InstanceData {
	if d.OriginalRuntimeCleanupStateUnknown == nil {
		return d
	}
	d.RuntimeCleanupStateUnknown = *d.OriginalRuntimeCleanupStateUnknown
	d.OriginalRuntimeCleanupStateUnknown = nil
	return d
}

// ForClientRead converts a storage row into the same bounded shape emitted by a
// live daemon snapshot. Disk fallback callers must not expose the complete
// report or the compatibility-only ownership flags merely because the daemon is
// unavailable.
//
// It replays FromInstanceData's full fence-restoration order — archive, then
// relocation recovery, then handoff and account-swap — so the rollback-safe
// ownership projection ForStorage writes for any row carrying an unresolved
// worktree relocation recovery cannot survive into the disk-fallback client
// view. A recovery row whose safety metadata is missing or undecodable is
// corrupt; ForClientRead cannot drop the row, so it leaves the ForStorage
// projection in place and the row reads as the conservative unknown-ownership
// verdict rather than fabricating confidence — the same fail-closed direction
// every other reader takes (the loader refuses such a row outright).
func (d InstanceData) ForClientRead() InstanceData {
	d = d.RestoreArchiveRollbackFence()
	// Mirror FromInstanceData's fence order so a disk-fallback client does not
	// see the projected RuntimeCleanupStateUnknown a previous release needs.
	d = d.RestoreRuntimeCleanupRollbackFence()
	restored, err := d.RestoreRelocationRecoveryOriginals()
	if err == nil {
		d = restored
	}
	d = d.RestoreHandoffRollbackFence()
	d = d.RestoreAccountSwapRollbackFence()
	d = d.restoreMissingHandoffMissionEvidence()
	d = d.restoreMissingAccountSwapMissionEvidence()
	// ForStorage persists RuntimeCleanup on ordinary sandbox rows so a daemon
	// restart can rebuild the off-box teardown handle, but that handle is
	// operator-private (a container id, an ssh host/identity-file path, a hook
	// delete command). The live-daemon snapshot never publishes it — ToInstanceData
	// stages it only in the private field ForStorage consumes — so a disk-fallback
	// client reading instances.json directly must not either: scrub it here so the
	// daemonless `sessions list`/`get` path exposes the same shape the live path
	// does, not the cleanup identity an ordinary row now carries on disk.
	d.RuntimeCleanup = nil
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
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// The wire names it as `kind_name`, the word `tab_kinds[].kind` uses too.
	Kind     TabKind `json:"kind"`
	Command  string  `json:"command,omitempty"`
	TmuxName string  `json:"tmux_name,omitempty"`
	// URL is the target of a TabKindWeb tab (the iframe/proxy address); empty for
	// every other kind. Surfaced in the snapshot so the web UI can iframe it and
	// so `af sessions get` shows the target.
	URL string `json:"url,omitempty"`
	// Proposal is the slug a docs tab edits; empty for every other kind.
	Proposal string `json:"proposal,omitempty"`
	// PushMode is a docs tab's push mode, "auto" or "manual"; empty means auto.
	PushMode string `json:"push_mode,omitempty"`
	// WebProxied is derived at JSON encoding with IsLoopbackWebTarget, the
	// daemon proxy's predicate. A pointer preserves explicit false for web tabs;
	// non-web tabs omit it. Stored/input values never override the derivation.
	WebProxied *bool `json:"web_proxied,omitempty"`
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
	// Exit records how a process tab's command ended (#4479): the pane was seen
	// dead, not merely missing, or af stopped it and says why. nil for tabs af
	// never saw finish — the load path maps that to "still in flight", never to
	// "re-run me".
	Exit *TabExitData `json:"exit,omitempty"`
	// AccountScope is the account af launched this tab's pane under (#4506
	// review). A sibling whose recorded scope is not the session's account is
	// stopped once at load, because its pane may run on another identity. Empty
	// for a pane launched on the ambient identity, and for rows written before
	// this field existed.
	AccountScope string `json:"account_scope,omitempty"`
}

// TabExitData is the wire form of Tab.Exit: only the fields a reader needs to
// render or reason about a finished command.
type TabExitData struct {
	Status      int  `json:"status,omitempty"`
	StatusKnown bool `json:"status_known,omitempty"`
	// At is omitted, not zero-valued, when tmux reported no death time:
	// omitempty never omits a struct, and "0001-01-01T00:00:00Z" would present
	// an invented completion time to every reader (#4506 review).
	At time.Time `json:"at,omitzero"`
	// StoppedBy is set when af stopped the command rather than it exiting:
	// "account-scope" or "account-swap" (#4506 review).
	StoppedBy string `json:"stopped_by,omitempty"`
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
// with the latest session mutation time, UpdatedAt (legacy records carry their
// last save time). Ties keep the earliest occurrence, so in-memory
// records — which both save paths place ahead of disk-only records — win.
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
// instances.json. It normally writes the manager's per-repo state directly;
// the narrow archive reconciliation below is the exception because targeted
// archive persistence and this shutdown checkpoint are two paths in that same
// writer. The old mergeInstancesWithDisk rule-zoo
// (#551/#766/#808/#819/#844/#959) remains gone.
//
// Only repos with at least one persistable in-memory instance are rewritten;
// repos the daemon holds nothing for are left untouched — their records were
// already removed by the targeted DeleteInstanceByStableID on kill, or were never loaded.
// Generic Loading/Deleting/non-started instances are skipped: their worktree is
// not yet populated (Loading) or is mid-teardown (Deleting), so FromInstanceData
// cannot restore them. Explicit durable retention markers override that legacy
// projection; in particular, a pending handoff names a live replacement and a
// staged archive report is the only durable handle to retained source trees.
//
// The targeted writers (appendInstanceData / persistInstanceData /
// DeleteInstanceByStableID) keep the disk current on every mutation; this full save is the
// shutdown checkpoint. Records are deduped by title (#808) before marshaling.
// Because the manager's memory is the source of truth, ordinary rows do not read
// disk first. Only a sandbox archive snapshot re-reads the exact repo under its
// file lock, and only a newer committed outcome for the same stable identity can
// displace memory.
func (s *Storage) SaveInstances(instances []*Instance) error {
	// Keep the live instances grouped by their stable storage repo. Each repo is
	// projected once to decide whether it has anything to checkpoint, then again
	// inside its file lock so an archive that starts after collection cannot be
	// written back as the stale Running snapshot (#3966405841).
	grouped := make(map[string][]*Instance)
	for _, inst := range instances {
		rid := inst.repoIDForStorage()
		grouped[rid] = append(grouped[rid], inst)
	}

	for rid, repoInstances := range grouped {
		initial := snapshotInstancesForCheckpoint(repoInstances)
		if !initial.shouldWrite() {
			continue
		}
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			checkpoint := snapshotInstancesForCheckpoint(repoInstances)
			if !checkpoint.shouldWrite() {
				return nil
			}
			group := checkpoint.group
			if checkpoint.needsArchiveReconcile() {
				raw, readErr := s.state.GetInstances(rid)
				if readErr != nil {
					return fmt.Errorf("reconcile: read disk for repo %s: %w", rid, readErr)
				}
				if len(raw) > 0 {
					var onDisk []InstanceData
					if jsonErr := json.Unmarshal(raw, &onDisk); jsonErr == nil {
						reconcilePendingArchiveRows(group, checkpoint.inFlightArchive, onDisk)
						group = mergeCommittedArchiveRows(group, onDisk, checkpoint.prePushArchive)
					}
				}
			}
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

// SaveInstancesForShutdown writes the daemon's terminal checkpoint. Sealing
// every instance first closes the post-snapshot admission race: an archive that
// has not begun is refused, while an archive already behind its BeginArchive
// fence is joined through its settling transition. The final snapshot therefore
// cannot precede an archive that the terminating process allowed to commit.
func (s *Storage) SaveInstancesForShutdown(instances []*Instance) error {
	settled := make([]<-chan struct{}, 0, len(instances))
	for _, inst := range instances {
		if archiveSettled := inst.sealArchiveCheckpoint(); archiveSettled != nil {
			settled = append(settled, archiveSettled)
		}
	}
	for _, archiveSettled := range settled {
		<-archiveSettled
	}
	return s.SaveInstances(instances)
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
