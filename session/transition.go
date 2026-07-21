package session

import (
	"errors"
	"fmt"
)

// The lifecycle transition chokepoint (#1195 Phase 2c).
//
// Phase 1 split the session state into two axes — liveness (daemon-owned health)
// and inFlightOp (client-owned operation) — but the ~15 writers still poke those
// fields through a scatter of setters (SetLiveness, SetInFlightOp, MarkLive,
// SetArchived, RestoreFromArchive, …) with NO transition table: the ordering
// invariants that keep the lifecycle sound are enforced only by convention at
// the call sites. Transition makes them structural — a single validated
// chokepoint every writer routes through — so an illegal or mis-ordered
// transition is caught by the edge table instead of silently corrupting state.
//
// The four ordering invariants it enforces (audit item #6):
//   - I1 tombstone-before-teardown is NOT a (liveness,op) edge, so it is NOT
//     enforced here: the tombstone (userKilled) is a daemon-side field the
//     client optimistic kill overlay never has, and daemon-internal kills (reap,
//     create-cleanup) legitimately carry no tombstone. The daemon KillSession
//     enforces I1 by ordering (MarkUserKilled before Instance.Kill) while setting
//     NO op — keeping the snapshot pure liveness for out-of-band kills. BeginKill
//     is therefore an unconstrained optimistic overlay. I2–I4 below are genuine
//     edges the table enforces.
//   - I2 move-before-Archived: the inert Archived state is reachable ONLY out of
//     the archive fence, which is only entered after the worktree move ran.
//   - I3 move-before-respawn: a restore may begin only from Archived, and the
//     daemon moves the worktree back before entering the restore fence.
//   - I4 fence-brackets-archive: Archived/Lost is reachable from an archive only
//     through the OpArchiving fence, so the poll/tab-spawn skip the whole window.
//
// Phase 2c lands this INERT: the API + table + tests, with no call-site
// migration. Phase 2d migrates the daemon/app writers onto it and deletes the
// ad-hoc guards; Phase 2e retires the direct setters.

// transitionKind enumerates the lifecycle events Transition accepts. Each maps
// to exactly one guarded edge (family) in transitionTable.
type transitionKind int

const (
	tkBeginCreate transitionKind = iota
	tkConfirmLive
	tkObserveLiveness
	tkBeginKill
	tkRevertKill
	tkBeginArchive
	tkCommitArchive
	tkAbortArchiveToLost
	tkBeginRestore
	tkAbortRestoreToLost
	tkMarkRestoring
	tkBeginHandoff
	tkCommitHandoff
	tkAbortHandoff
	tkClearOp
	numTransitionKinds
)

func (k transitionKind) String() string {
	switch k {
	case tkBeginCreate:
		return "BeginCreate"
	case tkConfirmLive:
		return "ConfirmLive"
	case tkObserveLiveness:
		return "ObserveLiveness"
	case tkBeginKill:
		return "BeginKill"
	case tkRevertKill:
		return "RevertKill"
	case tkBeginArchive:
		return "BeginArchive"
	case tkCommitArchive:
		return "CommitArchive"
	case tkAbortArchiveToLost:
		return "AbortArchiveToLost"
	case tkBeginRestore:
		return "BeginRestore"
	case tkAbortRestoreToLost:
		return "AbortRestoreToLost"
	case tkMarkRestoring:
		return "MarkRestoring"
	case tkBeginHandoff:
		return "BeginHandoff"
	case tkCommitHandoff:
		return "CommitHandoff"
	case tkAbortHandoff:
		return "AbortHandoff"
	case tkClearOp:
		return "ClearOp"
	}
	return fmt.Sprintf("transitionKind(%d)", int(k))
}

// TransitionEvent is a lifecycle event handed to Instance.Transition. Construct
// one with the exported constructors below; lv is meaningful only for
// ObserveLiveness. epoch/epochScoped are set only by AtEpoch.
type TransitionEvent struct {
	kind        transitionKind
	lv          Liveness
	epoch       uint64
	epochScoped bool
}

