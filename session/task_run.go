package session

// InterruptTaskRunAtRestoreBoundary closes the run owned by a Lost runtime just
// before its replacement becomes visible. The returned identity is valid only
// when interrupted is true.
//
// This is the durable pre-ConfirmLive half of runEndsOnRestoredRuntime. The live
// boundary callback must persist the instance before ConfirmLive drops the
// restore fence; clearing here puts the interrupted outcome in that settlement,
// so a daemon crash before the ordinary post-recovery write cannot reload the
// predecessor's run as active. ConfirmLive retains the same run effect as a
// structural fallback for callers without a settlement callback.
func (i *Instance) InterruptTaskRunAtRestoreBoundary() (taskID, title string, interrupted bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRestoring || !i.taskRunActive {
		return "", "", false
	}
	i.closeTaskRunLocked()
	return i.TaskID, i.Title, true
}

// closeTaskRunLocked performs the shared, one-way task-run close and captures
// the adoption baseline in the same critical section. Caller holds i.mu.
func (i *Instance) closeTaskRunLocked() {
	i.taskRunActive = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}
