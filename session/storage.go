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
	// BacklogItem is the backlog item this session was started for (backlog.json
	// holds the authoritative link). omitempty: an older af drops it on its next
	// write and loses only the client chip.
	BacklogItem int `json:"backlog_item,omitempty"`
	// CreatedBySandbox is the id of the session whose sandbox callback credential
	// created this one (#5571 W7.8), empty for a session the operator created. It
	// is durable provenance: such a session never receives push credentials,
	// because eligibility comes only from an operator decision, never from an
	// action a sandbox initiated. omitempty + additive.
	CreatedBySandbox string `json:"created_by_sandbox,omitempty"`
	// CreatedByOperator is the positive half of that provenance: set when the
	// operator's own authority created the session — every create no sandbox
	// credential asked for, since the daemon stamps CreatedBySandbox at the one
	// route a sandbox can create through (createSessionScoped) and a
	// sandbox-created task never spawns a session. A row with neither set has
	// UNKNOWN provenance (written before af recorded it, or by a release that
	// drops these fields) and is treated like a sandbox-created one, failing
	// closed: an operator re-creates it to make it eligible (security review of
	// #6433, N1). omitempty + additive.
	CreatedByOperator bool   `json:"created_by_operator,omitempty"`
	Title             string `json:"title"`
	Path              string `json:"path"`
	Branch            string `json:"branch"`
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
	// local-worktree backend, or an ssh/sandbox one whose agent-server advertises
	// agent-swap (Capabilities().Handoff), in a runtime state that admits
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
	// TaskRunInterrupted records that the run ended interrupted by a runtime
	// replacement — Lost-restore respawn or load-time re-spawn — rather than on
	// an agent-idle completion edge (#4222). Persisted because the daemon's
	// startup reconcile must recognize the row: a pending fire marker that
	// still names it describes an occurrence that was never delivered to the
	// replacement runtime, so adopting the row as the fire's product would
	// misreport the interrupted run as started and discharge a prompt that was
	// never replayed. omitempty + additive: an older record decodes to false —
	// indistinguishable from a normally-ended run (the pre-fix reading).
	TaskRunInterrupted   bool      `json:"task_run_interrupted,omitempty"`
	TaskRunInterruptedAt time.Time `json:"task_run_interrupted_at,omitzero"` // #6223 re-arm window
	// TaskRunFailurePending records that the run ended failed (initial prompt
	// proven undelivered, #5278) and the task's errored status has not yet been
	// acknowledged as written (#5646). omitempty + additive: an older record
	// decodes to false, the pre-fix behaviour.
	TaskRunFailurePending bool `json:"task_run_failure_pending,omitempty"`
	// TaskRunPromptBound records that the run's prompt boundary was bound at
	// least once (#5644). A cleared task_run_prompt_attempt_at can mean "never
	// bound" or "bound then retired by a runtime replacement" (handoff); only
	// the former may end the run failed on a PromptNotDelivered verdict.
	// Persisted while the run is in flight, same as the boundary it qualifies.
	TaskRunPromptBound bool `json:"task_run_prompt_bound,omitempty"`
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
	// BacklogRunActive is the backlog run marker (Instance.backlogRunActive). The
	// task-run gate fields above carry its prompt boundary and held edges.
	BacklogRunActive bool `json:"backlog_run_active,omitempty"`
	// BacklogRunEndedAt is when the latest backlog run ended; the on_done
	// archive treats pane churn after it as the user taking the session over.
	BacklogRunEndedAt time.Time `json:"backlog_run_ended_at,omitzero"`
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
	// ArchiveNonce is the occupant nonce stamped into the archived worktree's registration leaf (#5189).
	ArchiveNonce string `json:"archive_nonce,omitempty"`
	// PrunedAt marks a prune tombstone: the worktree was deleted, the branch kept (#5189).
	PrunedAt time.Time `json:"pruned_at,omitzero"`
	// PruneIntent is the durable record written before prune's first irreversible step (#5189).
	PruneIntent *PruneIntentData `json:"prune_intent,omitempty"`
	Prompt      string           `json:"prompt,omitempty"`
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
	// Host is the listed host the session was placed on (#5571 W6.3); additive.
	Host string `json:"host,omitempty"`
	// BranchUnpushed is true only for a branch af named at create and has not
	// seen pushed (#5972). It marks the exception rather than the proof so every
	// row an older af wrote — where a recorded branch could only have come from
	// a push — stays restorable: absent means "pushed", as it always did.
	BranchUnpushed bool `json:"branch_unpushed,omitempty"`
	// ArchivedWorkspace says where an archived off-box session's work lives
	// (#5571 W8.5): the preserved directory on its host, or the pushed branch.
	// Absent on every row archived before the field, and on live rows.
	ArchivedWorkspace *ArchivedWorkspaceData `json:"archived_workspace,omitempty"`
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
	// PendingRemoteTabClose is PendingTabCleanup for off-box tabs: closes the
	// sandbox never confirmed (#5571, #5957). The roster row is gone, so this
	// record is the only handle on a command that may still be running there.
	PendingRemoteTabClose []RemoteTabCleanupData `json:"pending_remote_tab_close,omitempty"`
	// RebuildTeardownPending records that a recovery rebuilt this worktree at a
	// new identity while sessions bound to it still answered has-session, and
	// the teardown of those stale panes is unconfirmed. The next attempt cannot
	// re-derive a committed rebuild from os.Stat (the rebuilt directory
	// exists), so the obligation to remove the pre-rebuild panes — whose cwd is
	// the deleted inode — persists (#5370, #5371 review). Set at rebuild commit,
	// cleared only once every bound name and cleanup handle is confirmed gone.
	RebuildTeardownPending bool `json:"rebuild_teardown_pending,omitempty"`
	// RebuildTeardownOriginalStartupStateUnknown is a storage-only rollback
	// fence: a current reader removes it before interpreting the row, while the
	// previous release sees StartupStateUnknown and leaves the row undriven —
	// it would otherwise find the rebuilt worktree present, skip the rebuild,
	// reattach the deleted-inode pane, and mark it Running (#5370, #5371 review).
	RebuildTeardownOriginalStartupStateUnknown *bool `json:"rebuild_teardown_original_startup_state_unknown,omitempty"`
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
	// Attention is the agent's own request for the user (`af sessions
	// attention`). Persisted: a restart does not answer the question the agent
	// asked.
	Attention *Attention      `json:"attention,omitempty"`
	Worktree  GitWorktreeData `json:"worktree"`
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
	// Placement is where the session runs (#5571 W6.4): derived on every snapshot
	// so each surface can show host, local-versus-off-box and workspace location.
	// ForStorage sets it back to nil, so a derived fact never reaches
	// instances.json.
	Placement *PlacementData `json:"placement,omitempty"`
	// TabManagement is Capabilities.TabManagement projected (#5953): whether this
	// session may gain shell and process tabs. For an off-box row the daemon lifts
	// it from the agent-server's advertisement, which a client's snapshot row
	// cannot read, so without the projection the TUI refuses new remote tabs.
	// A pointer so that false survives the wire; ForStorage clears it.
	TabManagement *bool `json:"tab_management,omitempty"`
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
	// CallbackUnavailable is the reason this session has no af callback (#5571
	// W3.1) — the mint refusal recorded when its sandbox provisioned without one,
	// or the in-sandbox agent-server's undelivered-credential notice. Persisted
	// like the LostRestoreFailure durability class: it survives restarts so the
	// surfaces never silently drop the explanation, and it is additive +
	// omitempty so older binaries read and rewrite the row untouched.
	CallbackUnavailable string `json:"callback_unavailable,omitempty"`
	// AgentConfigSkipped is one "agent:file: <reason>" entry per declared
	// sandbox_agent_config file the account-scope placement refused (#6337,
	// #5571 W7.7) — recorded at provision and refreshed by every reprovision and
	// account swap that stages a scope. Persisted in the same durability class
	// as CallbackUnavailable: the skip is still true after a restart, so the
	// row must keep saying so until positive evidence (a placement that copies
	// everything) clears it. Additive + omitempty for the same reason.
	AgentConfigSkipped []string `json:"agent_config_skipped,omitempty"`
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
	// Reverse projection order — see FromInstanceData: the rebuild-teardown
	// projector runs last of the pending projections in ForStorage (after
	// relocation), so its restore runs here, ahead of the handoff and
	// account-swap restores.
	d = d.RestoreRebuildTeardownRollbackFence()
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
// disk first. One on-disk read remains, under the repo file lock, serving two
// preservation purposes for rows the in-memory snapshot cannot see or has
// dropped:
//
//   - A mid-archive sandbox snapshot re-reads the repo so a newer committed
//     archive outcome for the same stable identity can displace the stale
//     pre-push in-memory snapshot.
//   - Every checkpoint re-reads the repo so on-disk rows the daemon did NOT
//     materialize (unloadable "ghost" rows the loader deliberately left on
//     disk for repair, #4162/#1892/#3310) survive the wholesale rewrite. The
//     carve-out for an older binary's disposable Loading/Deleting projection
//     (#551) is reaped; durable ghosts are preserved verbatim.
func (s *Storage) SaveInstances(instances []*Instance) error {
	return s.saveInstances(instances, config.WithFileLock)
}

