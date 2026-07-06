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
//   - I1 tombstone-before-kill: a kill may not begin unless the kill-intent
//     tombstone is recorded — otherwise a crash mid-teardown reclassifies the
//     session Lost and resurrects it.
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
	}
	return fmt.Sprintf("transitionKind(%d)", int(k))
}

// TransitionEvent is a lifecycle event handed to Instance.Transition. Construct
// one with the exported constructors below; lv is meaningful only for
// ObserveLiveness.
type TransitionEvent struct {
	kind transitionKind
	lv   Liveness
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

// BeginKill overlays OpKilling for an optimistic kill. Requires the kill-intent
// tombstone already recorded (I1).
func BeginKill() TransitionEvent { return TransitionEvent{kind: tkBeginKill} }

// RevertKill clears an optimistic kill overlay (kill aborted / reverted).
func RevertKill() TransitionEvent { return TransitionEvent{kind: tkRevertKill} }

// BeginArchive raises the OpArchiving fence over an archive teardown+move (I4).
func BeginArchive() TransitionEvent { return TransitionEvent{kind: tkBeginArchive} }

// CommitArchive flips the session to the inert Archived state, started=false, on
// a successful archive move (was SetArchived). Reachable only from the fence (I2).
func CommitArchive() TransitionEvent { return TransitionEvent{kind: tkCommitArchive} }

// AbortArchiveToLost rolls a failed archive move back to Lost so the restore
// loop heals the agent in place.
func AbortArchiveToLost() TransitionEvent { return TransitionEvent{kind: tkAbortArchiveToLost} }

// BeginRestore enters the restore fence for an archived session (I3): Lost +
// OpRestoring (replaces RestoreFromArchive's "park in Lost" head).
func BeginRestore() TransitionEvent { return TransitionEvent{kind: tkBeginRestore} }

// AbortRestoreToLost drops a failed restore's fence to a plain Lost so the
// #1108 loop retries against the now-restored worktree.
func AbortRestoreToLost() TransitionEvent { return TransitionEvent{kind: tkAbortRestoreToLost} }

// stateAxes is the two-axis lifecycle state a transition reads and writes.
type stateAxes struct {
	liveness Liveness
	op       InFlightOp
}

// edgeSpec is one row of the allowed-edge table: which from-states an event is
// legal from, the resulting state, and the event's side effects.
type edgeSpec struct {
	// allowedFrom reports whether the event is legal from state s.
	allowedFrom func(s stateAxes) bool
	// target computes the resulting state (ev carries ObserveLiveness's lv).
	target func(s stateAxes, ev TransitionEvent) stateAxes
	// clearsStarted marks whether the transition also sets started=false
	// (CommitArchive: the inert Archived state has no tmux behind it).
	clearsStarted bool
	// yieldWhenBlocked makes an out-of-set from-state a silent no-op instead of a
	// rejection — for the daemon-truth edge (ObserveLiveness is always allowed)
	// and ConfirmLive (yields to an in-flight teardown rather than fighting it).
	yieldWhenBlocked bool
	// requiresTombstone gates BeginKill on a recorded kill intent (I1).
	requiresTombstone bool
}

// transitionTable is the allowed-edge table — the single declarative source of
// truth for which (liveness, op) → (liveness, op) transitions are legal. Every
// enforcement of I1–I4 lives here as a from-state predicate, not as a guard
// scattered across the daemon/app call sites.
var transitionTable = map[transitionKind]edgeSpec{
	tkBeginCreate: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpCreating} },
	},
	tkConfirmLive: {
		allowedFrom:      func(s stateAxes) bool { return s.op == OpNone || s.op == OpCreating || s.op == OpRestoring },
		target:           func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveRunning, OpNone} },
		yieldWhenBlocked: true,
	},
	tkObserveLiveness: {
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, ev TransitionEvent) stateAxes { return stateAxes{ev.lv, s.op} },
	},
	tkBeginKill: {
		allowedFrom:       func(s stateAxes) bool { return s.op == OpNone },
		target:            func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpKilling} },
		requiresTombstone: true,
	},
	tkRevertKill: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpKilling },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
	},
	tkBeginArchive: {
		allowedFrom: func(s stateAxes) bool {
			return s.op == OpNone && (s.liveness == LiveRunning || s.liveness == LiveReady)
		},
		target: func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpArchiving} },
	},
	tkCommitArchive: {
		allowedFrom:   func(s stateAxes) bool { return s.op == OpArchiving },
		target:        func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveArchived, OpNone} },
		clearsStarted: true,
	},
	tkAbortArchiveToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpArchiving },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
	},
	tkBeginRestore: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone && s.liveness == LiveArchived },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpRestoring} },
	},
	tkAbortRestoreToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpRestoring && s.liveness == LiveLost },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
	},
}

// onIllegalTransition, when non-nil, is invoked with the rejection message
// before Transition returns the soft error. Test builds install a panicking hook
// (an illegal, mis-ordered transition must be a loud red failure, never a silent
// prod corruption); production leaves it nil and degrades to the soft error — a
// user's daemon must not crash on a racing double-archive. Keeps `testing` out
// of the production binary.
var onIllegalTransition func(msg string)

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
	if spec.requiresTombstone && !i.userKilled {
		return i.rejectTransitionLocked(ev, from, "kill without a recorded tombstone (I1)")
	}

	to := spec.target(from, ev)
	i.liveness = to.liveness
	i.inFlightOp = to.op
	if spec.clearsStarted {
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
	}
	return fmt.Sprintf("InFlightOp(%d)", int(op))
}
