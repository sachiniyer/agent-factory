package session

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Instance is a running instance of claude code.
type Instance struct {
	// mu protects fields that are accessed concurrently by async Start()
	// goroutines (writers) and the main bubbletea loop (readers):
	// started, liveness/inFlightOp, Tabs (and the agent tab's tmux session),
	// gitWorktree, diffStats.
	mu               sync.RWMutex
	agentObservation *agentObservationRuntime
	// agentObservationSettlementMu extends the runtime's transport lock through
	// daemon interpretation, so an automated send cannot overtake a completed
	// pane snapshot whose usage-limit conclusion has not been published yet.
	agentObservationSettlementMu sync.Mutex
	// agentObservationGeneration is atomic because daemon-owned side effects
	// validate an observation while holding Manager.mu, not Instance.mu. Runtime
	// replacement invalidates it before later manager bookkeeping, which orders a
	// predecessor side effect either wholly before the replacement or drops it.
	agentObservationGeneration atomic.Uint64
	// forceSandboxReplacement is the operator's explicit "replace this sandbox"
	// intent (`sessions restore --force-reap`) for the recovery in flight; it makes
	// recovery skip the in-place relaunch. See relaunchOrRecoverSandbox.
	forceSandboxReplacement atomic.Bool
	liveBoundaryMu          sync.Mutex
	liveBoundary            *runtimeLiveBoundary
	// ID is the instance's stable identity (#1195): a random UUID minted once at
	// NewInstance, persisted, and never mutated. The reconcile uses it to tell
	// "same session" from "title reused" (#765) without leaning on CreatedAt
	// equality — the audit's identity-by-circumstance gotcha (a manufactured or
	// zero-CreatedAt record silently degraded a swap into an in-place corpse
	// mutation). Legacy records persisted before #1195 carry no ID; the reconcile
	// receives a freshly minted in-memory ID from FromInstanceData, while daemon
	// load durably backfills that ID before materialization. Immutable after
	// construction, so cross-goroutine readers may read it without the mutex (like
	// Title).
	ID string
	// TaskID is the id of the task whose delivery spawned this session, empty for
	// a user-created one (#1892). It is the daemon's association between a task
	// delivery and its session, replacing title-prefix guessing; the watch-task
	// concurrency limit counts a task's in-flight sessions by it. Immutable after
	// construction, so cross-goroutine readers may read it without the mutex
	// (like ID and Title).
	TaskID string
	// BacklogItem is the id of the backlog item this session was started for, 0
	// for any other session. Immutable after construction. backlog.json is the
	// source of truth for the item ↔ session link; this is a cache for clients
	// and the one input restart recovery matches a starting run against.
	BacklogItem int
	// createdBySandbox and createdByOperator are InstanceData.CreatedBySandbox
	// and CreatedByOperator. Immutable after construction.
	createdBySandbox  string
	createdByOperator bool
	// Title is the title of the instance.
	Title string
	// Path is the path to the workspace.
	Path string
	// storageRepoID retains the instances/<repoID> key this row was loaded from,
	// preventing checkpoints from silently migrating legacy rows (#3358). It is
	// empty until a fresh instance's first save and is protected by mu.
	storageRepoID string
	// repoIdentityPath is the owning repo's canonical identity
	// (RepoContext.IdentityPath), resolved at create and kept for backends that
	// own no local worktree — the same value the pending-create row publishes as
	// Worktree.RepoPath. Path is the operational workspace; the two diverge when
	// the repo was registered through a bare repository's linked worktree, so
	// repo-scoped consumers must not re-derive identity from Path. Protected by
	// mu.
	repoIdentityPath string
	// Branch is the branch of the instance.
	Branch string
	// liveness and inFlightOp are the two orthogonal axes of session state
	// (#1195): liveness is the daemon-owned health of the backing session
	// (Running/Ready/Lost/Archived/…), inFlightOp is the transient client
	// operation overlaid on it (Creating/Killing/Archiving/Restoring). Snapshots
	// carry the op so secondary TUIs converge; disk persistence strips it. The
	// legacy Status enum is derived from them via the GetStatus/SetStatus shim in
	// liveness.go. Both are mutex-protected.
	liveness   Liveness
	inFlightOp InFlightOp
	// lostRestoreFailure is the durable terminal outcome of automatic recovery.
	// It gates only the automatic retry loop; an explicit restore may replace the
	// runtime and clear it at the live boundary.
	lostRestoreFailure LostRestoreFailure
	// rebuildTeardownPending records that a recovery rebuilt the worktree at a
	// new identity but did not finish tearing down the sessions that survived
	// the deletion. `rebuilt` inside respawnWithConversation is re-derived per
	// attempt from os.Stat, which sees the rebuilt directory on the retry and
	// reports false — without this flag a partial teardown would let the next
	// attempt reattach the stale pane and ConfirmLive it (#5371 review). Set
	// when the rebuild commits, cleared only after every bound session and
	// pending-cleanup handle is confirmed gone.
	rebuildTeardownPending bool
	// rebuildTeardownPendingOwed marks that the in-memory flag's current value
	// is NOT durably backed: a transition attempted the persist callback and it
	// failed, or ran without one installed. A value only skips the checkpoint
	// when it is the durable one — an owed value must re-fire the notify on the
	// next matching call rather than letting the rebuild proceed on a flag a
	// restart would reload differently (#5371 review).
	rebuildTeardownPendingOwed bool
	// rebuildTeardownStopRuntimes is the daemon-installed "stop what no tmux
	// session owns" callback the rebuild teardown must run before it may
	// believe the worktree is clear: the supervised VS Code editor and any
	// post-worktree hook runner hold a cwd inside the checkout while carrying
	// no tab.tmux and no AF_SESSION marker, so the tmux snapshot cannot see
	// them and the occupancy check would find them as strangers (#5371
	// review). Same installation contract as rebuildTeardownNotify — nil on a
	// teardown path means an entry point was missed, and it fails closed.
	rebuildTeardownStopRuntimes func(*Instance) error
	// rebuildTeardownNotify is the daemon-installed persist callback fired on
	// every rebuildTeardownPending transition, so a durable obligation and its
	// durable discharge both land before the code that depends on them runs —
	// the clear must be on disk before a replacement pane spawns, or a daemon
	// exit in that window reloads a stale `true` and tears the FRESH panes
	// down as though they belonged to the deleted worktree (#5371 review).
	// In-memory only: the flag matters only inside Recover, and every Recover
	// entry point installs it anew.
	rebuildTeardownNotify func(*Instance) error
	// lostRestoreWait is the durable non-terminal counterpart: the loop's last
	// failure was environmental, so an automatic retry is still armed behind a
	// capped backoff instead of the terminal budget being spent (#5295).
	lostRestoreWait LostRestoreWait
	// taskRunActive is THE fact the watch-task concurrency cap is about (#1892):
	// has this session's task run finished yet? It is true from creation for a
	// task-spawned session and flips false — once, permanently — when the AGENT
	// first goes idle, or when startup settles terminal-unknown without ever
	// establishing a runnable session. Either outcome means no run remains that a
	// later poll could observe finishing.
	//
	// It is a stored fact rather than something derived at read time because every
	// neighbouring signal answers a DIFFERENT question, and reconstructing the run
	// from them is what produced two separate cap breaches:
	//
	//   - liveness says whether the daemon can SEE the session. Lost is
	//     indistinguishable between a run that finished hours ago and one
	//     interrupted mid-flight, so deciding at the Lost edge let a completed run
	//     reacquire a slot from the grave.
	//   - inFlightOp says the DAEMON is doing something — and archiving or killing a
	//     session is teardown, not the agent working. Reading "any op ⇒ busy" made a
	//     finished session that failed to archive (LiveReady + OpArchiving →
	//     AbortArchiveToLost) look like an interrupted run and claim a slot.
	//
	// So the run's own lifetime is recorded on the run's own edges: it begins when
	// the session is created for a delivery and ends when the agent goes idle or
	// startup reaches its explicit terminal-unknown boundary. Neither has to be
	// inferred later from a neighbouring state. Persisted, because an outage that
	// loses sessions is the same event that restarts the daemon.
	//
	// It never flips back to true: a capped task creates one session per event (a
	// cap and a target_session are mutually exclusive — see task.ValidateTrigger),
	// so a session has exactly one run. Work a user starts in that session
	// afterwards is theirs, not the task's, and must not consume the task's cap.
	taskRunActive bool
	// taskRunIdleEdgeHeld records that the agent's idle edge arrived while a
	// handoff mission was still owed (#4429), so the run was kept open rather
	// than ended. The edge is spent once it is held — the pane stays Ready, and
	// every later idle poll is Ready → Ready — so without this the run would
	// never end after the blocker cleared without a fresh edge (Mark delivered,
	// or churn folded in while the row stays Ready). With it, the first idle
	// observation after the blocker clears ends the run, as the held edge would
	// have. Meaningful only while taskRunActive; persisted with it, because the
	// edge is not re-derivable.
	//
	// This field keeps its ORIGINAL meaning deliberately: a daemon rolled back
	// to a release that knows only the mission hold reads this bit exactly as
	// it wrote it. The turn-gate hold (#5219) persists separately in
	// taskRunTurnGateHeld so the older binary cannot mistake it for a resolved
	// mission hold and end a run whose agent never took the turn (#5221 review).
	taskRunIdleEdgeHeld bool
	// taskRunTurnGateHeld is the #5219 twin of taskRunIdleEdgeHeld: an idle edge
	// arrived while the run's prompt had been attempted without turn evidence,
	// so the run stayed open. Kept as a separate persisted field so a rollback
	// to the previous release — which knows only the mission hold — drops the
	// marker instead of releasing it as a retired mission edge.
	taskRunTurnGateHeld bool
	// agentRuntimeReplaced records whether the runtime a recover or load just
	// re-established is a REPLACEMENT process — a respawn or remote
	// re-provision, or a reattach whose pane identity could not be verified —
	// rather than the recorded agent still running. Backends assign it on every
	// re-establishment, before their ConfirmLive edge (or before Start(false)
	// returns), so the run-end classifier reads this attempt's verdict and
	// never a stale one: replaced ends an in-flight task run as interrupted;
	// a verified reattach leaves the run to the agent that still owns its
	// prompt (#4222).
	agentRuntimeReplaced bool
	// taskRunInterrupted records that this session's task run ended
	// interrupted — the runtime that owned its prompt was replaced without a
	// replay (#4222). It is what tells "the marker cleared" apart from "the
	// run completed" for the daemon's completion edge, which may have captured
	// taskRunWasActive=true on a tick that raced the restore. Sticky for the
	// process's lifetime: a session has exactly one run, and an interrupted
	// end is a fact about it that never un-happens. In-memory only; a restart
	// leaves no poll able to capture the active half of the edge.
	taskRunInterrupted   bool
	agentReplacedAt      time.Time // when the replacement was launched (#6231)
	taskRunInterruptedAt time.Time // bounds the re-arm window (#6223); persisted
	// taskRunInterruptedStamp is the one-shot twin of taskRunInterrupted for
	// the daemon's status write: set when a restore's ConfirmLive performs the
	// interrupt, consumed once by noteRuntimeReplaced so the owning task's
	// last_run_status is stamped exactly once — a later restore with no run to
	// interrupt must not rewrite a newer status. A restart in the gap keeps the
	// prior status while the durable fact — the cleared marker — was persisted
	// by the restore's own settlement.
	taskRunInterruptedStamp bool
	// taskRunFailed records that this session's task run ended failed — its
	// prompt was PROVEN undelivered on the initial send (#5278). Sticky for
	// the process's lifetime: a session has exactly one run, and the daemon's
	// completion edge may have captured taskRunWasActive on a tick that raced
	// the failed send — this is what tells "the marker cleared" apart from
	// "the run completed". A restart re-derives the same end from the persisted
	// verdict at FromInstanceData, so it never needs disk of its own.
	taskRunFailed bool
	// taskRunFailedStamp is the one-shot twin of taskRunFailed for the daemon's
	// status write: set where the run ends failed, consumed once by the poll's
	// run-end observation so the owning task's last_run_status is stamped
	// exactly once per process. A restart re-derives the same end from the same
	// durable verdict, so the re-stamp is idempotent.
	taskRunFailedStamp bool
	// taskRunFailurePending is the DURABLE twin of taskRunFailedStamp (#5646):
	// raised where the run ends failed, persisted with the row, and cleared only
	// when the daemon acknowledges the task's status write (or its refusal).
	// The in-memory stamp dies with the process; this survives a restart that
	// lands between the edge and the stamp, which would otherwise leave the task
	// reading "started" forever for a run that provably failed.
	taskRunFailurePending bool
	// taskRunPromptBound records that this run's prompt boundary was bound at
	// least once — the initial prompt reached a verdict that could have
	// delivered (#5644). ClearIdleEvidence retires the boundary itself when a
	// runtime is replaced (handoff), but the bound bit stays: a later
	// PromptNotDelivered on a replacement mission is NOT the initial delivery
	// failing, and only a never-bound run may be ended failed by it. Persisted
	// while the run is in flight, with the boundary it qualifies.
	taskRunPromptBound bool
	// backlogRunActive is the backlog's run marker. Unlike taskRunActive it is
	// re-armed each time the backlog delivers a prompt, so one item session has
	// many runs. It shares the task run's completion gate (prompt boundary, turn
	// evidence, held edges); a session is never both a task and an item session.
	backlogRunActive bool
	// backlogRunEndedAt is when the latest backlog run ended (persisted).
	backlogRunEndedAt time.Time
	// backlogRunGen counts the runs armed on this session (in memory only): a
	// completion queued for run N must not finish run N+1 (ArmBacklogRun).
	backlogRunGen int
	// backlogRunEndedAtBeforeArm is backlogRunEndedAt as ArmBacklogRun found it,
	// for DisarmBacklogRun to restore (in memory only).
	backlogRunEndedAtBeforeArm time.Time
	// adoption counts the deliveries that make a finished task session the USER's
	// and fences them against its declared teardown (#3865). Guarded by i.mu; see
	// adoption_fence.go, which owns the whole contract.
	adoption adoptionFence
	// owedOnComplete is the in-memory form of InstanceData.PendingOnComplete
	// (#4162): set when the daemon files the obligation, cleared when a decision
	// discharges it. owedOnCompleteNotify is the daemon-installed persist
	// callback a delivery fires after clearing it, and it returns any persist
	// error so NoteAdoptionDelivery can propagate it and refuse the PTY write
	// rather than proceed on top of a discharge that did not land — adoption
	// evidence must become durable immediately or a restart resurrects a teardown
	// the user already vetoed. owedDrainActive claims the obligation's lifecycle
	// worker, so a refresh that re-arms the marker mid-wait cannot launch a
	// second teardown beside the one already parked on the hook channel. All
	// guarded by mu; the claim is in-memory only, which is correct — a new
	// daemon generation has no workers in flight.
	//
	// discharge is the in-flight durable discharge of owedOnComplete the first
	// concurrent NoteAdoptionDelivery caller installs under mu before it
	// releases mu for the notify to run. A second caller arriving while the
	// notify is in flight finds owedOnComplete already nil (the first cleared
	// it) and discharge set; it waits on the discharge's future so its PTY
	// write is gated on the same durable clear, instead of bypassing the
	// in-flight clear to a PTY write the durable marker would survive. The
	// discharging caller clears this field under mu right before it returns,
	// closing the future once the notify's persist result is known. See
	// adoption_fence.go for the full contract; in-memory only because every
	// delivery installs a fresh one.
	owedOnComplete       *PendingOnCompleteData
	owedOnCompleteNotify func(*Instance) error
	owedDrainActive      bool
	discharge            *adoptionDischarge
	// relocationSettleNotify is the daemon-installed persist callback the
	// respawn fires after SettleWorktreeRelocationClaim resolves a single-
	// candidate recovery record but BEFORE the spawn proceeds — the settle is
	// in-memory only, and a daemon exit before the later boundary write would
	// reload the stale record and strand the session inert (#5295 review).
	// In-memory only; each daemon generation re-installs it at its
	// recover/respawn entry points.
	relocationSettleNotify func(*Instance) error
	// limitResetAt is the parsed usage-limit reset time (#1146), display-only in
	// PR2: set alongside liveness == LiveLimitReached when the pane shows a limit
	// banner carrying a parseable reset time (zero when it carried none). Read
	// only while limit-blocked (LimitResetAt/ToInstanceData gate on the liveness),
	// so a lingering value on a recovered session never surfaces. Persisted and
	// carried in the daemon snapshot so the badge survives a restart; PR3's
	// auto-resume scheduler reads it. Mutex-protected.
	limitResetAt time.Time
	// limitAgent and limitAccount attribute the current limit after Program or
	// Account changes. Account labels are scoped to an agent, so neither field
	// alone identifies the provider identity that produced the wall.
	limitAgent string
	// Empty means the agent's ambient identity.
	limitAccount string
	// accountLimitObservations keep named identity walls after liveness clears.
	accountLimitObservations []AccountLimitObservationData
	// agentModelChange is a live, projection-only diagnostic supplied by the
	// running agent-server. It is mutex-protected and deliberately omitted from
	// durable restore state; see AgentModelChange and InstanceData.ForStorage.
	agentModelChange *AgentModelChange
	// archiveWarning retains a daemon snapshot's projection-only notice so thin
	// client renderers do not drop incomplete-archive state during reconstruction.
	archiveWarning string
	// callbackUnavailable retains the reason this session has no af callback:
	// the daemon's mint refusal on a sandbox it created, the in-sandbox
	// agent-server's undelivered-credential notice, or a mirror of either on a
	// client-side projection. Persisted; surfaces render it under
	// CallbackUnavailablePrefix. Mutex-protected; see callback_unavailable.go.
	callbackUnavailable string
	// agentConfigSkipped retains one "agent:file: <reason>" entry per declared
	// sandbox_agent_config file the account-scope placement refused (#6337,
	// #5571 W7.7). Persisted like callbackUnavailable and mirrored onto client
	// projections by ReconcileAgentConfigSkipped; surfaces render the joined
	// entries under AgentConfigSkippedPrefix. Mutex-protected; see
	// agent_config_skipped.go.
	agentConfigSkipped []string
	// hostedRemotely marks an instance running INSIDE a sandbox, where the daemon
	// holds the session as a remote workspace (guidance.go, #5571 W3.3).
	hostedRemotely       bool
	persistedSiblingTabs int // non-agent tabs the daemon's row records but this fresh hosted instance lacks (W1.4)
	// advertisedFeatures is what the hosting agent-server advertises on
	// /v1/health; guidance lifts the static remote floor by it (#5878).
	advertisedFeatures []string
	// worktreeMissing is true once a bounded probe proved the tracked worktree
	// path absent with no af relocation owning it — deleted outside af (#5102).
	// Persisted with the worktree record; see worktree_missing.go for who sets
	// and clears it. Mutex-protected.
	worktreeMissing       bool
	worktreeMissingReason string
	// Durable delivery and pane-churn evidence; never a semantic claim (#3168).
	lastPromptAttemptAt      time.Time
	lastPromptDeliveryStatus PromptDeliveryStatus
	lastPaneChurnAt          time.Time
	// taskRunPromptAttemptAt is the task run's OWN prompt boundary (#5219),
	// scoped apart from lastPromptAttemptAt: a manual send while the run is
	// active must not re-arm a delivery window the agent already satisfied.
	// The boundary updates on the first send after the run begins and on any
	// later send only while the window stays unsatisfied — a redelivery is
	// how a pending window starts fresh. Once the turn is taken the boundary
	// freezes; the run's remaining life cannot be reshaped by prompts the
	// session receives afterwards.
	taskRunPromptAttemptAt time.Time
	// taskRunTurnObservedAt records when the agent's own in-turn chrome was
	// observed for the run in flight — the positive "the prompt's turn began"
	// boundary that boot output and the send's own echo cannot produce (#5219).
	// Ordering against taskRunPromptAttemptAt does the scoping: a redelivery
	// starts a new window the stale record cannot release.
	taskRunTurnObservedAt time.Time
	// paneObservationStreakAt is the START of the current uninterrupted
	// observation streak: the first successful capture, or the first capture
	// after a hole (failed snapshots, a stalled serial walk) in which churn
	// could hide. Both release arms measure their silence from it — the
	// quiet arm from the later of the durable churn stamp and the streak,
	// the silent arm from the later of the attempt and the streak — because
	// an unobserved interval is never silence (#5221 review). Zero until
	// this process has actually seen the pane.
	paneObservationStreakAt time.Time
	// paneObservationExpiresAt is when the streak goes stale: the last
	// successful capture plus the gap slack its caller declared. A release
	// arm evaluated past it has no current observation to release on — a
	// run of failed captures cannot inherit a satisfied window the way a
	// never-rebased streak would (#5221 review).
	paneObservationExpiresAt time.Time
	// lastPaneCaptureAt is the completion stamp of the latest successful
	// capture, used to detect the observation holes that rebase
	// paneObservationStreakAt.
	lastPaneCaptureAt   time.Time
	loadRuntimeReplaced bool
	// stateEpoch is the generation counter for lifecycle state and prompt-observation
	// boundaries, bumped by every writer that changes one (#2135, #3168). It is how
	// an observer learns whether its captured-pane decision was superseded before it applies it;
	// see state_epoch.go. Mutex-protected, in-memory only: it describes a window
	// between an observation and its apply, and no such window survives a restart.
	stateEpoch uint64
	// agentRuntimeGeneration identifies the concrete agent process currently
	// owning the Agent tab. Async conversation capture binds to this generation,
	// so a result from a replaced process cannot write through a later handoff or
	// recovery merely because the Instance pointer (or even agent name) matches.
	// In-memory only: no capture goroutine survives a daemon restart.
	agentRuntimeGeneration uint64
	// runtimeEvidenceGeneration is the lock-free invalidation token for an
	// asynchronous consumer that snapshots runtimeProgram and later commits a
	// result under its own lock. It advances with both runtime-command changes
	// and lifecycle transitions, so a replacement fence invalidates predecessor
	// evidence before the new command is installed.
	runtimeEvidenceGeneration atomic.Uint64
	// Program is the program to run in the instance.
	Program string
	// runtimeProgram is the override-resolved command that the last positively
	// established agent runtime launched from, before AF's generated resume and
	// system-prompt arguments. Unlike Program (the requested agent label), this is
	// durable evidence about the process being adopted after a daemon restart.
	runtimeProgram string
	// runtimePID and runtimeStartID pin runtimeProgram to the concrete pane-root
	// process af launched: a surviving tmux NAME cannot prove it still names that
	// process, but the name's pane root carrying the recorded (pid, kernel
	// start-time) pair can — that is what lets a daemon restart's reattach keep
	// the launch claim instead of retiring it (#5066). Zero on records written
	// before this evidence existed and everywhere runtimeProgram is cleared.
	runtimePID     int
	runtimeStartID uint64
	// Account is the credential account this instance's agent runs as, or empty
	// for the ambient identity — which is the behaviour every session had before
	// #3051 and remains the default.
	//
	// Persisted, because the identity a session runs as must survive a restart:
	// a restored session that silently reverted to the ambient account would
	// spend the wrong quota while still displaying the account it was created
	// with.
	Account string `json:"account,omitempty"`
	// Host names the listed host (global `hosts`, #5571 W6.3) this session was
	// placed on, or empty when none was picked. Persisted independently of the
	// runtime cleanup handle, which a teardown clears: restore and recovery
	// re-provision from this so a pick never silently moves to another machine.
	Host string `json:"host,omitempty"`
	// branchUnpushed: Branch is the name af chose at create and nothing has
	// published it, so it is not a restore checkpoint (#5972). Persisted as
	// BranchUnpushed; false for any row an older af wrote.
	branchUnpushed bool
	// archivedWorkspace records where this archived off-box session's work lives
	// (#5571 W8.5); nil for a live row or one archived before the field.
	archivedWorkspace *ArchivedWorkspaceData
	// placement is the daemon-projected place this row runs (#5571 W6.4). Set on
	// a client row built from a snapshot, which has no runtime to derive it from.
	placement *PlacementData
	// projectedTabManagement is the daemon's verdict that this off-box session
	// serves shell and process tabs (#5953). A client row built from a snapshot has
	// no live agent-server to read the advertisement from, so it adopts the
	// projection instead.
	projectedTabManagement bool
	// accountAgent is the agent namespace Account was selected in. It must be
	// durable rather than re-derived at refresh time: a program_overrides edit
	// after the pin can resolve the recorded Program to a different agent's
	// command, and re-deriving from that new command would silently reinterpret
	// the same label in another agent's registry (#4430 review). Empty only when
	// Account is ambient or the record predates the field — those selections
	// could only have used the Program enum's namespace.
	accountAgent string
	// restartingSameAgent is raised, in-sandbox only, for the duration of an
	// account swap (#5571 W7.6): the replacement is the SAME agent on another
	// account, which the handoff validation otherwise refuses as "already running".
	// Every boundary of the handoff chain (prepare, record, swap) reads it.
	restartingSameAgent bool
	// accountAutoSelected distinguishes a scheduler choice; false keeps pre-#3127 accounts pinned.
	accountAutoSelected bool
	// pendingAccountSwap survives until the replacement notice and task land.
	pendingAccountSwap *AccountSwapData
	// accountSwapLaunch is the admitted command, rebuilt before a post-restart retry.
	accountSwapLaunch *accountSwapLaunchPlan
	// Height is the height of the instance.
	Height int
	// Width is the width of the instance.
	Width int
	// CreatedAt is the time the instance was created.
	CreatedAt time.Time
	// UpdatedAt is the last session state mutation time, guarded by mu.
	// Reads, serialization, and cache refreshes do not advance it.
	UpdatedAt time.Time
	// Prompt is the initial prompt to pass to the instance on startup
	Prompt string
	// pendingHandoffMission is a rendered takeover brief whose delivery has not
	// been durably confirmed. Its status is mission-scoped: session-wide prompt
	// evidence can be overwritten by unrelated work and cannot authorize replay.
	pendingHandoffMission string
	handoffDeliveryStatus PromptDeliveryStatus
	// inPlace is true when the instance was created with `--here`: on first
	// start it attaches to the repo's existing working tree at its current
	// branch (external worktree) instead of creating a fresh worktree+branch.
	// Set once by NewInstance and only read by LocalBackend.Start's first-time
	// setup; restored instances don't need it (the persisted
	// external_worktree flag carries the semantics from then on).
	inPlace bool
	// branchPrefix is the caller-resolved snapshot for a fresh local worktree.
	// branchPrefixResolved distinguishes an explicit empty prefix from a legacy
	// direct constructor that should resolve the value when provisioning begins.
	branchPrefix         string
	branchPrefixResolved bool
	// branchName is a caller-chosen branch that overrides the prefix+title
	// derivation for a fresh local worktree (InstanceOptions.BranchName).
	branchName string

	// carriedConversation is a provider conversation a PREVIOUS record held that
	// this create should come up on instead of starting a new one (#2616). It
	// exists for exactly one caller: the daemon's root-agent heal, which reaps
	// the vanished root's record — and with it agent_conversation — and creates a
	// replacement in place, where every other Lost session is re-spawned on its
	// own surviving record. Set by NewInstance or SetResumeConversation; not
	// consumed by a launch (root-recreate classification reads it afterwards);
	// retired when the account changes (retireConversationsLocked, #6189). A
	// REQUEST, not a fact: honored only when the resolved command can express it,
	// and the session's conversation is whatever the launch committed to Tabs[0].
	carriedConversation AgentConversationData

	// reportedConversation is the last conversation a remote agent-server reported
	// (recordReportedConversationFrom), held until the daemon-side agent tab exists to
	// carry it. Guarded by mu.
	reportedConversation AgentConversationData
	// conversationUnpersisted is set when a remote agent-server reported a
	// conversation identity that changed the row and the change has not yet been
	// durably written (#5826). The daemon's poll persistence reads it as a
	// durable change of its own and clears it only after the write succeeds, so
	// a failed write is retried on the next tick. Guarded by mu.
	conversationUnpersisted bool

	// carriedTabs is the tab roster a PREVIOUS record held, to be rebuilt onto
	// this instance once its agent tab is live (#2628). It has the same single
	// caller as carriedConversation — the root-agent heal, the one path that
	// replaces the record instead of re-spawning into it — and exists for the
	// same reason: the general Lost-restore path reconnects every persisted tab
	// by name, so without this a root that had a terminal, a process, a web, or
	// an editor tab comes back with only its agent tab after a tmux outage.
	//
	// Written once by NewInstance and consumed once by restoreCarriedTabs, which
	// clears it under i.mu so a second launch of the same object cannot rebuild
	// the same tabs twice.
	carriedTabs []TabData
	// pendingMetadataTabs stages tabs restored from a record whose workspace did not
	// survive; see appendPendingMetadataTabsLocked for why they are staged (#3062).
	pendingMetadataTabs []TabData

	// carriedRecreateNotice is an UNACKNOWLEDGED re-create notice inherited from
	// the record this create replaces (#2629). A root can lose its history and
	// then have tmux die again before anyone opens its pane; the second heal
	// classifies the second replacement, which answers a different question, so
	// without this floor a clean second heal would erase the warning about the
	// first. Write-once by NewInstance; read on every (re)classification.
	carriedRecreateNotice RootRecreateContext

	// rootRecreateContext is the one-shot note a re-created root carries when it
	// did not demonstrably come back on its prior conversation (#2629). Set at
	// first launch by noteRecreateContext, persisted, rendered on the row by
	// every rail, and cleared the first time a client opens the session's pane.
	rootRecreateContext RootRecreateContext

	attention *Attention

	// userKilled is the kill-intent tombstone (#1108): set (and persisted)
	// before an explicit kill's teardown begins, so a record that survives a
	// daemon crash or teardown failure mid-kill is provably a corpse the user
	// wanted dead — never classified Lost, never restored. The happy kill path
	// deletes the record entirely, so the tombstone is normally invisible; the
	// daemon poll finishes a tombstoned record's teardown instead of probing it.
	userKilled bool

	// startupStateUnknown means a create or restored child crossed a runtime
	// boundary but af could not determine whether it is live. The record is kept
	// inert: no liveness probe, restore, or automatic teardown may turn that
	// uncertainty into permission to delete its workspace (#2207, #3340).
	// Persisted so a daemon restart cannot re-arm the destructive path.
	startupStateUnknown bool

	// archivedAt records when this session's archive committed — the moment the
	// worktree move and teardown were declared done, local or remote (#5136).
	// It is the timestamp `af sessions prune --older-than` measures age from:
	// UpdatedAt cannot serve because it also advances on unrelated mutations
	// after the archive. Persisted; rows from before the field carry no value and
	// the cutoff falls back to UpdatedAt, which approximates it. Guarded by mu.
	archivedAt time.Time
	prune      pruneState // archive nonce, tombstone and intent (#5189); guarded by mu

	// backend abstracts session lifecycle (local tmux+git vs off-box runtimes).
	backend Backend
	// preResolvedProgram is the exact command an outer sandbox runtime already
	// selected. It is intentionally tied to the current Program string: if a
	// handoff rewrites Program, normal one-step override resolution resumes.
	preResolvedProgram string
	// sessionEnvPassthrough is an internal exact-name extension delivered by an
	// outer runtime's agent-server flags. Local sessions normally source the
	// same setting from global config during Provision.
	sessionEnvPassthrough []string
	// sandboxCreds mints and revokes this session's callback credential (#3068).
	// Held here because the RUNTIME's lifetime drives it: provisioning a
	// replacement mints, and reaping a runtime revokes. Set at construction for a
	// created session and attached by the daemon for one loaded from disk — without
	// that second half every restore after a daemon restart would silently skip it.
	// nil for a local session and for any instance with no daemon behind it.
	sandboxCreds SandboxCredentials
	// onSandboxRetired is called by reprovisionRemote after
	// reapRemoteRuntimeForReplacement returns without error — the moment the old
	// sandbox is provably gone and the new episode genuinely begins. The daemon
	// sets this before each Recover attempt to reset the failure-budget at the
	// right point: not before Recover is attempted (too early: reprovisionRemote
	// can fail before reaching the reap), and not only on success (too late:
	// a post-reap failure against the new sandbox earns attempt 1, not
	// maxAttempts+1). The hook fires at most once per Recover attempt; it is
	// cleared after firing or when reprovisionRemote returns early without firing.
	// Guarded by mu.
	onSandboxRetired func()

	// The below fields are initialized upon calling Start().

	started bool
	// Tabs is the instance's ordered list of tabs. In PR 1 of the #930
	// ephemeral-tabs epic this holds exactly one Agent-kind tab (Tabs[0]) that
	// wraps the instance's single tmux session; every tmux-touching method
	// routes through it via tmuxLocked/setTmuxLocked. Remote/hook-backed
	// instances drive their agent session through hook commands and so carry no
	// tmux-backed tab. Later PRs add shell/process tabs, lifecycle, and per-tab
	// persistence.
	Tabs []*Tab
	// tabRosterGeneration fences consent across authoritative roster reconciliations.
	// Guarded by mu; client-local, never persisted or sent over the wire.
	tabRosterGeneration uint64
	// gitWorktree is the git worktree for the instance.
	gitWorktree *git.GitWorktree
	// hookCreatePersistencePending bridges daemon create ownership across local
	// worktree provisioning. Guarded by mu.
	hookCreatePersistencePending bool
	// hookCreatePersistenceWorktree is the worktree whose progress lease was
	// armed. Teardown may clear gitWorktree before the create transaction settles,
	// so release must use this captured owner rather than a later lookup.
	hookCreatePersistenceWorktree *git.GitWorktree

	// agentSrv is the cached per-instance AgentServer (#1592 Phase 2 PR5). Cached
	// rather than reconstructed per call because its data plane holds stateful
	// pieces — the PTY output ring buffer and the fan-out subscriber set — that
	// must persist across calls; a fresh server each call would drop subscribers
	// and lose the replay buffer. Lazily built by AgentServer() and guarded by i.mu,
	// the SAME lock as remoteClient/runtimeTeardown (the fields that select which
	// impl it is): a cache and its source fields must share one mutex so a restore
	// that swaps the fields and clears the cache is atomic wrt the poll — a
	// dedicated agentSrvMu split them and let AgentServer() rebuild the cache from a
	// pre-restore snapshot, pinning a torn-down endpoint (#1729). Interface-typed
	// since #1592 Phase 4 PR2: AgentServer() returns the local in-process impl by
	// default, or a remoteAgentServer when remoteClient is set.
	agentSrv AgentServer
	// remoteClient is the runtime handle selecting the REMOTE agent-server impl
	// (#1592 Phase 4 PR2): when non-nil, AgentServer() returns a remoteAgentServer
	// driving the `af agent-server` this points at, instead of the local in-process
	// runtime. Built once at NewInstance from InstanceOptions.RemoteAgentServer (so
	// a bad endpoint fails there, keeping AgentServer() infallible) and nil for
	// every local session — the default path is provably unchanged. Set by the
	// docker runtime (#1592 Phase 4 PR4) from its provisioned container; nil for
	// local/hook sessions.
	remoteClient *remoteAgentClient
	// runtimeTeardown reaps the off-box sandbox a runtime provisioned (#1592
	// Phase 4 PR4): remove the container, SSH directory, or hook-provisioned
	// workspace. Set at NewInstance from the runtime's ProvisionResult and run by
	// the remote agent-server's Kill AFTER it tears the in-sandbox workspace down
	// over REST. nil only for local sessions. Each runtime serializes repeated
	// calls; docker/SSH deliberately retry outcomes whose completion is unknown.
	runtimeTeardown func() error
	// archivePushCompleted fences checkpoint retention on the push belonging to
	// the current OpArchiving generation. It is reset by BeginArchive, raised only
	// after AgentServer.Archive returns a non-empty durable branch, and cleared
	// when the archive transition settles. Branch is historical restore state and
	// therefore cannot answer whether the current push completed.
	archivePushCompleted bool
	// archiveCheckpointSealed is raised by the daemon's terminal checkpoint.
	// Once raised, no later archive generation may begin. archiveSettled is the
	// current generation's settlement fence: the checkpoint waits for it before
	// taking the snapshot that will survive process exit.
	archiveCheckpointSealed bool
	archiveSettled          chan struct{}
	// runtimeCleanupStateUnknown is the durable third outcome of a sandbox reap:
	// teardown neither confirmed success nor reported a completed failure, so the
	// exact runtime identity above must survive storage and be retried before any
	// replacement is provisioned. It is independent of userKilled: restore
	// failures retain wanted sessions, not kill tombstones.
	runtimeCleanupStateUnknown bool
	// pendingTabCleanup holds the tmux cleanup handles of tabs already durably
	// removed from Tabs whose teardown was never confirmed (#2669). Guarded by
	// i.mu like Tabs, because a close stages both under one acquisition and a
	// spawn must reserve against both in the same critical section. It is
	// deliberately NOT part of the roster: nothing renders or respawns from it.
	pendingTabCleanup []TabCleanupData

	// pendingRemoteClose is pendingTabCleanup for off-box tabs: closes the
	// sandbox never confirmed (#5957). Persisted with the session; retried by
	// ReconcileOffBoxTabs. Guarded by i.mu.
	pendingRemoteClose []RemoteTabCleanupData
	// tabCreateIntents holds the minted ids of remote creates whose outcome was
	// uncertain, keyed by request shape, so a retry reuses the id (#5958).
	// Guarded by i.mu; in memory — a restart before the retry loses the mapping,
	// and the sandbox's own roster then still holds the tab it started.
	tabCreateIntents map[string]*tabCreateIntent
	// tabTeardown serializes teardown attempts per tab id (lockTeardown).
	tabTeardown map[string]*teardownGate

	// tabSpawnClaims reserves the FULL tmux session names of sibling spawns
	// still in flight — derived under i.mu, released once the append resolves
	// (#5571 W4.3). uniqueTabTmuxNameReserving consults it alongside the roster
	// and pendingTabCleanup so two concurrent creates never derive the same
	// name: without it a loser's new-session collides inside tmux on the
	// winner's name and Start's failed-spawn cleanup kill-sessions that name —
	// the winner's live session. Guarded by i.mu; lazily allocated since an
	// instance that never spawns a sibling never needs it.
	tabSpawnClaims map[string]bool

	// tabDisplayClaims is tabSpawnClaims for the roster DISPLAY name: names
	// derived by in-flight creates but not yet on i.Tabs. Guarded by i.mu.
	tabDisplayClaims map[string]bool

	// tabCreateInFlight reserves the stable ids of supplied-id creates still
	// in flight — claimed under i.mu at the early check, released when the
	// owning create returns (#5571 W4.3). The roster dedupes same-id creates
	// only AFTER they spawn, which cannot undo a duplicate command's side
	// effects — a replayed `deploy.sh` that ran twice already deployed twice.
	// The claim serializes same-id creates instead: a second request joins on
	// the owner's done channel and resolves through the roster recheck, so a
	// matching retry adopts without spawning and a mismatched one refuses
	// before spawning. Guarded by i.mu; lazily allocated.
	tabCreateInFlight map[string]*tabCreateClaim

	// unreachableSince/unreachableErr are the live-probe OUTAGE observation
	// (#5571 W1.3): set when a remote observation goes unanswered, retired on
	// the next answered probe or any other transport evidence (attach stream,
	// runtime replacement, tombstone). They are an observation flag, not a
	// liveness — unreachable coexists with Running/Ready/Lost because an
	// unanswered probe is not evidence about the agent, and the remote-loss
	// debounce deliberately preserves last-known liveness underneath.
	// Projection-only: ForStorage scrubs them, so a restarted daemon re-derives
	// the claim from its own probes instead of inheriting an outage it never
	// observed. Guarded by i.mu.
	unreachableSince time.Time
	unreachableErr   string
}

