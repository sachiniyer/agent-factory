package session

import (
	"fmt"
	"time"
)

// Two-axis session state (#1195 Phase 1b).
//
// The legacy Status enum jammed two orthogonal axes into one int: what the
// backing session actually IS (liveness — daemon-owned, persisted) and what
// operation a client is mid-executing (an in-flight op — transient, never
// persisted). Overloading a single field is what generated the whole
// SetStatusIfNotDeleting / isTransientStatus / overrideDeleting apparatus that
// reconstructs the split at read time, and the #1187 collision (Deleting meaning
// both "TUI kill" and "daemon archive fence").
//
// This file introduces the two axes as separate fields and keeps the legacy
// Status enum working as a thin DERIVATION SHIM: GetStatus composes the old value
// from (liveness, inFlightOp) and SetStatus decomposes a legacy write back onto
// them. Phase 1b is deliberately INERT — no behavior changes — so later PRs can
// migrate writers (1c: daemon poll → SetLiveness) and readers (1d: reconcile →
// inFlightOp) onto the axes incrementally, then delete the shim (1e).

// Liveness is the daemon-owned health axis: what state the backing tmux/worktree
// is actually in, independent of any client operation in flight. It is the
// persisted half of the old Status enum — exactly the values SaveInstances ever
// wrote to disk (transients are skipped) — now named on its own axis.
type Liveness int

const (
	// LivenessUnset is the zero value. It is never a live in-memory state: it
	// exists so an InstanceData decoded from a record written before #1195 (no
	// `liveness` key) lands here and FromInstanceData falls back to the legacy
	// `status` int. omitempty drops it on write.
	LivenessUnset Liveness = iota
	// LiveRunning: the agent is working.
	LiveRunning
	// LiveReady: idle, waiting for user input.
	LiveReady
	// LiveLost: the backing session vanished under a live record — recovery-
	// eligible (#1108/#1104).
	LiveLost
	// LiveDead: legacy observed-death. Write-never since #1108 (deaths record
	// Lost); FromInstanceData maps persisted Dead→Lost. Retained so the shim
	// round-trips and a later PR can retire it (1e).
	LiveDead
	// LiveArchived: deliberately shelved, worktree moved out, inert (#1028).
	LiveArchived
	// LiveLimitReached: the agent hit a usage-limit wall (#1146). Folded in here
	// from the start so the limit epic consumes a Liveness value rather than
	// appending to the flat Status enum.
	LiveLimitReached
)

// InFlightOp is the client/executor-owned axis: the operation a client (or the
// daemon executor) is mid-way through, overlaid on the liveness. It is carried
// in daemon Snapshots so read-only TUIs can cold-start into the exact
// archive/restore operation, but disk writers scrub it before persistence: a
// transient overlay must not survive a daemon restart.
type InFlightOp int

const (
	// OpNone: no client operation in flight.
	OpNone InFlightOp = iota
	// OpCreating: a create is in flight (was Loading).
	OpCreating
	// OpKilling: an optimistic kill is in flight (was Deleting).
	OpKilling
	// OpArchiving: an archive teardown+move is in flight (was Deleting used as
	// the archive fence) — a distinct value from OpKilling so the two owners no
	// longer collide (#1187). Populated only through the shim in Phase 1b; the
	// daemon archive executor sets it directly in 1c.
	OpArchiving
	// OpRestoring: an archive restore is in flight (replaces the RestoreFromArchive
	// "park it in Lost to trigger the re-spawn loop" hack). Wired in 1c.
	OpRestoring
	// OpReplacing: an agent handoff is between its outgoing and incoming runtime.
	// The status poll must not observe the close/start gap or settle a result from
	// the outgoing pane after the incoming pane has taken its place.
	OpReplacing
	// OpRespawning: a limit resume is re-spawning an EXISTING session's runtime —
	// for a remote backend, pushing the old sandbox's work and provisioning a fresh
	// one, which outlasts a poll interval (#2997). Its only job is to be an
	// in-flight op: refreshInstanceStatus skips a session that has one, and raising
	// it advances the state epoch so an observation the poll already decided is
	// dropped rather than applied.
	//
	// Deliberately NOT a client overlay, which is what separates it from
	// OpCreating. A respawn acts on a session that is already established, so
	// composeStatus lets it fall through to the settled liveness instead of masking
	// it with Loading. Masking it would be wrong twice over: SaveInstances drops
	// ordinary Loading rows, so a shutdown checkpoint mid-respawn would erase an
	// established session's only on-disk record and orphan its workspace; and the
	// TUI's reconcile clears adopted overlays per-op, so a create overlay it never
	// expected on an established row would strand it as Loading with its lifecycle
	// actions disabled until restart. There is nothing here for a client to mirror.
	OpRespawning
)

