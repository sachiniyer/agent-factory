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
//   - the daemon's manual Lost/Dead router: RuntimeActionRestoreLostOrDead
//   - Instance.Recover: RuntimeActionRecoverLost
//   - Instance.Respawn: RuntimeActionResumeLimit
//   - SwapAgentProgram / Instance.SwapAgent: RuntimeActionHandoff
//
// The universal pending-kill veto lives in ValidateRuntimeAction, outside the
// per-action switch, so adding a new action cannot accidentally omit it.
type RuntimeAction int

const (
	RuntimeActionRestoreArchived RuntimeAction = iota
	RuntimeActionRestoreLostOrDead
	RuntimeActionRecoverLost
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

	switch action {
	case RuntimeActionRestoreArchived:
		if v.Liveness != LiveArchived {
			return fmt.Errorf("session %q is not archived", v.Title)
		}
		if v.InFlightOp != OpNone {
			return runtimeActionBusyError(v)
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
