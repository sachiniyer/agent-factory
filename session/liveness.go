package session

import (
	"fmt"
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
// This file holds the two axes as separate fields and keeps the legacy Status
// enum readable through a thin DERIVATION SHIM: GetStatus composes the old value
// from (liveness, inFlightOp). Writers are migrated off the legacy enum —
// SetStatusForTest is the only legacy-shaped writer left; production code
// writes the axes directly or through the Transition chokepoint. The composed
// read and the legacy Status enum itself remain pending a separate retirement
// (1e).

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
	// round-trips; it goes away with the legacy Status enum's retirement (1e,
	// still open).
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
// already mutating (including OpRestoring) is inside its operation fence, so
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
//
// An unreachable row (#5571 W3) also offers nothing: Archive would try to push
// a worktree the daemon cannot reach, and Restore's live recheck would refuse
// — both verbs fail on press exactly like the #2500 fence above, so the flag
// hides them for the outage's duration while Kill stays available.
func lifecycleActionFor(id, title string, liveness Liveness, op InFlightOp, startupStateUnknown, userKilled, pendingAccountSwap, unreachable, pruned bool) LifecycleAction {
	// A pruned row's worktree is gone by design: restore would refuse it by naming
	// the kept branch, so projecting the verb offers a control that can never
	// succeed (#5189).
	if pruned {
		return LifecycleActionNone
	}
	if id == "" || op == OpCreating || op == OpReplacing || op == OpRespawning || op == OpRestoring ||
		opIsTeardown(op) || startupStateUnknown || userKilled || pendingAccountSwap || unreachable {
		return LifecycleActionNone
	}
	// The daemon refuses archive and restore of the reserved root session
	// unconditionally (daemon/archive.go, daemon/restore.go), so projecting either
	// verb offers a control that can never succeed (#5406). Same shared-predicate
	// treatment as the handoff refusal (#2436).
	if IsReservedTitle(title) {
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
//
// A restoring row is excluded for the symmetrical reason and a sharper one: the
// daemon holds that same per-session operation lock for the whole restore, and
// claimRestoreOperation registers killsInFlight the moment it has that lock
// (daemon/restore.go), so a Kill pressed during it is rejected at the ADMISSION
// gate — before BeginKill is ever reached — with "kill already in progress for
// session X"
// (daemon/manager_sessions.go). That message does not match the user's mental
// model: they started a restore, not a kill, yet the error implies a prior kill.
// Kill never gets a chance to supersede the op, so advertising it promises an
// action that fails immediately and confuses. Hiding it mirrors the OpRespawning
// exclusion above and the TUI guard the Restore verb already has for this state
// (handleRestore shows an "already being restored" notice rather than
// dispatching), closing the asymmetry where Restore was guarded and Kill fell
// through to the daemon's misleading refusal.
func canKillFor(id string, op InFlightOp) bool {
	return id != "" && op != OpCreating && op != OpReplacing && op != OpRespawning &&
		op != OpRestoring && !opIsTeardown(op)
}

// LifecycleAction returns the shared lifecycle verb for this instance. TUI menus
// and handlers use this method; browser clients receive the same value from
// InstanceData.LifecycleAction.
func (i *Instance) LifecycleAction() LifecycleAction {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return lifecycleActionFor(i.ID, i.Title, i.liveness, i.inFlightOp, i.startupStateUnknown, i.userKilled, i.pendingAccountSwap != nil, i.unreachableDataLocked() != nil, !i.prune.prunedAt.IsZero())
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
		i.assignLivenessLocked(LivenessForStatus(s))
	}
	i.noteStateChangeLocked(lv, op, resetAt)
}

// assignLivenessLocked is the ONE write path for i.liveness outside the
// transition table itself, which routes here too. While a rebuild teardown
// obligation is owed, every reviving write coerces to LiveLost: a positive
// answer can be coming from a pane still bound to the DELETED inode — exactly
// the pane the teardown has not yet proven gone — and leaving Lost is what
// keeps the owning loop retrying (#5371 review). LiveLost passes through
// trivially; LiveArchived also passes through, because archive is the row
// leaving service under its own stop-everything teardown, not an observation
// that could be lying about the stale pane.
// Callers hold i.mu.
func (i *Instance) assignLivenessLocked(l Liveness) {
	if i.rebuildTeardownPending && l != LiveLost && l != LiveArchived {
		l = LiveLost
	}
	i.liveness = l
}

// SetStatusForTest sets the status under the instance mutex by decomposing the
// legacy composed Status onto the two axes — TEST scaffolding only (#1195 Phase
// 2e), for establishing a precondition state via the familiar single-value API.
// Production code never writes lifecycle state through the legacy Status. Mirrors
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

// IsResting reports whether the row's liveness is one of the inert,
// restorable states — archived, lost, or dead. It is a liveness-only read:
// unlike LifecycleAction it does NOT consult the op/startup fences, so a
// fenced resting row (startup-unknown, restoring in flight) still reports
// true. Callers that need the "which verb is offered" answer use
// LifecycleAction; callers that need "owns no live runtime surface" use this
// (#4755 review).
func (i *Instance) IsResting() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.liveness == LiveArchived || i.liveness == LiveLost || i.liveness == LiveDead
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

// teardownExpected reports whether af's own state already accounts for this
// instance's tmux session going away: the kill tombstone, or an in-flight
// kill/archive op. The status monitor consults it — through the
// ExpectingTeardown poll variants — when a poll finds the session gone, so a
// disappearance inside the window between af recording the teardown and
// close() landing the generation mark classifies at INFO rather than ERROR
// (#5138). A vanish with neither set is genuinely unexpected and stays ERROR.
//
// A cleared started flag alone is deliberately NOT evidence: teardownTabs
// only clears it inside the kill/archive fences already listed here, while
// MarkStartupStateUnknown and the restore-failure CloseAttachOnly path clear
// it while deliberately retaining the runtime — issuing no kill-session — so
// a disappearance there is exactly the unexpected loss ERROR exists for.
// An owed on_complete marker is excluded for the same reason: it is filed
// before teardown begins and can outlive it through adoption, so it says
// nothing about whether af is tearing the session down RIGHT NOW — the
// discharge itself runs through KillSession/ArchiveSession, which raises
// the op fence and tombstone this predicate does read.
//
// It is also deliberately narrower than HasInFlightOp: OpCreating is af
// BUILDING the session, not tearing it down — a death there is the anomaly
// ERROR exists for. The runtime-replacement ops (OpRestoring/OpReplacing/
// OpRespawning) are left out on the same ground; their own close() calls
// land the generation mark, which is the attribution meant for them.
func (i *Instance) teardownExpected() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.userKilled || opIsTeardown(i.inFlightOp)
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

// tabSpawnBlockedLocked reports the error, if any, forbidding a new tab spawn.
// Caller holds i.mu. It reads the two axes directly (the #1195 structural fold of
// the #1196 archive-orphan guard): a tab may not spawn into an archived session
// (its worktree was moved away), one with a teardown op in flight — an archive
// (OpArchiving) or kill (OpKilling) — or one whose durable account replacement
// has not completed. The archive case is the load-bearing one:
// ArchiveTeardownWithClaim keeps started=true, so the #990 started-flag guard never fires
// during archive; OpArchiving is the fence that started=true cannot provide.
func (i *Instance) tabSpawnBlockedLocked() error {
	if i.liveness == LiveArchived {
		return fmt.Errorf("cannot add a tab to an archived session; restore it first (af sessions restore)")
	}
	if i.inFlightOp == OpArchiving || i.inFlightOp == OpKilling {
		return fmt.Errorf("cannot add a tab to a session that is being archived or removed; try again in a moment")
	}
	if i.accountSwapLaunch != nil || i.pendingAccountSwap != nil {
		if pending := i.pendingAccountSwap; pending != nil {
			return fmt.Errorf("cannot add a tab while session %q has %s in progress; retry it with %s or wait for it to settle",
				i.Title, accountSwapSpan(pending.From, pending.To), accountSwapRetryCommand(i.Title, pending.To))
		}
		return fmt.Errorf("cannot add a tab while session %q has an account swap in progress", i.Title)
	}
	if i.rebuildTeardownPending {
		// A spawned tab binds a NEW session inside the worktree while the
		// committed teardown still owes the old one: it snapshots names, not
		// vintages, and would kill the fresh tab together with the genuinely
		// stale sessions (#5371 review).
		return fmt.Errorf("cannot add a tab to session %q while a worktree-rebuild teardown is pending; let its recovery finish", i.Title)
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

// livenessFromData resolves the liveness a persisted or snapshot record should
// take, applying the same rollforward FromInstanceData uses: prefer the
// `liveness` field, fall back to the legacy `status` int for pre-#1195 records,
// and map a persisted Dead → Lost (recovery-eligible, #1108). Shared so the
// snapshot reconcile mirrors liveness identically to a cold-start restore.
func livenessFromData(data InstanceData) Liveness {
	lv := RecordedLiveness(data)
	if lv == LivenessUnset {
		// A record that records no liveness at all — a pre-#1195 snapshot caught
		// mid-operation, or a status integer from a newer af — still has to load
		// as SOME state, so this keeps LivenessForStatus's historical default
		// rather than leaving an Instance in the zero value. RecordedLiveness
		// refuses that guess because naming and filtering can say "unknown"; a
		// loader cannot.
		lv = LiveReady
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
// field, fall back to the legacy `status` int for records written before #1195,
// and rewrite a persisted Dead to Lost. That last step is what separates it from
// RecordedLiveness (session/state_names.go), which reports the record's own
// value and is therefore what `liveness_name` and the `--status` filter read: a
// caller asking "what state should this session be treated as" wants Dead folded
// into recovery-eligible Lost, while a caller NAMING the `liveness` integer must
// not rename the value sitting beside it.
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