// AtEpoch scopes an event to the state epoch its decision was made at (#2135):
// Transition applies it only while the instance is still at that epoch, and
// silently DROPS it once a newer authoritative transition has moved the state on.
// Use it wherever the event is a conclusion drawn from an observation taken
// earlier — the daemon poll settling liveness from pane content it captured a
// moment ago — so a decision about a state the session has already left cannot
// overwrite the one it moved to. Events constructed without it are unscoped and
// apply unconditionally, exactly as before. See session/state_epoch.go.
func (ev TransitionEvent) AtEpoch(epoch uint64) TransitionEvent {
	ev.epoch = epoch
	ev.epochScoped = true
	return ev
}

// BeginCreate overlays OpCreating for an optimistic create (was SetStatus(Loading)).
func BeginCreate() TransitionEvent { return TransitionEvent{kind: tkBeginCreate} }

// ConfirmLive marks a completed create/recover live — Running, op cleared (was
// MarkLive). It YIELDS (no-op) when a kill/archive op is in flight, so a
// completing spawn never resurrects a session a teardown owns.
func ConfirmLive() TransitionEvent { return TransitionEvent{kind: tkConfirmLive} }

// ObserveLiveness applies the daemon's authoritative liveness (was SetLiveness).
// It is the unconditional daemon-truth edge: it sets liveness and preserves the
// op axis, and never rejects — which is what keeps the #1187 strand impossible.
func ObserveLiveness(lv Liveness) TransitionEvent {
	return TransitionEvent{kind: tkObserveLiveness, lv: lv}
}

// BeginKill overlays OpKilling for an optimistic kill. It is always legal — a
// kill is the terminal user intent and supersedes any in-flight op. I1
// (tombstone-before-teardown) is NOT enforced here: the tombstone is a daemon-
// side field the client optimistic overlay never has, and the daemon's
// KillSession enforces I1 by ordering (MarkUserKilled before Instance.Kill)
// while setting NO op (the snapshot stays pure liveness for out-of-band kills).
func BeginKill() TransitionEvent { return TransitionEvent{kind: tkBeginKill} }

// RevertKill clears an optimistic kill overlay (kill aborted / reverted).
func RevertKill() TransitionEvent { return TransitionEvent{kind: tkRevertKill} }

// BeginArchive raises the OpArchiving fence over an archive teardown+move (I4).
func BeginArchive() TransitionEvent { return TransitionEvent{kind: tkBeginArchive} }

// CommitArchive flips the session to the inert Archived state, started=false, on
// a successful archive move (the daemon path). Reachable ONLY from the OpArchiving
// fence (I2). The TUI's finalize is a separate unconditional projection-mirror
// (SetArchived), not this fenced commit — it copies the daemon's already-committed
// Archived state onto the read-only row, so it is not subject to I2.
func CommitArchive() TransitionEvent { return TransitionEvent{kind: tkCommitArchive} }

// AbortArchiveToLost rolls a failed archive move back to Lost so the restore
// loop heals the agent in place.
func AbortArchiveToLost() TransitionEvent { return TransitionEvent{kind: tkAbortArchiveToLost} }

// BeginRestore enters the restore fence for a restorable session (I3): Lost +
// OpRestoring (replaces RestoreFromArchive's "park in Lost" head).
func BeginRestore() TransitionEvent { return TransitionEvent{kind: tkBeginRestore} }

// AbortRestoreToLost drops a failed restore's fence to a plain Lost so the
// #1108 loop retries against the now-restored worktree.
func AbortRestoreToLost() TransitionEvent { return TransitionEvent{kind: tkAbortRestoreToLost} }

// MarkRestoring overlays OpRestoring WITHOUT touching liveness — the TUI's
// optimistic restore action. It deliberately keeps liveness=Archived (unlike
// BeginRestore, the daemon edge, which flips to Lost) so the reconcile still
// sees the Archived→live transition and rebuilds the row (#1203), while
// ShownArchived re-homes it into the live section eagerly (#1210).
func MarkRestoring() TransitionEvent { return TransitionEvent{kind: tkMarkRestoring} }