// tmuxLocked returns the agent tab's tmux session, or nil when the instance has
// no agent tab yet (not started) or is remote. Callers must hold i.mu (read or
// write).
func (i *Instance) tmuxLocked() *tmux.TmuxSession {
	if len(i.Tabs) == 0 {
		return nil
	}
	return i.Tabs[0].tmux
}

// GetTabs returns a snapshot of the instance's tab list under the instance
// mutex. The returned slice is a copy, so callers (the UI tab bar) can iterate
// it without racing concurrent tab mutation.
//
// The *Tab elements are the LIVE pointers, not copies, and callers read their
// Name/Kind/ID off-lock (tree.TabLabels on the render path, the TUI's
// tabNameAt/tabIndexByName). That is safe because those fields are never
// assigned in place once a tab is in i.Tabs: a tab's name can change, but the
// writers that change it — RenameTab and ReconcileTabsFromData — swap in a
// COPY carrying the new value (replaceTabFieldLocked) instead of writing the
// object a reader is already holding. So a snapshot keeps reading the values it
// was taken with, and the next GetTabs observes the new ones. Anything that
// wants to change one of those fields must go through replaceTabFieldLocked;
// assigning to a handed-out tab's Name is a data race, not a stale read (#1930).
//
// This does NOT extend to the whole struct: tmux and Conversation are still
// assigned IN PLACE under i.mu (setTmuxLocked, setupTabs' dead-shell
// replacement, teardown's ref clearing). The package does read those off a
// snapshot in places — setupTabs and teardownTabs both capture tabs under the
// lock and work outside it — and that is safe only because the daemon's
// per-instance op-lock serializes start/teardown against every other mutation.
// That discipline lives in the daemon, not in this type: prefer the locking
// accessors (ToInstanceData), and if you read tmux/Conversation off
// a snapshot, know that is what you are leaning on.
func (i *Instance) GetTabs() []*Tab {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*Tab, len(i.Tabs))
	copy(out, i.Tabs)
	return out
}