// SaveInstancesWithin is SaveInstances with a DEADLINE on every instances-flock
// wait, shared across all repos in the call: a lock holder that never lets go
// surfaces as config.ErrLockTimeout once budget is spent instead of parking the
// caller. Nothing is written for a repo whose lock was not acquired — the
// checkpoint never proceeds unlocked. Repos checkpointed before the timeout
// stay written. Callers on a path with its own budget (the daemon's missing-file
// restore, bounded by the poll/shutdown grace) use this form (#6048 review).
func (s *Storage) SaveInstancesWithin(instances []*Instance, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	return s.saveInstances(instances, func(path string, fn func() error) error {
		// A spent budget (0) is a genuinely non-blocking attempt: a free lock is
		// never refused for being late, a held one times out at once without
		// sleeping, so a pass over many repos cannot overrun the shared deadline.
		return config.WithFileLockTimeout(path, max(time.Until(deadline), 0), fn)
	})
}

// saveInstances is the checkpoint body; withLock is how each repo's instances
// flock is taken.
func (s *Storage) saveInstances(instances []*Instance, withLock func(path string, fn func() error) error) error {
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
		// inMemoryTitles is the set of titles the daemon holds in memory for
		// THIS repo. Titles are unique per repo (the daemon's
		// findTitleConflictLocked enforces this on create), so a title match is
		// a same-logical-session match. preserveUnmaterializedGhostRows uses it
		// to recognize an on-disk row that is the durable record for a live
		// (even if checkpoint-dropped) instance and must not be re-persisted as
		// a ghost — including a killed instance the snapshot legitimately
		// drops (#819/#844). Scoped per repo: a same-titled session in a
		// DIFFERENT repo is a distinct logical session and must not silence
		// this repo's ghost.
		inMemoryTitles := make(map[string]struct{}, len(repoInstances))
		for _, inst := range repoInstances {
			if inst.Title != "" {
				inMemoryTitles[inst.Title] = struct{}{}
			}
		}
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := withLock(path, func() error {
			checkpoint := snapshotInstancesForCheckpoint(repoInstances)
			if !checkpoint.shouldWrite() {
				return nil
			}
			group := checkpoint.group
			// Read the on-disk rows for this repo under the file lock. The
			// checkpoint rewrites the repo's instances.json wholesale from the
			// in-memory group, so without preserving rows the daemon did not
			// materialize the rewrite would silently erase every unloadable
			// ("ghost") row in the file — the opposite of the
			// durable-across-restarts promise the loader makes to those rows
			// (#4162/#1892/#3310). The archive reconciliation below used to be
			// the only on-disk read; this single read generalizes it so a mixed
			// repo's ghosts survive the shutdown checkpoint too. A read failure
			// propagates: silently proceeding would let the wholesale rewrite
			// clobber ghost rows (or a committed archive row) with the stale
			// in-memory snapshot, the exact data loss (#938) the file lock
			// exists to prevent.
			raw, readErr := s.state.GetInstances(rid)
			if readErr != nil {
				// Classify the read failure: a newer-schema file or an
				// I/O failure propagates so the checkpoint does not clobber
				// rows it cannot read (#4783/#766); a corrupt-but-readable
				// file falls back to overwrite (#938). GetInstances maps a
				// missing file to "[]".
				if err := checkpointReadError(readErr, path); err != nil {
					return fmt.Errorf("checkpoint: read disk for repo %s: %w", rid, err)
				}
				raw = nil
			}
			// onDisk is the decoded form used for matching (title), the
			// disposable-ghost carve-out, and archive reconciliation. onDiskRaw
			// is the parallel raw form preserved verbatim so additive fields a
			// newer binary wrote under the same schema version — which this
			// binary's InstanceData does not know and would silently drop on a
			// decode/encode round trip — survive the checkpoint (#6).
			onDisk, onDiskRaw := decodeOnDiskRows(raw)
			if checkpoint.needsArchiveReconcile() {
				reconcilePendingArchiveRows(group, checkpoint.inFlightArchive, onDisk)
				group = mergeCommittedArchiveRows(group, onDisk, checkpoint.prePushArchive)
			}
			// A same-titled on-disk row whose stable ID differs from the
			// in-memory instance is a replacement the daemon has not observed:
			// refreshDaemonInstances keeps the old in-memory pointer by title
			// even when the disk row's ID differs, so the checkpoint would write
			// the stale instance and clobber the replacement's identity — the
			// data-loss class persistInstanceData's title+ID guard exists to
			// prevent. Drop the stale in-memory row and let the on-disk
			// replacement survive the wholesale rewrite verbatim (retained below
			// outside the ghost reaper). An empty ID on either side is a legacy record and still
			// matches, mirroring stableIDMatchesForDaemon (#6048 review).
			//
			// Duplicate disk titles are resolved FIRST, newest UpdatedAt wins (ties
			// keep the earliest), the same rule dedupeInstanceData applies: only
			// that authoritative row is compared with the live instance, so an older
			// stale duplicate cannot supersede a current row matching it.
			authoritative := make(map[string]int)
			for i, d := range onDisk {
				if prev, ok := authoritative[d.Title]; !ok || d.UpdatedAt.After(onDisk[prev].UpdatedAt) {
					authoritative[d.Title] = i
				}
			}
			replaced := make(map[string]struct{})
			replacedID := make(map[string]string)
			for i := range group {
				if idx, ok := authoritative[group[i].Title]; ok && !stableIDMatches(onDisk[idx].ID, group[i].ID) {
					replaced[group[i].Title] = struct{}{}
					replacedID[group[i].Title] = group[i].ID
				}
			}
			var replacements []json.RawMessage
			if len(replaced) > 0 {
				// The conflicting on-disk row is retained UNCONDITIONALLY and
				// verbatim: handing it to preserveUnmaterializedGhostRows would let
				// that helper reap it as a disposable legacy transient when it is a
				// Loading/Deleting row with no retention marker, and with the stale
				// in-memory row already dropped the checkpoint would then lose both
				// records and orphan the replacement session (#6048 review).
				best := make(map[string]int)
				restDisk := make([]InstanceData, 0, len(onDisk))
				restRaw := make([]json.RawMessage, 0, len(onDisk))
				for i, d := range onDisk {
					if _, isReplaced := replaced[d.Title]; !isReplaced {
						restDisk = append(restDisk, d)
						if i < len(onDiskRaw) {
							restRaw = append(restRaw, onDiskRaw[i])
						}
						continue
					}
					if i != authoritative[d.Title] {
						continue // an older duplicate of the replacement: superseded
					}
					best[d.Title] = i
				}
				for _, i := range best {
					raw := json.RawMessage(nil)
					if i < len(onDiskRaw) {
						raw = onDiskRaw[i]
					} else {
						raw, _ = json.Marshal(onDisk[i])
					}
					replacements = append(replacements, stripRemovedInstanceDataWireKeys(raw))
				}
				onDisk, onDiskRaw = restDisk, restRaw
				kept := make([]InstanceData, 0, len(group))
				for _, g := range group {
					if _, drop := replaced[g.Title]; drop {
						continue
					}
					kept = append(kept, g)
				}
				group = kept
				for title := range replaced {
					delete(inMemoryTitles, title)
				}
			}
			ghosts := append(preserveUnmaterializedGhostRows(onDisk, onDiskRaw, inMemoryTitles), replacements...)
			jsonData, err := marshalCheckpointInstances(group, ghosts)
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