// LifecycleAction is the session domain's answer to which reversible lifecycle
// verb a visible row supports. The zero value means no lifecycle controls at all;
// a client must not infer one from liveness or from the fact that the row rendered.
// It is serialized into daemon projections so the TUI and web consume one decision
// instead of maintaining parallel state tables (#2234).
type LifecycleAction string

const (
	LifecycleActionNone    LifecycleAction = ""
	LifecycleActionArchive LifecycleAction = "archive"
	LifecycleActionRestore LifecycleAction = "restore"
)

// opIsTeardown reports whether op is a teardown already in flight — a kill or an
// archive. Both are documented mutation FENCES (see BeginKill/BeginArchive and
// IsTearingDown): while one is running, no competing lifecycle mutation may
// start. The two UI visibility gates below share this so neither can drift from
// the other or from the fence — the #2500 defect was exactly that, the gates
// excluding OpCreating/OpReplacing but not these, so a session mid-teardown still
// offered Kill/Archive/Restore and pressing one hit "kill already in progress".
func opIsTeardown(op InFlightOp) bool {
	return op == OpKilling || op == OpArchiving
}

// lifecycleActionFor is the one archive/restore policy shared by Instance (the
// TUI) and ToInstanceData (the web projection). A creating row has a provisional
// identity but no session to manage yet. An id-less row cannot address a mutation
// API unambiguously, so it also exposes nothing. A startup-unknown row must not
// reuse its unconfirmed runtime binding, while a replacing row is inside one
// transactional handoff and cannot admit a competing lifecycle mutation. A row
// already tearing down (OpKilling/OpArchiving) is inside the teardown fence, so
// it offers no verb either — the action it would show (Archive on a live row,
// Restore on the archived result) is precisely the mutation the fence exists to
// refuse. A kill tombstone likewise admits no competing archive/restore action:
// its only valid transition is finishing teardown. Resting rows restore; every
// A respawning row (#2997) is inside the limit-resume fence for the same reason a
// replacing one is: ArchiveSession would tear down and relocate a worktree the
// respawn is provisioning into, and RuntimeActionResumeLimit refuses while an op is
// in flight, so both verbs it could show would be refused on press — the #2500
// defect above. Resting rows restore; every
// other settled row archives. Kill addressability is intentionally independent
// (CanKill): a retained tombstone or startup-unknown row must remain removable
// without becoming attachable, archivable, or restorable.
func lifecycleActionFor(id string, liveness Liveness, op InFlightOp, startupStateUnknown, userKilled bool) LifecycleAction {
	if id == "" || op == OpCreating || op == OpReplacing || op == OpRespawning ||
		opIsTeardown(op) || startupStateUnknown || userKilled {
		return LifecycleActionNone
	}
	switch liveness {
	case LiveArchived, LiveLost, LiveDead:
		return LifecycleActionRestore
	default:
		return LifecycleActionArchive
	}
}

