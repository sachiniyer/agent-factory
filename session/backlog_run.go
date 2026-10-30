package session

import "time"

// runGateActiveLocked reports whether a task run or a backlog run is in flight.
// Both use the same completion gate. Caller holds i.mu.
func (i *Instance) runGateActiveLocked() bool {
	return i.taskRunActive || i.backlogRunActive
}

func runGateActiveInData(data InstanceData) bool {
	return data.TaskRunActive || (data.BacklogRunActive && data.BacklogItem != 0)
}

// endRunsLocked ends the run in flight on its own edge. Caller holds i.mu.
func (i *Instance) endRunsLocked() {
	if i.backlogRunActive {
		i.backlogRunEndedAt = time.Now()
	}
	i.taskRunActive = false
	i.backlogRunActive = false
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}

// BacklogRunEndedAt is when this item session's latest backlog run ended;
// zero while a run is in flight or before the first ends. Pane churn after it
// means someone worked in the session since.
func (i *Instance) BacklogRunEndedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.backlogRunEndedAt
}

// BacklogRunActive reports whether the backlog run on this item session is in
// flight: armed by a backlog delivery and not yet ended by an idle edge or a
// teardown.
func (i *Instance) BacklogRunActive() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.backlogRunActive
}

// BacklogRunGeneration is how many backlog runs have been armed on this session
// since the daemon loaded it. A completion records the generation of the run it
// ended and applies only while that is still the latest.
func (i *Instance) BacklogRunGeneration() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.backlogRunGen
}

// ArmBacklogRun opens a backlog run at the moment the backlog delivers a prompt.
// It sets the run's prompt boundary and holds the turn gate, so the session's
// next idle observation ends the run only after the agent has taken the turn.
// It reports false for a session that is not an item session.
func (i *Instance) ArmBacklogRun(at time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.BacklogItem == 0 || i.taskRunActive {
		return false
	}
	if !i.backlogRunActive {
		// Kept so a rollback restores the previous run's completion evidence
		// (the archive guard compares pane churn against it).
		i.backlogRunEndedAtBeforeArm = i.backlogRunEndedAt
	}
	i.backlogRunActive = true
	i.backlogRunGen++
	i.backlogRunEndedAt = time.Time{}
	i.taskRunPromptAttemptAt = at
	i.taskRunTurnObservedAt = time.Time{}
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = true
	i.stateEpoch++
	i.touchLocked()
	return true
}

// DisarmBacklogRun closes a backlog run whose prompt was not delivered.
func (i *Instance) DisarmBacklogRun() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.backlogRunActive {
		return false
	}
	i.backlogRunActive = false
	// A run that never started leaves the previous run's end as it was: arming
	// cleared it, and without it any pane churn since reads as "the session
	// changed after its run ended" and blocks the on_done archive for good.
	i.backlogRunEndedAt = i.backlogRunEndedAtBeforeArm
	i.taskRunPromptAttemptAt = time.Time{}
	i.taskRunTurnObservedAt = time.Time{}
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	i.touchLocked()
	return true
}

// ReconcileBacklogRunState mirrors the daemon's backlog run marker onto a client
// row, like ReconcileTaskRunState.
func (i *Instance) ReconcileBacklogRunState(active bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.backlogRunActive == active {
		return false
	}
	i.backlogRunActive = active
	i.touchLocked()
	return true
}

// RunGateActive reports whether a task run or a backlog run is in flight, so
// turn evidence is worth recording.
func (i *Instance) RunGateActive() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runGateActiveLocked()
}

// EndBacklogRunForTest ends the backlog run as its idle edge would.
func (i *Instance) EndBacklogRunForTest() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.endRunsLocked()
}