// BeginHandoff raises the OpReplacing fence before the outgoing pane is touched.
func BeginHandoff() TransitionEvent { return TransitionEvent{kind: tkBeginHandoff} }

// CommitHandoff settles a successfully launched incoming agent as Running.
func CommitHandoff() TransitionEvent { return TransitionEvent{kind: tkCommitHandoff} }

// AbortHandoff drops a replacement fence whose runtime swap did not complete.
func AbortHandoff() TransitionEvent { return TransitionEvent{kind: tkAbortHandoff} }

// ClearOp drops any in-flight optimistic op back to None, leaving liveness
// untouched — the client-projection bookkeeping for when an optimistic op's
// outcome is confirmed by the reconcile or the op's RPC failed and the overlay
// must revert to the underlying daemon liveness.
func ClearOp() TransitionEvent { return TransitionEvent{kind: tkClearOp} }

// stateAxes is the two-axis lifecycle state a transition reads and writes.
type stateAxes struct {
	liveness Liveness
	op       InFlightOp
}

// startedEffect is a transition's effect on the started flag: most transitions
// leave it to the teardown/spawn machinery, but two own it — CommitArchive
// clears it (the inert Archived state has no tmux behind it) and BeginRestore
// sets it (mirroring RestoreFromArchive, whose head flips started=true before
// Recover so the re-spawn is eligible — Recover's !Started() gate would
// otherwise short-circuit and the restore would silently never start).
type startedEffect int

const (
	startedUnchanged startedEffect = iota
	startedSet                     // started = true (BeginRestore)
	startedClear                   // started = false (CommitArchive)
)

// runEffect is a transition's effect on the task-run marker (#1892):
// Instance.taskRunActive, "is the run this session was spawned for still in
// flight?", which is the one fact the watch-task concurrency cap counts.
//
// It is declared PER TRANSITION, in the table, rather than derived from the
// resulting state — and that is the whole point. The marker has been wrong four
// times, each time at a transition nobody had asked the question about:
// completion, archive-failure, the Lost→Running race, and archive-commit. Every
// one was the same question — "at THIS transition, does the run still own a
// slot?" — and the answer kept being reconstructed from whatever neighbouring
// state was nearby. A fact only helps if its lifecycle covers every edge the thing
// can take, so every kind names its effect here and the exhaustiveness test
// (TestTransitionTable_EveryKindHasSpec) rejects a row that does not. A new
// transition cannot be added without answering the question.
//
// The zero value is deliberately INVALID: omission must fail loudly, not default
// to "keep" and become door number five.
type runEffect int

const (
	// runEffectUnset is the zero value: no answer given. Always a bug.
	runEffectUnset runEffect = iota
	// runKeep: this transition says nothing about whether the run is in flight.
	// The marker carries whatever it already held.
	runKeep
	// runEndsOnIdleEdge: this transition ends the run IF it settles the AGENT idle.
	// Only the daemon-truth edge (ObserveLiveness) can do that — every other kind
	// either targets a fixed non-Ready liveness or leaves liveness alone.
	//
	// The EDGE is what matters, not the resulting state: a session is born
	// LiveReady before its agent has ever run, so "liveness == LiveReady" alone
	// would end the run at birth. It ends only on a transition INTO Ready from
	// somewhere else, which is the agent finishing its turn.
	//
	// Deliberately keyed on the LIVENESS axis and not on ClassifyActivity: an
	// agent that goes idle while the daemon happens to be archiving it is idle —
	// its run is over — even though ClassifyActivity calls the in-flight op
	// pending. That conflation is what let a finished run reclaim a slot through
	// the archive door.
	runEndsOnIdleEdge
	// runEnds: this transition ends the run outright, whatever the agent was doing.
	// CommitArchive only: a committed archive is the user deliberately shelving the
	// session. Its slot is already released (an Archived session is not restorable
	// in place), and the run must not come back — otherwise a later RestoreArchived
	// re-enters via BeginRestore/ConfirmLive, the old run counts again, and the cap
	// is exceeded by however many events took its slot in the meantime.
	//
	// The distinction against AbortArchiveToLost is exact: teardown that SUCCEEDS
	// ends the run; teardown that FAILS leaves the run exactly as interrupted as it
	// was, for the restore loop to heal in place.
	runEnds
)

