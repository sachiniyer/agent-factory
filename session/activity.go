package session

// Activity is the derived answer to "is this session still busy?" — the single
// question both `af sessions watch` and the watch-task concurrency limit (#1892)
// ask of a session record. It is a projection of the two-axis state (#1195), not
// a fourth stored axis: nothing persists it, ClassifyActivity computes it.
//
// It lives here, in the leaf session package, because both consumers need it and
// daemon/ cannot import api/ (api/ imports daemon/). Two copies of this state
// machine would drift — and the two callers disagreeing about whether a session
// is busy is exactly the class of bug #1892 reports from userland, where a
// monitor inferred busyness from titles and liveness and overshot its own cap.
type Activity int

const (
	// ActivityPending: the session is still settling or working — an operation is
	// in flight (create/kill/archive/restore), the agent is running, or it is
	// parked on a usage limit the daemon auto-resumes (#1146). It holds a
	// concurrency slot and `sessions watch` keeps polling.
	ActivityPending Activity = iota
	// ActivityIdle: the agent went idle and awaits input — done working, ready for
	// review. Releases a concurrency slot; `sessions watch` exits 0.
	ActivityIdle
	// ActivityTerminal: the session reached a state it cannot leave ON ITS OWN
	// (lost/dead/archived) — it needs a restore, a kill, or the daemon's restore
	// loop. `sessions watch` exits non-zero with the reason.
	//
	// "Cannot leave on its own" is not the same as "gone for good", and consumers
	// must not read it that way. A LiveLost session in particular is one the
	// daemon's restore loop may be actively reviving, so the watch-task
	// concurrency limit (#1892) keeps counting it — see
	// daemon.canAutoRestoreLostSession, which composes this verdict with that
	// question rather than changing it here.
	ActivityTerminal
)

