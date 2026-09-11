package session

import "time"

// TaskRunIdentity is the durable association between a task delivery and the
// session runtime that received it. RunAt is zero only for records written by a
// binary that predates explicit run identity.
type TaskRunIdentity struct {
	TaskID    string
	SessionID string
	Title     string
	RunAt     time.Time
	CreatedAt time.Time
}

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
//
// OpRestoring is a no-prompt-replay contract. Any future recovery that chooses
// to deliver the prompt again must remain under a delivery fence such as
// OpRespawning, or introduce a distinct state; it must not cross this boundary
// and hand an already-closed run to the prompted replacement.
func (i *Instance) InterruptTaskRunAtRestoreBoundary() (TaskRunIdentity, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRestoring || !i.taskRunActive {
		return TaskRunIdentity{}, false
	}
	i.closeTaskRunLocked()
	return TaskRunIdentity{
		TaskID: i.TaskID, SessionID: i.ID, Title: i.Title,
		RunAt: i.taskRunAt, CreatedAt: i.CreatedAt,
	}, true
}

// closeTaskRunLocked performs the shared, one-way task-run close and captures
// the adoption baseline in the same critical section. Caller holds i.mu.
func (i *Instance) closeTaskRunLocked() {
	i.taskRunActive = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}