// tabTmuxAtLocked returns the tmux session of the tab at idx, or nil when idx
// is out of range or the tab has no session. Callers must hold i.mu.
func (i *Instance) tabTmuxAtLocked(idx int) *tmux.TmuxSession {
	if idx < 0 || idx >= len(i.Tabs) {
		return nil
	}
	return i.Tabs[idx].tmux
}

// TabIDAt returns the stable id (#1738) of the tab at ordinal idx, and whether
// idx is in range. It is the index→id direction the data plane keys its per-tab
// broker on, so a broker follows its tab across a reorder/close instead of being
// pinned to a shifting ordinal.
func (i *Instance) TabIDAt(idx int) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if idx < 0 || idx >= len(i.Tabs) {
		return "", false
	}
	return i.Tabs[idx].ID, true
}

// tabTmuxTargetAt resolves an ordinal to both the stable broker key and its tmux
// target under one lock. Reading those in separate calls can pair one tab's ID
// with a sibling's tmux when a close shifts the roster between the calls (#2200).
// tabTmuxTargetAtLocked resolves an ordinal and its stable stream target in one
// snapshot. Callers must hold i.mu.
func (i *Instance) tabTmuxTargetAtLocked(idx int) (id string, ts *tmux.TmuxSession, exists bool) {
	if idx < 0 || idx >= len(i.Tabs) {
		return "", nil, false
	}
	tab := i.Tabs[idx]
	if !i.started {
		return tab.ID, nil, true
	}
	return tab.ID, tab.tmux, true
}