// ClassifyActivity maps a session record onto the activity projection, returning
// the outcome and a human clause explaining a terminal (or idle) result.
//
// It reads the canonical two-axis state (#1195): an in-flight client/executor
// operation means the session is still settling, so it wins over the liveness
// axis — this is what makes a brand-new session count as busy from the moment its
// create begins, before any liveness exists and while its asynchronous
// post-worktree hooks still run (#1892).
//
// VERSION SKEW IS PART OF THE CONTRACT (#3450). The record may come from a remote
// daemon NEWER than this binary, so every axis can hold a value this build has no
// constant for, and an unknown value is not a definite state. Each axis therefore
// fails closed onto ActivityPending rather than guessing: any non-zero InFlightOp
// counts as in flight, an unrecognized Liveness falls to the pending return at the
// end, and classifyActivityByStatus defaults there too. Pending is the only safe
// landing spot, because it is the one outcome that COMPLETES nothing — `af sessions
// watch` keeps polling (bounded by its --timeout) and the #1892 cap keeps holding
// the slot. Idle is the outcome that must never be reached by fallthrough: it exits
// watch 0 and tells automation a mid-operation session is ready for review.
//
// The LivenessUnset branch falls back to the composed legacy Status for records
// that predate the liveness field. That fallback is load-bearing, not vestigial:
// LivenessForStatus maps the transient Loading/Deleting to LiveReady, so
// resolving a legacy record through the liveness axis alone would report a
// mid-create session as idle — releasing a concurrency slot it should hold, and
// telling `sessions watch` a session is ready before it ever started.
func ClassifyActivity(data InstanceData) (Activity, string) {
	// Storage may carry StartupStateUnknown solely as a rollback fence for an
	// ambiguous handoff. Current readers understand its mission-scoped evidence
	// and must classify the real state; an older binary ignores the additive
	// original field and deliberately remains inert.
	data = data.RestoreHandoffRollbackFence()
	data = data.RestoreAccountSwapRollbackFence()
	data = data.restoreMissingHandoffMissionEvidence()
	data = data.restoreMissingAccountSwapMissionEvidence()
	// A committed kill is terminal even while its teardown or an older operation
	// marker remains visible. UserKilled means finish-this-kill, never resume work;
	// treating a stale pending mission/op as active would keep watch and task slots
	// alive for a run that can no longer continue.
	if data.UserKilled {
		return ActivityTerminal, "session was killed and its teardown is pending"
	}
	// A pending account swap is a durable replacement transaction, including its
	// preflight/replacement and mission-delivery phases. It must hold the activity
	// slot until recovery settles it. Storage projects StartupStateUnknown solely
	// to fence an older binary; RestoreAccountSwapRollbackFence above removes that
	// compatibility value before this current-reader decision.
	if data.PendingAccountSwap != nil {
		return ActivityPending, ""
	}
	// A failed create whose runtime identity could not be confirmed is a settled
	// blocked outcome, not an idle LiveReady session. It wins even over a stale
	// OpCreating persisted by the failure path: no automatic operation may settle
	// this row, and watch must exit non-zero with an actionable explanation.
	if data.StartupStateUnknown {
		return ActivityTerminal, "session startup state is unknown (af could not confirm which runtime owns its workspace); inspect it and explicitly remove it before retrying"
	}
	// A post-swap runtime with an undelivered takeover mission is still settling,
	// even though instances.json intentionally scrubs the generic OpReplacing
	// value. Treat the durable marker as its specific fence so a raw-record reader
	// cannot release the task slot in the crash window before FromInstanceData has
	// reconstructed the op axis.
	if data.PendingHandoffMission != "" {
		return ActivityPending, ""
	}
	// ANY operation in flight means the session is mid-transition; wait for it to
	// settle rather than reporting the transient composed status.
	//
	// Deliberately a non-zero test and NOT a switch over the ops this build knows
	// (#3450). The set of operation values is not closed: a newer remote daemon can
	// send one this binary has no constant for, JSON decoding accepts it, and
	// inFlightOpFromData carries it through verbatim. An enumerated switch lets that
	// value fall past the op axis onto a liveness field that is stale precisely
	// BECAUSE an operation is running — and the stale value is usually LiveReady, so
	// the answer comes back `idle`: the one verdict that makes `af sessions watch`
	// exit 0 and releases a #1892 concurrency slot. Automation then prompts a session
	// mid-create, and a task admits a run over its cap.
	//
	// Unknown must therefore resolve to pending rather than idle, and pending is the
	// safe direction on both consumers: watch keeps polling until the op clears (its
	// --timeout is the backstop) and the cap keeps holding the slot. Neither COMPLETES
	// on a state this build cannot read, which is the #504 version-skew rule.
	//
	// Do not "tidy" this back into a switch with a default arm. There is nothing to
	// enumerate: naming the unknown value is exactly what this build cannot do, which
	// is why opLabel renders it as InFlightOp(%d) instead of guessing, and why
	// classifyWatchStop in api/sessions_watch_fleet.go tests the same way. Both other
	// axes here already fail safe — an unrecognized liveness falls to the pending
	// return at the end of this function, and classifyActivityByStatus defaults to
	// pending too — so an enumerated op switch was the only axis that failed open.
	if data.InFlightOp != OpNone {
		return ActivityPending, ""
	}

	switch data.Liveness {
	case LiveReady:
		return ActivityIdle, "idle (ready for review)"
	case LiveRunning:
		return ActivityPending, ""
	case LiveLimitReached:
		// Blocked on a provider usage limit; the daemon auto-resumes it (#1146),
		// so treat it as still-working rather than done.
		return ActivityPending, ""
	case LiveLost:
		return ActivityTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case LiveDead:
		return ActivityTerminal, "session is dead (its backing tmux/worktree vanished)"
	case LiveArchived:
		return ActivityTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	case LivenessUnset:
		// Pre-#1195 record with no liveness axis: derive from the legacy Status.
		return classifyActivityByStatus(data.Status)
	}
	return ActivityPending, ""
}

