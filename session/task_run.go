package session

import "time"

// TaskRunIdentity is the durable association between a task delivery and the
// session runtime that received it. SessionID is the unique identity; Sequence
// is its clock-independent delivery order; RunAt is its display timestamp and
// is zero only for records written before task-run publication moved inside the
// manager boundary.
type TaskRunIdentity struct {
	TaskID    string
	SessionID string
	Title     string
	Sequence  uint64
	RunAt     time.Time
	CreatedAt time.Time
}

// TaskRun returns this session's immutable task-delivery identity. The active
// bit is deliberately separate: a completed session still retains the identity
// that ordered its task-row writes.
func (i *Instance) TaskRun() TaskRunIdentity {
	return TaskRunIdentity{
		TaskID: i.TaskID, SessionID: i.ID, Title: i.Title,
		Sequence: i.taskRunSequence, RunAt: i.taskRunAt, CreatedAt: i.CreatedAt,
	}
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
	return i.TaskRun(), true
}

// closeTaskRunLocked performs the shared, one-way task-run close and captures
// the adoption baseline in the same critical section. Caller holds i.mu.
func (i *Instance) closeTaskRunLocked() {
	i.taskRunActive = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}