// tabTmuxByIDLocked resolves a tab's stable id (#1738) to the tmux session it
// currently backs, for callers coupling the resolved target to another
// lock-protected operation. Callers must hold i.mu.
func (i *Instance) tabTmuxByIDLocked(id string) (ts *tmux.TmuxSession, exists bool) {
	if id == "" {
		return nil, false
	}
	for idx, t := range i.Tabs {
		if t.ID != id {
			continue
		}
		if !i.started {
			return nil, true // the tab exists; it just has no live PTY yet
		}
		return i.tabTmuxAtLocked(idx), true
	}
	return nil, false
}

// TabTargetByID resolves a tab's stable id (#1738) DIRECTLY to what the web-tab
// proxy addresses it by — its kind, and the target URL a TabKindWeb tab stores —
// under a SINGLE lock acquisition. It is the tmux resolver's counterpart for the iframe
// plane, and exists for the same reason: id→ordinal followed by ordinal→tab takes
// i.mu TWICE, and a concurrent close between the two lands the second lookup on a
// DIFFERENT tab.
//
// A bounds check does not close that window, because the racing list is SHORTER,
// not out of range: with tabs [agent, A, B, C], resolving B yields ordinal 2, and
// a close of A before the second lookup leaves [agent, B, C] — where ordinal 2 is
// now C, in range and wrong. The proxy would then serve C's dev server under B's
// stable id, which is the exact misroute keying the route by id (#1810) exists to
// prevent.
//
// url is "" for every kind but TabKindWeb; a VSCODE tab deliberately stores none
// (its editor is resolved per request), so callers must not read absence of a URL
// as absence of a tab — that is what exists is for.
func (i *Instance) TabTargetByID(id string) (kind TabKind, url string, exists bool) {
	if id == "" {
		return 0, "", false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, t := range i.Tabs {
		if t.ID == id {
			return t.Kind, t.URL, true
		}
	}
	return 0, "", false
}

// TabIndexByID returns the CURRENT ordinal of the tab with stable id (#1738),
// and whether such a tab exists. It is the id→index resolution the stream
// endpoint runs per operation: a client addresses a tab by its stable id and the
// daemon maps it to wherever that tab now sits, so a reorder/close on another
// client can never make the client's captured position refer to a different tab.
// An empty id never matches (a legacy/absent id is not addressable by id).
//
// Prefer a single-lock primitive (TabTargetByID) where one exists
// for what the caller actually needs: an ordinal handed back to a SECOND lookup
// reopens the close/reorder window this resolution is meant to close.
func (i *Instance) TabIndexByID(id string) (int, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tabIndexByIDLocked(id)
}

// TabCount returns the number of tabs the instance currently holds.
func (i *Instance) TabCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.Tabs)
}