// LifecycleView is a CONSISTENT snapshot of one session's lifecycle state, taken
// under a single instance lock. It exists because a predicate that reads a live
// Instance more than once is not a predicate — it is a race.
//
// The daemon's Lost-restore loop mutates a session WITHOUT holding the manager
// lock (restoreLostSession releases m.mu before calling Recover, which ends in
// Transition(ConfirmLive) → LiveRunning). So a caller that asked "is it busy?" and
// then "is it a restorable lost run?" through two separate accessors could have
// the restore land between them: the first read sees LiveLost (not busy), the
// second sees LiveRunning (not Lost), and the session falls through BOTH arms —
// counted by neither, which silently undercounts the watch-task concurrency cap
// and admits a run over the limit (#1892). More checks cannot fix that; only one
// snapshot can.
//
// It is deliberately narrow rather than reusing ToInstanceData, which walks an
// instance's tabs, worktree, and PR state: the cap classifies every session in a
// repo while holding the manager lock, the same reason Snapshot keeps its
// serialization outside that lock.
type LifecycleView struct {
	// Title and TaskID are immutable after construction; carried so a caller can
	// judge a session entirely from the view.
	Title  string
	TaskID string
	// Liveness and InFlightOp are the two canonical axes (#1195); Status is their
	// composed legacy value, resolved under the same lock so a caller reading the
	// composed form cannot disagree with one reading the axes.
	Liveness   Liveness
	InFlightOp InFlightOp
	Status     Status
	// StateEpoch is captured with the lifecycle axes. Any transition derived from
	// this snapshot must be scoped to it so newer lifecycle truth wins.
	StateEpoch uint64
	Started    bool
	UserKilled bool
	// PendingAccountSwap is a committed identity move whose replacement notice
	// and task still have to land. Only the limit-resume action may consume it;
	// competing archive/handoff/restore actions must leave that obligation intact.
	PendingAccountSwap bool
	// PendingHandoffMission is an agent handoff whose takeover brief has not been
	// confirmed delivered. The swap itself is complete — its fence settles on the
	// incoming runtime's liveness (#4429) — so this obligation, not the op axis,
	// is what keeps a SECOND handoff from overwriting the first one's mission.
	PendingHandoffMission bool
	// StartupStateUnknown is the retained-create fence: the launch may have
	// succeeded under an identity af could not confirm, so no runtime or workspace
	// action may infer ordinary LiveReady semantics from this view.
	StartupStateUnknown bool
	// TaskRunActive is whether this session's task run is still in flight — the one
	// fact the concurrency cap counts. See Instance.taskRunActive.
	TaskRunActive bool
	// Recoverable is the backend's Recover capability: whether a lost session can
	// be revived in place at all.
	Recoverable bool
	// LostRestoreGaveUp is the durable terminal gate for automatic recovery. An
	// explicit restore remains legal and clears this when its replacement crosses
	// the live boundary.
	LostRestoreGaveUp bool
}

// LifecycleView snapshots the session's lifecycle state under ONE lock. Every
// field a caller needs to reach a verdict must come from here rather than from a
// follow-up accessor call, or the verdict spans a window the restore loop can
// move through.
func (i *Instance) LifecycleView() LifecycleView {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lifecycleViewLocked()
}

// lifecycleViewLocked is LifecycleView's already-locked half. Callers must hold
// i.mu for reading or writing. Runtime-action chokepoints use it while making a
// state mutation under the same critical section, so validation and mutation
// cannot observe different lifecycle states.
func (i *Instance) lifecycleViewLocked() LifecycleView {
	return LifecycleView{
		Title:                 i.Title,
		TaskID:                i.TaskID,
		Liveness:              i.liveness,
		InFlightOp:            i.inFlightOp,
		Status:                i.statusLocked(),
		StateEpoch:            i.stateEpoch,
		Started:               i.started,
		UserKilled:            i.userKilled,
		PendingAccountSwap:    i.pendingAccountSwap != nil,
		PendingHandoffMission: i.pendingHandoffMission != "",
		StartupStateUnknown:   i.startupStateUnknown,
		// The already-locked variant, NOT Capabilities(): the backend is mutable
		// (a restore rebinds it in bindProvisionResult), so Capabilities() now
		// takes i.mu.RLock itself — and calling it while this snapshot holds the
		// same non-reentrant lock would deadlock against a queued restore writer
		// (#2096). Resolving it here also keeps the capability in the SAME critical
		// section as the liveness axes, so the two can never disagree.
		Recoverable:       i.capabilitiesLocked().Recover,
		LostRestoreGaveUp: i.lostRestoreFailure.valid(),
		TaskRunActive:     i.taskRunActive,
	}
}