// edgeSpec is one row of the allowed-edge table: which from-states an event is
// legal from, the resulting state, and the event's side effects.
type edgeSpec struct {
	// allowedFrom reports whether the event is legal from state s.
	allowedFrom func(s stateAxes) bool
	// target computes the resulting state (ev carries ObserveLiveness's lv).
	target func(s stateAxes, ev TransitionEvent) stateAxes
	// started is the transition's effect on the started flag (default: leave it
	// to the teardown/spawn machinery).
	started startedEffect
	// run is the transition's effect on the task-run marker (#1892). Required —
	// the zero value is invalid and the exhaustiveness test rejects it, so a new
	// transition must state whether the run it lands in is still in flight.
	run runEffect
	// yieldWhenBlocked makes an out-of-set from-state a silent no-op instead of a
	// rejection — for the daemon-truth edge (ObserveLiveness is always allowed)
	// and ConfirmLive (yields to an in-flight teardown rather than fighting it).
	yieldWhenBlocked bool
}

// transitionTable is the allowed-edge table — the single declarative source of
// truth for which (liveness, op) → (liveness, op) transitions are legal. Every
// enforcement of I1–I4 lives here as a from-state predicate, not as a guard
// scattered across the daemon/app call sites.
var transitionTable = map[transitionKind]edgeSpec{
	tkBeginCreate: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpCreating} },
		// The run is beginning, not ending. NewInstance already opened it.
		run: runKeep,
	},
	tkConfirmLive: {
		allowedFrom:      func(s stateAxes) bool { return s.op == OpNone || s.op == OpCreating || s.op == OpRestoring },
		target:           func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveRunning, OpNone} },
		yieldWhenBlocked: true,
		// A spawn completing says the agent is up, not that its work is done. It
		// cannot REOPEN a finished run either: the marker only ever goes true→false,
		// so a restored archive (whose commit ended the run) stays ended here.
		run: runKeep,
	},
	tkObserveLiveness: {
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, ev TransitionEvent) stateAxes { return stateAxes{ev.lv, s.op} },
		// The ONLY kind that can settle the agent idle, and therefore the only one
		// that can end a run naturally. Running/LimitReached/Lost → Ready ends it;
		// every other observed liveness (Running, LimitReached, Lost, Dead) leaves it
		// alone — Lost in particular must not decide anything, since a finished and an
		// interrupted run are indistinguishable once lost.
		run: runEndsOnIdleEdge,
	},
	tkBeginKill: {
		// Always legal: a kill supersedes any in-flight op (see BeginKill doc). I1
		// is enforced by the daemon KillSession ordering, not this overlay.
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpKilling} },
		// A kill is an optimistic overlay that can be REVERTED (RevertKill), so it
		// must not end the run — a reverted kill would otherwise leave a live run
		// permanently uncounted. The slot is released anyway while the kill is on
		// record (the tombstone fences canAutoRestoreLostSession) and for good when
		// the record is deleted, so nothing is over-held.
		run: runKeep,
	},
	tkRevertKill: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpKilling },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
		// The kill was called off: the run is whatever it was before it.
		run: runKeep,
	},
	tkBeginArchive: {
		// Any non-archived session with no op in flight may be archived — matching
		// the daemon's guards, which reject only an already-archived or busy
		// session (a Lost / LimitReached session is archivable: shelving it tears
		// down whatever tmux remains and moves the worktree out). I4 is the fence
		// itself, not a liveness restriction.
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone && s.liveness != LiveArchived },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpArchiving} },
		// Raising the fence decides nothing: the archive may still commit (run ends)
		// or abort back to Lost (run continues). Only the outcome answers.
		run: runKeep,
	},
	tkCommitArchive: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpArchiving },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveArchived, OpNone} },
		started:     startedClear,
		// The archive SUCCEEDED: the user shelved this session and its slot is gone.
		// End the run permanently, or a later RestoreArchived re-enters through
		// BeginRestore/ConfirmLive and the old run counts again — on top of whatever
		// events already took its slot.
		run: runEnds,
	},
	tkAbortArchiveToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpArchiving },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
		// The archive FAILED and the restore loop will heal the agent in place, so
		// the run is exactly as interrupted as it was when the archive began — no
		// more, no less. If it had already finished, the marker is already false and
		// this must not resurrect it.
		run: runKeep,
	},
	tkBeginRestore: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone && s.liveness == LiveArchived },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpRestoring} },
		// started=true mirrors RestoreFromArchive: Recover's !Started() gate would
		// otherwise short-circuit and the restore would never start (Greptile #1314).
		started: startedSet,
		// Only reachable from Archived, whose commit already ended the run. Un-shelving
		// a session gives the user their workspace back; it does not re-open the task's
		// run, and must not re-take the task's slot.
		run: runKeep,
	},
	tkAbortRestoreToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpRestoring && s.liveness == LiveLost },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
		// A failed restore changes nothing about the run it was trying to bring back.
		run: runKeep,
	},
	tkMarkRestoring: {
		// Optimistic restore overlay: op None -> OpRestoring, liveness UNCHANGED
		// (the reconcile keys its rebuild on the still-Archived liveness, #1203).
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpRestoring} },
		// A TUI-side optimistic overlay on a read-only projection. It owns no durable
		// state and the daemon never counts caps off it.
		run: runKeep,
	},
	tkBeginHandoff: {
		allowedFrom: func(s stateAxes) bool {
			return s.op == OpNone && (s.liveness == LiveRunning || s.liveness == LiveReady || s.liveness == LiveLimitReached)
		},
		target: func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpReplacing} },
		// Replacement continues the same task run under another agent.
		run: runKeep,
	},
	tkCommitHandoff: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpReplacing },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveRunning, OpNone} },
		run:         runKeep,
	},
	tkAbortHandoff: {
		allowedFrom:      func(s stateAxes) bool { return s.op == OpReplacing },
		target:           func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
		yieldWhenBlocked: true, // a terminal kill may supersede the replacement
		run:              runKeep,
	},
	tkClearOp: {
		// Clearing an optimistic overlay back to None is always valid — it never
		// resurrects or teardown-clobbers (liveness is untouched).
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
		// Dropping an overlay reveals the liveness underneath; it does not change what
		// the agent was doing.
		run: runKeep,
	},
}

