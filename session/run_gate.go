package session

import "time"

// runGateActiveLocked reports whether a task run is in flight. Caller holds
// i.mu.
func (i *Instance) runGateActiveLocked() bool {
	return i.taskRunActive
}

func runGateActiveInData(data InstanceData) bool {
	return data.TaskRunActive
}

// endRunsLocked ends the run in flight on its own edge. Caller holds i.mu.
func (i *Instance) endRunsLocked() {
	i.taskRunActive = false
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}

// RunGateActive reports whether a task run is in flight, so turn evidence is
// worth recording.
func (i *Instance) RunGateActive() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runGateActiveLocked()
}

// LastActivityAt is the latest pane output, prompt sent, or runtime
// replacement this process saw; zero when there is none.
func (i *Instance) LastActivityAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	last := i.lastPaneChurnAt
	for _, t := range []time.Time{i.lastPromptAttemptAt, i.runtimeReplacedAt} {
		if t.After(last) {
			last = t
		}
	}
	return last
}