// TabTmuxName returns the sanitized tmux session name of the tab at idx, or
// "" when the instance is not started or the tab has no local session
// (remote tabs, out-of-range idx). The embedded terminal pane (#1089) uses it
// to attach its own render client to the tab's session; it never creates or
// mutates the session.
func (i *Instance) TabTmuxName(idx int) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started {
		return ""
	}
	ts := i.tabTmuxAtLocked(idx)
	if ts == nil {
		return ""
	}
	return ts.SanitizedName()
}

// AttachShellTab reconnects this local instance's in-memory tab list to a shell
// tab that already exists server-side — one the daemon's CreateTab RPC just
// spawned out-of-band (#960 PR 2). It is the no-spawn counterpart of
// AddShellTab: the daemon owns the spawn (so its authoritative view holds the
// tab and can't be clobbered), and the TUI only needs to reflect the new tab
// locally for instant display. It binds to the EXACT tmux session the daemon
// spawned and Restores (reconnects) it, mirroring restoreLocalTabs +
// LocalBackend.setupTabs, so the tab is immediately previewable/attachable
// without a second, colliding spawn.
//
// name and tmuxName are BOTH the daemon's, as returned by CreateTab. tmuxName is
// passed, not re-derived as "<agent>__<name>", because the two are independent
// namespaces (#1957, see tab_names.go): after a rename the daemon spawns
// "…__shell-2" for a tab named "shell", and re-deriving would bind this
// projection to the OLDER tab's still-live session. Empty falls back to the
// derivation — right for its only cause, a daemon predating the field.
//
// Local instances only — callers reject backends without TabManagement first. A
// tab with that name already present (a refresh raced ahead) makes this a no-op
// returning it. Errors when the instance is not started or has no session.
func (i *Instance) AttachShellTab(name, tmuxName, tabID string) (*Tab, error) {
	i.mu.Lock()
	if existing, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		i.mu.Unlock()
		return existing, err
	}
	started := i.started
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	title := i.Title
	i.mu.Unlock()
	if !started || agentTmux == nil || gw == nil {
		return nil, fmt.Errorf("cannot attach a tab to an instance that is not started")
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot attach a tab without a worktree")
	}

	// Bind to the exact session the daemon spawned and ATTACH-ONLY to it — never
	// spawn. Pass empty workDir so a session that is missing surfaces as an
	// error instead of re-spawning (#1152). The daemon is the single writer that
	// owns every tmux spawn (#960); this is a pure TUI-side projection of a tab
	// the daemon already created. If the daemon killed the instance in the window
	// since our RLock, the session is gone, and re-spawning it here would create a
	// tmux session that escapes the daemon's Kill teardown and orphans over the
	// about-to-be-deleted worktree — the same #990 leak AddShellTab guards. Fail
	// cleanly and let the daemon's next Snapshot reconcile the tab away.
	if tmuxName == "" {
		tmuxName = agentTmux.SanitizedName() + tmuxTabSeparator + name
	}
	shellTmux := agentTmux.NewSiblingSession(tmuxName, defaultShell())
	if err := shellTmux.Restore(""); err != nil {
		return nil, fmt.Errorf("failed to reconnect shell tab: %w", err)
	}

	tab := newShellTab(shellTmux)
	tab.Name = name
	// The daemon owns this tab's stable id (#1738). Empty is not a locally minted
	// substitute: it means an older daemon omitted the additive response field.
	tab.ID = tabID
	i.mu.Lock()
	if existing, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		i.mu.Unlock()
		if cerr := shellTmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("attach shell tab to %q: releasing raced attach client: %v", title, cerr)
		}
		return existing, err
	}
	// Re-check the teardown fence under the write lock before appending, mirroring
	// AddShellTab: Kill is not serialized against attach and can have flipped
	// started=false (snapshotting Tabs for teardown) in the window since our
	// RLock, and an archive teardown+move keeps started=true and raises OpArchiving
	// over that same window instead (#1195) — which the started-only recheck this
	// shipped with never saw, even though the caller gates on HasInFlightOp before
	// the daemon round-trip precisely because the op matters (#2100, the sibling of
	// the reconcile's missing recheck). Nothing was spawned above (attach-only), so
	// a lost race only needs to release the local attach client we opened and drop
	// the projection; the next reconcile re-adds the tab if it still exists
	// server-side.
	killed := !i.started || i.tabSpawnBlockedLocked() != nil
	if !killed {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	if killed {
		if cerr := shellTmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("attach shell tab to %q: releasing attach client after kill race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab attach")
	}
	return tab, nil
}