// onIllegalTransition, when non-nil, is invoked with the rejection message
// before Transition returns the soft error. Test builds install a panicking hook
// (an illegal, mis-ordered transition must be a loud red failure, never a silent
// prod corruption); production leaves it nil and degrades to the soft error — a
// user's daemon must not crash on a racing double-archive. Keeps `testing` out
// of the production binary.
var onIllegalTransition func(msg string)

// SetIllegalTransitionHook installs fn as the illegal-transition hook and returns
// a restore func. It exists so test binaries in OTHER packages (app, daemon) can
// install the same panic-on-illegal guard the session tests install directly — a
// mis-ordered transition must be a loud failure everywhere a writer routes
// through the chokepoint, not only in session-package tests. Production never
// calls this: the hook stays nil and an illegal edge degrades to the soft error.
func SetIllegalTransitionHook(fn func(msg string)) (restore func()) {
	prev := onIllegalTransition
	onIllegalTransition = fn
	return func() { onIllegalTransition = prev }
}

// Transition applies a lifecycle event to the two-axis (liveness, inFlightOp)
// state under i.mu, validating it against the allowed-edge table. It is the
// single writer-side chokepoint for lifecycle-state changes (#1195 Phase 2c) and
// the enforcement point for the I1–I4 ordering invariants. An illegal edge
// returns an error AND fires onIllegalTransition (panic in test builds); a
// yielding edge (ObserveLiveness always, ConfirmLive under a teardown op) that
// is out-of-set is a silent no-op. INERT until Phase 2d migrates the writers.
func (i *Instance) Transition(ev TransitionEvent) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if ev.epochScoped && i.stateEpoch != ev.epoch {
		// The decision behind this event was drawn from an observation that a newer
		// authoritative transition has since superseded. Drop it — silently and
		// without an error, since being out-competed by newer truth is not a
		// mis-ordered edge (#2135). The observer re-decides on its next tick.
		return nil
	}
	from := stateAxes{i.liveness, i.inFlightOp}
	spec, ok := transitionTable[ev.kind]
	if !ok {
		return i.rejectTransitionLocked(ev, from, "unknown event")
	}
	if !spec.allowedFrom(from) {
		if spec.yieldWhenBlocked {
			return nil
		}
		return i.rejectTransitionLocked(ev, from, "edge not allowed from this state")
	}

	to := spec.target(from, ev)
	// Apply this transition's declared effect on the task run (#1892). The answer
	// comes from the table, not from reading the resulting state — see runEffect.
	//
	// Only ever true→false: a capped task creates one session per event, so a
	// session has exactly one run, and anything that happens to it afterwards
	// (a user prompting it, an un-archive) is not the task's.
	if i.taskRunActive {
		switch spec.run {
		case runEnds:
			i.taskRunActive = false
		case runEndsOnIdleEdge:
			// The AGENT's own axis, and the EDGE into it. Not ClassifyActivity — that
			// calls an in-flight archive "pending", which would miss an agent going
			// idle mid-teardown. Not the resulting state alone — a session is born
			// LiveReady before its agent ever runs, so that would end the run at birth.
			if to.liveness == LiveReady && from.liveness != LiveReady {
				i.taskRunActive = false
			}
		}
	}
	i.liveness = to.liveness
	i.inFlightOp = to.op
	// Every real change to the lifecycle state advances the epoch, so an observer
	// holding an older one learns its in-flight decision is stale (#2135).
	i.noteStateChangeLocked(from.liveness, from.op, i.limitResetAt)
	switch spec.started {
	case startedSet:
		i.started = true
	case startedClear:
		i.started = false
	}
	return nil
}