// canKillFor answers only whether a row has a stable teardown target. It does not
// imply that the runtime binding is safe to reuse: startup-unknown rows are the
// important counterexample. A creating row has no confirmed session to tear down;
// a replacing row already owns the runtime mutation fence; a row already tearing
// down (OpKilling/OpArchiving) has a kill/archive in flight, so a second one is
// the "kill already in progress" refusal the fence exists to prevent (#2500); and
// an id-less legacy row cannot address the destructive API without guessing.
//
// A respawning row (#2997) is excluded for a reason the transition table alone does
// NOT show, and reading only the table gets this backwards: tkBeginKill is
// allowedFrom-always ("a kill supersedes any in-flight op"), which looks like Kill
// works during any op. It does not work during THIS one, because the daemon must
// traverse a lock to reach that transition — KillSession waits opLockTimeout (30s,
// daemon/killbound.go) for the per-session operation lock and then returns
// errKillBusy WITHOUT applying BeginKill (daemon/manager_sessions.go), while the
// limit resume holds that same lock for the entire respawn (daemon/limit.go). So
// advertising Kill here promises a supersede that is really a 30-second wait
// followed by "try again". Hiding it is honest; making it genuinely interrupt the
// owning operation is a separate change to the kill path, and until then a hung
// remote provision is bounded by the backend's own deadlines rather than by a user
// gesture. The same is already true of OpCreating/OpReplacing/teardown above.
func canKillFor(id string, op InFlightOp) bool {
	return id != "" && op != OpCreating && op != OpReplacing && op != OpRespawning &&
		!opIsTeardown(op)
}

// LifecycleAction returns the shared lifecycle verb for this instance. TUI menus
// and handlers use this method; browser clients receive the same value from
// InstanceData.LifecycleAction.
func (i *Instance) LifecycleAction() LifecycleAction {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return lifecycleActionFor(i.ID, i.liveness, i.inFlightOp, i.startupStateUnknown, i.userKilled)
}

// CanKill reports whether interactive clients may offer explicit teardown for
// this instance. It is a separate domain axis from LifecycleAction so exceptional
// retained records do not become impossible to remove.
func (i *Instance) CanKill() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return canKillFor(i.ID, i.inFlightOp)
}

// composeStatus derives the legacy Status enum from the two-axis model. An
// in-flight op wins the composed value (it overlays the liveness), matching the
// old single-field semantics where Loading/Deleting masked the underlying state.
func composeStatus(lv Liveness, op InFlightOp) Status {
	if op == OpReplacing {
		// Loading is only the legacy client projection for replacement. Unlike a
		// create, a replacement can carry PendingHandoffMission, which is a durable
		// recovery obligation; persistence must inspect that marker rather than use
		// this lossy display value as a retention decision.
		return Loading
	}
	switch op {
	case OpCreating:
		return Loading
	case OpKilling, OpArchiving:
		return Deleting
	case OpRestoring:
		return Lost
	}
	switch lv {
	case LiveRunning:
		return Running
	case LiveReady:
		return Ready
	case LiveLost:
		return Lost
	case LiveDead:
		return Dead
	case LiveArchived:
		return Archived
	case LiveLimitReached:
		// No legacy Status equivalent; nothing composes it until the limit
		// reader lands (1c/1d). Present the closest settled state meanwhile.
		return Ready
	}
	// LivenessUnset and any unknown value: preserve the old zero-value Status
	// (Running) so a bare Instance{} literal reads as it did pre-split.
	return Running
}

// LivenessForStatus maps a settled (non-transient) legacy Status to its Liveness
// axis. Transient values (Loading/Deleting) are handled by setStatusLocked, which
// sets the op and leaves liveness untouched, so they never reach here.
func LivenessForStatus(s Status) Liveness {
	switch s {
	case Running:
		return LiveRunning
	case Ready:
		return LiveReady
	case Lost:
		return LiveLost
	case Dead:
		return LiveDead
	case Archived:
		return LiveArchived
	}
	return LiveReady
}

// opForStatus extracts the in-flight-op axis from a legacy Status: only the
// transient values carry one, settled values map to OpNone. FromInstanceData
// uses it so rebuilding an instance from a snapshot that caught a transient
// (Loading/Deleting) reconstructs the same composed Status (the shim must be a
// faithful round-trip in Phase 1b).
func opForStatus(s Status) InFlightOp {
	switch s {
	case Loading:
		return OpCreating
	case Deleting:
		return OpKilling
	}
	return OpNone
}

// inFlightOpFromData resolves the operation axis a persisted or snapshot record
// should take. New daemon snapshots carry the exact op; older payloads and disk
// records omit it and fall back to the legacy composed Status.
func inFlightOpFromData(data InstanceData) InFlightOp {
	if data.InFlightOp != OpNone {
		return data.InFlightOp
	}
	return opForStatus(data.Status)
}

