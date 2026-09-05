package session

import "fmt"

// RuntimeAction names every operation that can start, replace, or resume a
// session runtime after creation. Backend capability and lifecycle eligibility
// are deliberately separate questions: a backend can know how to perform an
// action while this particular row is Archived, Lost, or pending deletion.
//
// Keep this list exhaustive. Every production runtime-entry chokepoint validates
// one of these actions before delegating to a backend:
//   - RestoreArchivedWorktree / RestoreFromArchive: RuntimeActionRestoreArchived
//   - their *HeldFenced twins: RuntimeActionRestoreArchivedFenced
//   - the daemon's manual Lost/Dead router: RuntimeActionRestoreLostOrDead
//   - Instance.Recover / BeginRecoverFence: RuntimeActionRecoverLost
//   - Instance.RecoverHeldFencedWithLiveBoundary: RuntimeActionRecoverFenced
//   - Instance.Respawn: RuntimeActionResumeLimit
//   - SwapAgentProgram / Instance.SwapAgent: RuntimeActionHandoff
//
// The universal pending-kill veto lives in ValidateRuntimeAction, outside the
// per-action switch, so adding a new action cannot accidentally omit it.
type RuntimeAction int

const (
	RuntimeActionRestoreArchived RuntimeAction = iota
	RuntimeActionRestoreArchivedFenced
	RuntimeActionRestoreLostOrDead
	RuntimeActionRecoverLost
	RuntimeActionRecoverFenced
	RuntimeActionResumeLimit
	RuntimeActionHandoff
	numRuntimeActions
)