// Activity classifies a snapshot through the same state machine ClassifyActivity
// runs, so a live instance and its persisted record can never disagree about
// whether a session is busy.
//
// The legacy Status axis is not consulted: a live in-memory instance always has a
// resolved liveness (NewInstance sets it, FromInstanceData rolls a legacy record
// forward at load), so ClassifyActivity's LivenessUnset fallback never applies.
func (v LifecycleView) Activity() Activity {
	data := InstanceData{
		Liveness:            v.Liveness,
		InFlightOp:          v.InFlightOp,
		UserKilled:          v.UserKilled,
		StartupStateUnknown: v.StartupStateUnknown,
	}
	if v.PendingAccountSwap {
		data.PendingAccountSwap = &AccountSwapData{}
	}
	activity, _ := ClassifyActivity(data)
	return activity
}

// LoadedActivity reports the activity the loaded form of a raw InstanceData would
// have, mirroring LifecycleView.Activity() composed with FromInstanceData's
// InFlightOp reconstruction. It is the raw-record reader's equivalent of the
// live arm's v.Activity() — the activity holdsTaskRunSlot sees for a materialized
// Instance — so a row that failed to materialize can reach the same verdict
// without an in-memory Instance.
//
// It replays FromInstanceData's FULL fence-restoration order before classifying,
// because a persisted row may carry a rollback fence that projected
// StartupStateUnknown=true to fence an older binary: RestoreArchiveRollbackFence
// and RestoreRelocationRecoveryOriginals run first (the loader calls both before
// any handoff/account-swap restore), then the handoff and account-swap fences. A
// row whose relocation-recovery metadata is undecodable cannot be materialized by
// the loader at all; the only durable field the missing original can change is
// StartupStateUnknown, which is terminal either way for a non-archived sandbox
// row, so a sandbox ghost whose handoff the loader would NOT fence (no mission,
// a known ambiguous verdict, or the missing evidence a legacy record carries)
// and that has no pending account swap or in-flight op has no field its loaded
// form could be pending on. LoadedActivity reports ActivityTerminal for that
// shape — holding it pending would wedge the cap forever on a row that can never
// materialize to clear TaskRunActive — and ActivityPending for the rows that DO
// carry a fence-requiring or unrecognized handoff, or whose liveness this binary
// does not recognize, or any non-sandbox row whose loaded form may still be
// active. A committed kill (UserKilled) is the one definitive terminal marker
// the loader honors before any fence (ClassifyActivity returns terminal for it
// first), so a tombstoned row whose relocation recovery is undecodable still
// releases — the sandbox arm of rawTaskRunHoldsSlot delegates to LoadedActivity
// instead of the raw UserKilled guard, so honoring the tombstone here is what
// keeps the kill terminal on the raw path too.
//
// The loaded form's activity is computed from the InFlightOp FromInstanceData
// reconstructs, NOT from the raw PendingHandoffMission. ClassifyActivity on a raw
// record returns ActivityPending for any non-empty PendingHandoffMission (line 99),
// but the loader reconstructs OpReplacing only for the verdicts that still own an
// in-flight obligation the daemon resolves itself — PromptNotDelivered (automatic
// replay) and PromptDelivered (crash-window settle). An ambiguous verdict
// (PromptCouldNotConfirm, PromptSentUnverified, or the missing evidence a legacy
// record carries) loads WITHOUT the fence, so the loaded form has InFlightOp=OpNone.
// LifecycleView.Activity() reaches the terminal verdict for such a row by omitting
// PendingHandoffMission from the InstanceData it builds; LoadedActivity does the
// same for a raw record that never materialized, so a sandbox LiveLost ghost with
// an ambiguous handoff releases its slot instead of wedging the cap forever. An
// UNRECOGNIZED future verdict a newer release wrote is not the known ambiguous
// case, though: it may stand for an obligation still in flight, so LoadedActivity
// re-raises the mission and reports pending for it (the same version-skew safety
// raw ClassifyActivity applies to an unrecognized value) rather than release.
//
// A non-archived sandbox row that the loader does not short-circuit
// (StartupStateUnknown / PendingAccountSwap) is rewritten to inert LiveLost on
// load — started stays false regardless of the stored liveness — so a row
// ghosted mid-run (persisted LiveRunning/LiveReady/...) releases on the live arm.
// LoadedActivity applies that same rewrite before classifying, or the raw arm
// would read the stored LiveRunning as ActivityPending and hold a slot its
// materialized form releases, wedging the cap on the common pre-restart record.
// The rewrite is restricted to liveness values this binary recognizes: a future
// value a newer release wrote stays pending, the same fail-closed direction
// ClassifyActivity takes for an unrecognized liveness, rather than being
// converted to LiveLost and releasing a slot whose real state is unknown.
func LoadedActivity(data InstanceData) (Activity, string) {
	// A committed kill is terminal even when a rollback fence or undecodable
	// relocation-recovery record would otherwise leave the verdict unknown.
	// ClassifyActivity honors UserKilled before any fence (it returns terminal
	// first), and the sandbox arm of rawTaskRunHoldsSlot now routes through
	// LoadedActivity instead of the raw UserKilled guard, so this check is what
	// keeps a tombstoned sandbox ghost — including one whose relocation recovery
	// cannot be decoded — terminal on the raw path instead of wedging the cap.
	if data.UserKilled {
		return ActivityTerminal, "session was killed and its teardown is pending"
	}
	data = data.RestoreArchiveRollbackFence()
	restored, err := data.RestoreRelocationRecoveryOriginals()
	if err != nil {
		// A relocation-undecodable row cannot be materialized, so there is no
		// loaded form to read; LoadedActivity infers it. The only durable field
		// the missing relocation original can change is StartupStateUnknown,
		// and for a non-archived sandbox row BOTH of its values are terminal
		// (true -> ClassifyActivity's startup-unknown return; false -> the
		// loader's inert LiveLost rewrite below). A sandbox ghost whose only
		// durable fields are a handoff the loader would NOT reconstruct a fence
		// for (no mission, or a known ambiguous verdict, or the missing
		// evidence a legacy record carries) therefore has no field that could
		// make its loaded form pending, and because the row can never
		// materialize there is no lifecycle edge that clears TaskRunActive —
		// holding it pending wedges max_concurrent_runs forever on the very
		// ghost this change exists to release. Reserve the conservative pending
		// verdict for rows whose durable transaction fields could actually
		// produce pending activity the loader would hold on — a fence-requiring
		// handoff (PromptNotDelivered/PromptDelivered) or a verdict this binary
		// does not recognize — and for a row whose liveness this binary does not
		// recognize (a future value a newer release wrote, which ClassifyActivity
		// fails closed to pending). A non-sandbox row's liveness is not
		// rewritten, so it stays pending too rather than guessing terminal about
		// a backend whose loaded form may still be active.
		if isSandboxBackendType(data.BackendType) &&
			data.PendingAccountSwap == nil &&
			inFlightOpFromData(data) == OpNone &&
			livenessIsKnown(livenessFromData(data)) &&
			!handoffCouldHoldSlot(data.PendingHandoffMission, data.HandoffDeliveryStatus) {
			return ActivityTerminal, "session is lost (its backing sandbox vanished and its relocation record is undecodable); recover it with 'af sessions restore' before watching again"
		}
		return ActivityPending, ""
	}
	data = restored
	data = data.RestoreHandoffRollbackFence()
	data = data.RestoreAccountSwapRollbackFence()
	data = data.restoreMissingHandoffMissionEvidence()
	data = data.restoreMissingAccountSwapMissionEvidence()
	op := inFlightOpFromData(data)
	if data.UserKilled {
		op = OpNone
	} else if data.PendingHandoffMission != "" && !data.StartupStateUnknown && op == OpNone &&
		pendingHandoffMissionNeedsFence(data.HandoffDeliveryStatus) {
		op = OpReplacing
	}
	effective := InstanceData{
		Liveness:            livenessFromData(data),
		InFlightOp:          op,
		UserKilled:          data.UserKilled,
		StartupStateUnknown: data.StartupStateUnknown,
	}
	if data.PendingAccountSwap != nil {
		effective.PendingAccountSwap = &AccountSwapData{}
	}
	// A newer release may persist a PendingHandoffMission with a
	// HandoffDeliveryStatus this binary does not recognize. The two known
	// in-flight verdicts (PromptNotDelivered/PromptDelivered) reconstruct
	// OpReplacing above; the known ambiguous verdicts
	// (PromptCouldNotConfirm/PromptSentUnverified, plus the missing evidence a
	// legacy record carries, which restoreMissingHandoffMissionEvidence
	// normalizes to PromptCouldNotConfirm) load without the fence and release
	// — an operator decision, not an automatic obligation. An UNKNOWN future
	// verdict may stand for an obligation still in flight, and the pre-#5218
	// raw path held such a row counted (raw ClassifyActivity returns pending for
	// any non-empty PendingHandoffMission). Re-raise the mission on the
	// effective record so ClassifyActivity returns pending for it — the same
	// version-skew safety it applies to an unrecognized liveness — instead of
	// letting the inert sandbox rewrite below turn the unknown verdict into a
	// released slot.
	if data.PendingHandoffMission != "" && !data.StartupStateUnknown &&
		data.HandoffDeliveryStatus != "" && !data.HandoffDeliveryStatus.Valid() {
		effective.PendingHandoffMission = data.PendingHandoffMission
	}
	if isSandboxBackendType(data.BackendType) &&
		!data.StartupStateUnknown && data.PendingAccountSwap == nil &&
		effective.Liveness != LiveArchived && livenessIsKnown(effective.Liveness) {
		effective.Liveness = LiveLost
	}
	return ClassifyActivity(effective)
}