// statusLocked composes the legacy Status under the caller-held mutex. It is the
// read half of the Phase 1b shim; every existing Status reader routes through it
// (GetStatus, ToInstanceData, the tab-spawn guards) unchanged.
func (i *Instance) statusLocked() Status {
	return composeStatus(i.liveness, i.inFlightOp)
}

// setStatusLocked decomposes a legacy Status write onto the two axes under the
// caller-held mutex. Transient values set the in-flight op and leave liveness
// untouched (they overlay it); settled values set liveness and clear the op —
// matching the old single-field clobber exactly.
func (i *Instance) setStatusLocked(s Status) {
	lv, op, resetAt := i.lifecycleStateLocked()
	switch s {
	case Loading:
		i.inFlightOp = OpCreating
	case Deleting:
		i.inFlightOp = OpKilling
	default:
		i.inFlightOp = OpNone
		i.liveness = LivenessForStatus(s)
	}
	i.noteStateChangeLocked(lv, op, resetAt)
}

// SetStatusForTest sets the status under the instance mutex by decomposing the
// legacy composed Status onto the two axes — TEST scaffolding only (#1195 Phase
// 2e), for establishing a precondition state via the familiar single-value API.
// Production code never writes lifecycle state through the legacy Status: every
// (liveness, inFlightOp) mutation goes through the Transition chokepoint. Mirrors
// the SetInFlightOpForTest / SetStartedForTest scaffolding pattern. (GetStatus
// stays — the composed value is still a legitimate read for rendering and test
// assertions, pending a separate retirement of the legacy Status enum.)
func (i *Instance) SetStatusForTest(status Status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setStatusLocked(status)
}

// GetStatus returns the current status under the Instance's mutex, so
// cross-goroutine readers don't race with SetStatus (legacy shim — composes the
// value from the two axes).
func (i *Instance) GetStatus() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.statusLocked()
}

// GetLiveness returns the daemon-owned liveness axis under the Instance's mutex
// (#1146/#1195). Readers use it where the composed Status is lossy: the snapshot
// reconcile mirrors liveness (not Status) so LiveLimitReached — which composes to
// Ready — propagates from the daemon to the read-only TUI.
func (i *Instance) GetLiveness() Liveness {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness
}

// SetLiveness is retired (#1195 Phase 2e): the liveness axis is now written only
// through the Transition chokepoint's ObserveLiveness edge (the daemon-truth
// edge — sets liveness, preserves the in-flight op, never rejects), so there is
// no direct liveness setter left to bypass it.

// GetInFlightOp returns the client/executor op axis under the instance mutex.
func (i *Instance) GetInFlightOp() InFlightOp {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp
}

// ShownArchived reports whether the row belongs in the sidebar's Archived
// section (#1028): it is archived on the liveness axis AND not mid-restore. An
// OpRestoring overlay re-homes the row into the live Instances section EAGERLY
// (#1210) — the visible feedback the archive epic owes restore — WITHOUT touching
// the liveness axis. Leaving liveness LiveArchived is load-bearing: the snapshot
// reconcile keys its Archived→live REBUILD (re-Start, restoring started + the
// agent-tmux binding, #1203) on seeing that exact transition, so an eager
// liveness flip here would make the reconcile see live→live and SKIP the rebuild,
// stranding the restored row "live but not started" — the #1203 regression. The
// rebuild replaces the row with a fresh started instance (OpNone), which clears
// the overlay; a restore FAILURE clears OpRestoring so the row drops back into
// the Archived section.
func (i *Instance) ShownArchived() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveArchived && i.inFlightOp != OpRestoring
}

