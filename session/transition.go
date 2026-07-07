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
	case tkClearOp:
		return "ClearOp"
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
		// Always legal: a kill supersedes any in-flight op (see BeginKill doc). I1
		// is enforced by the daemon KillSession ordering, not this overlay.
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpKilling} },
	},
	tkRevertKill: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpKilling },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
	},
	tkBeginArchive: {
		// Any non-archived session with no op in flight may be archived — matching
		// the daemon's guards, which reject only an already-archived or busy
		// session (a Lost / LimitReached session is archivable: shelving it tears
		// down whatever tmux remains and moves the worktree out). I4 is the fence
		// itself, not a liveness restriction.
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone && s.liveness != LiveArchived },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpArchiving} },
	},
	tkCommitArchive: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpArchiving },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveArchived, OpNone} },
		started:     startedClear,
	},
	tkAbortArchiveToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpArchiving },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
	},
	tkBeginRestore: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone && s.liveness == LiveArchived },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpRestoring} },
		// started=true mirrors RestoreFromArchive: Recover's !Started() gate would
		// otherwise short-circuit and the restore would never start (Greptile #1314).
		started: startedSet,
	},
	tkAbortRestoreToLost: {
		allowedFrom: func(s stateAxes) bool { return s.op == OpRestoring && s.liveness == LiveLost },
		target:      func(stateAxes, TransitionEvent) stateAxes { return stateAxes{LiveLost, OpNone} },
	},
	tkMarkRestoring: {
		// Optimistic restore overlay: op None -> OpRestoring, liveness UNCHANGED
		// (the reconcile keys its rebuild on the still-Archived liveness, #1203).
		allowedFrom: func(s stateAxes) bool { return s.op == OpNone },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpRestoring} },
	},
	tkClearOp: {
		// Clearing an optimistic overlay back to None is always valid — it never
		// resurrects or teardown-clobbers (liveness is untouched).
		allowedFrom: func(stateAxes) bool { return true },
		target:      func(s stateAxes, _ TransitionEvent) stateAxes { return stateAxes{s.liveness, OpNone} },
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
	i.liveness = to.liveness
	i.inFlightOp = to.op
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
	}
	return fmt.Sprintf("InFlightOp(%d)", int(op))
}