// livenessIsKnown reports whether lv is one of the Liveness values this binary
// understands. A record written by a newer release may carry a value this build
// has no constant for; the inert-sandbox rewrite in LoadedActivity must not
// turn that future state into LiveLost (and so into permission to exceed the
// task cap), because the older daemon cannot know whether the newer state is
// active. ClassifyActivity already fails such a value closed onto
// ActivityPending (its liveness switch has no case for it), and the rewrite
// preserves that by leaving an unknown liveness alone.
func livenessIsKnown(lv Liveness) bool {
	switch lv {
	case LivenessUnset, LiveRunning, LiveReady, LiveLost, LiveDead, LiveArchived, LiveLimitReached:
		return true
	}
	return false
}

// handoffCouldHoldSlot reports whether a PendingHandoffMission could make a
// row's loaded form hold its task-run slot. Only a verdict that reconstructs
// the replacement fence does so: PromptNotDelivered/PromptDelivered rebuild
// OpReplacing, which keeps the loaded sandbox pending. An UNRECOGNIZED future
// verdict a newer release wrote may stand for an obligation still in flight,
// so the conservative raw path holds for it too. A known ambiguous verdict
// (PromptCouldNotConfirm/PromptSentUnverified) and the missing evidence a
// legacy record carries (an empty verdict) load WITHOUT the fence — the
// operator owns the confirm-or-retry decision — so the loaded form is terminal
// and there is nothing to hold the slot for. LoadedActivity uses this on the
// relocation-undecodable path, where restoreMissingHandoffMissionEvidence has
// not yet normalized the empty legacy verdict, so it reads "" as the ambiguous
// case the loader treats it as.
func handoffCouldHoldSlot(mission string, status PromptDeliveryStatus) bool {
	if mission == "" {
		return false
	}
	return pendingHandoffMissionNeedsFence(status) ||
		(status != "" && !status.Valid())
}

// classifyActivityByStatus is the legacy-Status fallback for ClassifyActivity,
// used only for records written before the liveness axis existed (#1195).
func classifyActivityByStatus(s Status) (Activity, string) {
	switch s {
	case Ready:
		return ActivityIdle, "idle (ready for review)"
	case Running, Loading, Deleting:
		return ActivityPending, ""
	case Lost:
		return ActivityTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case Dead:
		return ActivityTerminal, "session is dead (its backing tmux/worktree vanished)"
	case Archived:
		return ActivityTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	}
	return ActivityPending, ""
}