// IsArchived reports whether the session is archived on the liveness axis, i.e.
// INERT: its tmux is gone and its worktree has been moved out to the archive dir,
// so nothing may be spawned in it, closed from it, or served out of it until a
// restore brings it back. It is the gate for interacting with an archived
// session's PRESERVED tabs (#1809 follow-up): archive now keeps web tabs, which
// made an archived session the first one to carry a non-agent tab — a tab the
// web-tab proxy would happily resolve and CloseTab would happily delete, neither
// of which had a reason to check for archived before.
//
// Unlike ShownArchived (a RENDER predicate, which yields the row to the live
// section the moment a restore starts, #1210) this reads the liveness axis ALONE.
// It is therefore SETTLED-state only: it does not cover a session mid-archive
// (OpArchiving, liveness still live) — see WebTabServeBlocked for the serve-side
// gate that does.
//
// It deliberately opens again the moment a restore begins. BeginRestore moves the
// session to LiveLost + OpRestoring, but both callers (RestoreArchived and
// undoCommittedArchive) move the worktree back home BEFORE that transition, so an
// OpRestoring session's worktree is already in place — there is no mid-move window
// to protect, and the tab it serves is the same one it will serve a moment later
// when the restore completes.
func (i *Instance) IsArchived() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveArchived
}

// WebTabServeBlocked is the serve-side analogue of TabSpawnBlocked: "may this
// session's preserved web tab be resolved and proxied right now?" It answers no
// for a settled archive AND for the teardown window that precedes one. The
// returned error is a REASON fragment, not a sentence: the proxy prefixes it with
// the session it could not serve, so the two read as one message.
//
// The in-flight ops matter here for the same reason they matter to a tab spawn.
// BeginArchive raises OpArchiving BEFORE tmux comes down and the worktree moves,
// and leaves liveness live until CommitArchive lands at the very end (#1195 Phase
// 2d). A gate reading only the settled LiveArchived would keep proxying a
// preserved loopback URL throughout that teardown — an iframe that was open when
// the user hit archive would go on reaching a port on the daemon's machine while
// the session it belongs to is being dismantled. Terminal streams already fence
// this window via killsInFlight; the proxy route is not serialized with
// ArchiveSession at all, so it needs the fence on the instance itself.
//
// OpKilling rides along for the same reason: a session being removed must not
// serve. OpRestoring deliberately does NOT — see IsArchived.
func (i *Instance) WebTabServeBlocked() error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness == LiveArchived {
		return fmt.Errorf("it is archived and inert until restored (af sessions restore)")
	}
	if i.inFlightOp == OpArchiving || i.inFlightOp == OpKilling {
		return fmt.Errorf("it is being archived or removed")
	}
	return nil
}

// IsCreating reports whether a create is in flight (the render/gate replacement
// for the old GetStatus()==Loading check).
func (i *Instance) IsCreating() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp == OpCreating
}

// IsTearingDown reports whether a kill or archive teardown is in flight (the
// render/gate replacement for the old GetStatus()==Deleting check — both owners
// now live on distinct ops, but both read as "going away").
func (i *Instance) IsTearingDown() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp == OpKilling || i.inFlightOp == OpArchiving
}

// HasInFlightOp reports whether any client op is in flight (the render/gate
// replacement for the old Loading||Deleting "is this row transient" check).
func (i *Instance) HasInFlightOp() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.inFlightOp != OpNone
}

// SetInFlightOpForTest writes the op axis directly, for TEST scaffolding only —
// establishing a precondition state rather than exercising a transition (#1195
// Phase 2e). Production code never sets the op axis directly: every op write goes
// through the Transition chokepoint (BeginCreate/BeginKill/BeginArchive/
// BeginRestore/MarkRestoring raise an op; ConfirmLive/RevertKill/CommitArchive/
// AbortArchive/AbortRestore/ClearOp clear it). Mirrors the SetStartedForTest /
// SetGitWorktreeForTest scaffolding pattern.
func (i *Instance) SetInFlightOpForTest(op InFlightOp) {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, prevOp, resetAt := i.lifecycleStateLocked()
	i.inFlightOp = op
	i.noteStateChangeLocked(lv, prevOp, resetAt)
}

// MarkLive is retired (#1195 Phase 2e): marking a completed create/recover live
// is now the Transition chokepoint's ConfirmLive edge (Running + clear the
// completing create/restore op, while yielding to an in-flight kill/archive
// teardown). No direct "mark live" setter remains to bypass it.