// ValidateRuntimeAction checks whether one consistent lifecycle snapshot may
// perform action. It returns user-facing errors because callers must explain why
// retrying cannot work, especially when a durable kill tombstone owns the row.
func (v LifecycleView) ValidateRuntimeAction(action RuntimeAction) error {
	if v.UserKilled {
		return fmt.Errorf("session %q has a pending kill", v.Title)
	}
	if v.StartupStateUnknown {
		return fmt.Errorf("session %q has an unknown startup state; inspect its workspace and runtime before explicitly removing it", v.Title)
	}
	if v.PendingAccountSwap && action != RuntimeActionResumeLimit {
		return fmt.Errorf("session %q has a committed account swap awaiting its replacement notice and task; retry that account swap before another lifecycle action", v.Title)
	}

	switch action {
	case RuntimeActionRestoreArchived:
		if v.Liveness != LiveArchived {
			return fmt.Errorf("session %q is not archived", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
		}
	case RuntimeActionRestoreArchivedFenced:
		// RuntimeActionRestoreArchived with the fence required UP rather than absent:
		// it names the continuation of an archived restore whose OpRestoring the
		// caller already holds. The daemon's LOCAL archived route raises MarkRestoring
		// at the top so the fence covers the relocation claim, the repo-gone guard,
		// the destination derivation and the worktree relocate — every slow step in
		// front of the re-spawn (#3596).
		//
		// Separate rather than a relaxation, for the reason RuntimeActionRestoreArchived
		// still demands OpNone: that action is the PUBLIC "may a restore be started on
		// this row" question, and accepting OpRestoring there would answer yes for a
		// row whose restore is already running. Liveness stays LiveArchived because the
		// fence this names is MarkRestoring, which moves the op axis alone — BeginRestore
		// is still what flips the row live, and still only from OpNone.
		if v.Liveness != LiveArchived {
			return fmt.Errorf("session %q is not archived", v.Title)
		}
		if v.InFlightOp != OpRestoring {
			return fmt.Errorf("session %q is not under a restore fence (in-flight op is %s)", v.Title, opLabel(v.InFlightOp))
		}
	case RuntimeActionRestoreLostOrDead:
		if v.Liveness != LiveLost && v.Liveness != LiveDead {
			return fmt.Errorf("session %q is not lost or dead", v.Title)
		}
		if !v.Started {
			return fmt.Errorf("session %q is not started", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
		}
	case RuntimeActionRecoverLost:
		if v.Liveness != LiveLost {
			return fmt.Errorf("session %q is not lost", v.Title)
		}
		if !v.Started {
			return fmt.Errorf("session %q is not started", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
		}
	case RuntimeActionRecoverFenced:
		// The SAME precondition as RuntimeActionRecoverLost on every axis but one:
		// the restore fence is required to be UP rather than absent, because this
		// action names the continuation of a recover whose fence the caller already
		// holds (daemon/restore.go raises it for the whole manual restore, so the
		// slow network phases in front of the backend call are covered too — #3586).
		//
		// It is a separate action rather than a relaxation of RuntimeActionRecoverLost
		// for the reason that one still requires OpNone: that action is the PUBLIC
		// "may this row be recovered" question, asked by the daemon's automatic loop
		// (lostSessionWantsRestore) and by callers deciding whether to start one at
		// all. Weakening it to accept OpRestoring would tell every one of them that a
		// restore already in flight is a restore they may start, which is exactly the
		// re-entrancy #3555 closed. Naming the two states separately keeps the
		// admission question strict and makes the continuation explicit.
		if v.Liveness != LiveLost {
			return fmt.Errorf("session %q is not lost", v.Title)
		}
		if !v.Started {
			return fmt.Errorf("session %q is not started", v.Title)
		}
		if v.InFlightOp != OpRestoring {
			return fmt.Errorf("session %q is not under a restore fence (in-flight op is %s)", v.Title, opLabel(v.InFlightOp))
		}
	case RuntimeActionResumeLimit:
		if v.Liveness != LiveLimitReached {
			return fmt.Errorf("session %q is not blocked on a usage limit", v.Title)
		}
		if !v.Started {
			return fmt.Errorf("session %q is not started", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
		}
	case RuntimeActionHandoff:
		// The reserved root agent is the daemon's own singleton: it is re-ensured
		// when it dies, so swapping its agent out from under the daemon is not a
		// thing that can be committed to. This is checked FIRST, and here rather
		// than at a caller, for two different reasons.
		//
		// First over the others because it is permanent. A busy row says "try again
		// in a moment"; root will never become eligible, and sending the user back
		// to retry a thing that cannot work is the worse of the two answers.
		//
		// Here rather than at a caller because the daemon used to hold this rule
		// alone, next to its own call into this function. The TUI asks this
		// predicate and nothing else before opening the handoff picker, so it
		// believed root was eligible and only surfaced the refusal after the user
		// had picked an agent and confirmed the swap (#2436). A rule each caller has
		// to remember separately is one a caller will forget; this is the question
		// they already share.
		if IsReservedTitle(v.Title) {
			return fmt.Errorf("session %q is the daemon-managed root agent and cannot be handed off", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
		}
		switch v.Liveness {
		case LiveRunning, LiveReady, LiveLimitReached:
			// A live, idle, or limit-parked agent has a runtime to replace.
			if !v.Started {
				return fmt.Errorf("session %q is not running and cannot be handed off", v.Title)
			}
		case LiveArchived:
			return fmt.Errorf("session %q is archived and cannot be handed off; restore it first", v.Title)
		case LiveLost, LiveDead:
			return fmt.Errorf("session %q is not running and cannot be handed off; restore it first", v.Title)
		default:
			return fmt.Errorf("session %q is not available to hand off", v.Title)
		}
	default:
		return fmt.Errorf("unknown runtime action %d", action)
	}
	return nil
}

// ValidateRuntimeAction is the locking form for live instances.
func (i *Instance) ValidateRuntimeAction(action RuntimeAction) error {
	return i.LifecycleView().ValidateRuntimeAction(action)
}

func runtimeActionBusyError(v LifecycleView) error {
	return fmt.Errorf("session %q is busy (%v); try again in a moment", v.Title, v.Status)
}