// rejectTransitionLocked builds the rejection error, fires the illegal-transition
// hook (panic in test), and returns the soft error. Caller holds i.mu; nothing
// is mutated, so a rejected transition leaves the state untouched.
func (i *Instance) rejectTransitionLocked(ev TransitionEvent, from stateAxes, why string) error {
	msg := fmt.Sprintf("illegal session transition %s from (liveness=%s, op=%s): %s",
		ev.kind, livenessLabel(from.liveness), opLabel(from.op), why)
	if onIllegalTransition != nil {
		onIllegalTransition(msg)
	}
	return errors.New(msg)
}

// livenessLabel / opLabel render the axes for transition error messages without
// adding String() methods to the axis types (which would change %v formatting
// elsewhere, e.g. in existing status logs).
func livenessLabel(lv Liveness) string {
	switch lv {
	case LivenessUnset:
		return "Unset"
	case LiveRunning:
		return "Running"
	case LiveReady:
		return "Ready"
	case LiveLost:
		return "Lost"
	case LiveDead:
		return "Dead"
	case LiveArchived:
		return "Archived"
	case LiveLimitReached:
		return "LimitReached"
	}
	return fmt.Sprintf("Liveness(%d)", int(lv))
}

func opLabel(op InFlightOp) string {
	switch op {
	case OpNone:
		return "None"
	case OpCreating:
		return "Creating"
	case OpKilling:
		return "Killing"
	case OpArchiving:
		return "Archiving"
	case OpRestoring:
		return "Restoring"
	case OpReplacing:
		return "Replacing"
	}
	return fmt.Sprintf("InFlightOp(%d)", int(op))
}