// tabSpawnBlockedLocked reports the error, if any, forbidding a new tab spawn.
// Caller holds i.mu. It reads the two axes directly (the #1195 structural fold of
// the #1196 archive-orphan guard): a tab may not spawn into an archived session
// (its worktree was moved away) or one with a teardown op in flight — an archive
// (OpArchiving) or kill (OpKilling). The archive case is the load-bearing one:
// ArchiveTeardown keeps started=true, so the #990 started-flag guard never fires
// during archive; OpArchiving is the fence that started=true cannot provide.
func (i *Instance) tabSpawnBlockedLocked() error {
	if i.liveness == LiveArchived {
		return fmt.Errorf("cannot add a tab to an archived session; restore it first (af sessions restore)")
	}
	if i.inFlightOp == OpArchiving || i.inFlightOp == OpKilling {
		return fmt.Errorf("cannot add a tab to a session that is being archived or removed; try again in a moment")
	}
	return nil
}

// TabSpawnBlocked is the locking form of tabSpawnBlockedLocked, for callers that
// don't already hold i.mu (the daemon's archive-exclusive tab lock).
func (i *Instance) TabSpawnBlocked() error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tabSpawnBlockedLocked()
}

// SetLimitReached marks the instance blocked on a usage-limit wall (#1146): it
// sets the LiveLimitReached liveness and stores the parsed reset time (zero when
// the banner carried none) for the sidebar badge and PR3's auto-resume
// scheduler. There is no legacy Status value for SetStatus to decompose onto, so
// the daemon single-writer (#960) sets the liveness axis directly here. Skips a
// row with any operation in flight so it never clobbers that operation's fence.
func (i *Instance) SetLimitReached(resetAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setLimitReachedLocked(resetAt)
}

// SetLimitReachedAtEpoch is SetLimitReached for a decision derived from an
// OBSERVATION — the daemon poll's usage-limit detection over captured pane
// content (#2135). It applies the block only while the instance's state epoch is
// still the one the observation was captured at; if a newer authoritative
// transition has landed since (a resume's ClearLimitReached above all, but equally
// a kill or an archive) the decision is known-stale and is dropped. Reports
// whether it applied.
//
// The check and the write are one critical section under i.mu, which is the whole
// point: an epoch read followed by a separate SetLimitReached would leave the same
// window it closes.
func (i *Instance) SetLimitReachedAtEpoch(resetAt time.Time, epoch uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != epoch {
		return false
	}
	return i.setLimitReachedLocked(resetAt)
}

// setLimitReachedLocked is the shared body: mark the instance limit-blocked and
// record its reset time, skipping a row with any operation in flight so it never
// clobbers that operation's fence. Reports whether it applied. Caller holds i.mu.
func (i *Instance) setLimitReachedLocked(resetAt time.Time) bool {
	if i.inFlightOp != OpNone {
		return false
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.liveness = LiveLimitReached
	i.limitResetAt = resetAt
	i.noteStateChangeLocked(lv, op, prevReset)
	return true
}

// SetLimitResetAt records only the display-only reset time (#1146), leaving both
// axes untouched. The read-only TUI reconcile uses it to mirror the daemon's
// parsed reset time after it has already applied LiveLimitReached on the liveness
// axis (Phase 1d applies liveness UNCONDITIONALLY via SetLiveness): the reset
// time rides the liveness as pure display metadata, so it is set on its own
// rather than through SetLimitReached, which would re-drive the liveness axis and
// carry SetLimitReached's transient-op guard into a path that must be
// unconditional.
func (i *Instance) SetLimitResetAt(resetAt time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, prevReset := i.lifecycleStateLocked()
	i.limitResetAt = resetAt
	i.noteStateChangeLocked(lv, op, prevReset)
}

// ClearLimitReached moves a limit-blocked instance back to LiveRunning so the
// daemon poll re-resolves its real state on the next tick and the [limit] badge
// clears (#1146). A no-op when the instance is not limit-blocked, so the resume
// action (and PR3's scheduler) can call it unconditionally.
// ReparkLimitUnderResumeFence restores a limit window while the caller holds the
// resume fence. It is the transaction-owned twin of SetLimitReached, in the same
// shape as RecordHandoffSwap is to SwapAgentProgram: the plain setter refuses while
// ANY op is in flight, which is right for every other writer and wrong for the one
// that owns the operation.
//
// The resume needs it because it re-parks BEFORE delivering its prompt (#2997). With
// the fence held across that stretch — which is what keeps the poll from settling the
// fresh runtime Ready and ending the task run — the plain setter would no-op and this
// episode's reset time would be lost, leaving the auto-resume scheduler nothing to
// schedule off. Silently, too: it reports the refusal only through a bool that path
// had no reason to read.
//
// Refuses unless OpRespawning is actually held, so it cannot become a back door
// around the guard it deliberately steps past.
func (i *Instance) ReparkLimitUnderResumeFence(resetAt time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("re-parking the limit for %q requires the resume fence (in-flight op is %s)",
			i.Title, opLabel(i.inFlightOp))
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.liveness = LiveLimitReached
	i.limitResetAt = resetAt
	i.noteStateChangeLocked(lv, op, prevReset)
	return nil
}

func (i *Instance) ClearLimitReached() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.liveness != LiveLimitReached {
		return
	}
	lv, op, prevReset := i.lifecycleStateLocked()
	i.liveness = LiveRunning
	i.limitResetAt = time.Time{}
	// The epoch bump here is what a racing poll checks: it is the resume's
	// completion point, so any limit re-detection made from content captured before
	// it is stale by definition and must not land (#2135).
	i.noteStateChangeLocked(lv, op, prevReset)
}

// LimitReached reports whether the instance is blocked on a usage limit (#1146).
// Every render/serialize site keys its [limit] badge off this rather than the
// composed Status, which has no limit value (LiveLimitReached composes to Ready).
func (i *Instance) LimitReached() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveLimitReached
}

// LimitResetAt returns the parsed usage-limit reset time and whether one is
// known (#1146). It reports (zero, false) when the session is not limit-blocked
// or the banner carried no parseable reset time, so a stale reset value can
// never leak onto a recovered session.
func (i *Instance) LimitResetAt() (time.Time, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.liveness != LiveLimitReached || i.limitResetAt.IsZero() {
		return time.Time{}, false
	}
	return i.limitResetAt, true
}

// livenessFromData resolves the liveness a persisted or snapshot record should
// take, applying the same rollforward FromInstanceData uses: prefer the
// `liveness` field, fall back to the legacy `status` int for pre-#1195 records,
// and map a persisted Dead → Lost (recovery-eligible, #1108). Shared so the
// snapshot reconcile mirrors liveness identically to a cold-start restore.
func livenessFromData(data InstanceData) Liveness {
	lv := data.Liveness
	if lv == LivenessUnset {
		lv = LivenessForStatus(data.Status)
	}
	if lv == LiveDead {
		lv = LiveLost
	}
	return lv
}

// IsArchivedData reports whether a serialized session record is archived,
// resolving its effective liveness with the same rollforward livenessFromData
// applies (so a pre-#1195 record with only a legacy status still classifies
// correctly). It is the []InstanceData analogue of Instance.ShownArchived for
// callers that iterate the daemon Snapshot rather than live instances — e.g. the
// "active projects" derivation, which counts only non-archived sessions so a
// project whose sessions are all archived drops out of the active list (#1735).
func IsArchivedData(data InstanceData) bool {
	return livenessFromData(data) == LiveArchived
}

// EffectiveLiveness resolves the liveness a serialized record actually has,
// applying the same rollforward IsArchivedData relies on: prefer the `liveness`
// field, fall back to the legacy `status` int for records written before #1195.
//
// Exported because reading data.Liveness directly is a trap for any caller that
// iterates records rather than live instances — a pre-#1195 record carries the
// ZERO liveness (LivenessUnset) while its real state lives in the legacy status,
// so a direct comparison silently misclassifies it as "not archived", "not lost",
// and so on. IsArchivedData answers one question; this answers the general one
// for callers that must distinguish several states (#2983's quota report needs
// "is an agent actually running", which is four states wide).
func EffectiveLiveness(data InstanceData) Liveness {
	return livenessFromData(data)
}
