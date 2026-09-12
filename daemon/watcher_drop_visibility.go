package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// persistWatcherStatus records a watcher lifecycle status on the task:
// "stopped", or "errored: <exit>: <first output line>" from the crash-loop
// breaker (#797). LastRunAt is preserved — it tracks event deliveries, not
// supervision changes. Passing nil for lastRunAt tells UpdateTaskStatus to
// leave LastRunAt untouched: reading it here (outside the file lock) and
// writing it back would revert a newer timestamp a concurrent deliverWatchEvent
// committed in the gap — the TOCTOU race in #1215. UpdateTaskStatus skips
// Program enum validation so legacy task records still receive status bumps
// (#664).
func persistWatcherStatus(taskID, status string) {
	if _, err := task.UpdateTaskStatus(taskID, nil, status); err != nil {
		log.WarningLog.Printf("failed to record watcher status %q on task %s: %v", status, taskID, err)
	}
}

// persistTerminalStatus publishes the terminal outcome to live readers before
// writing it to the task store. A terminal watcher deliberately remains in the
// supervisor map until an explicit re-arm; without this latch, its older drop
// timestamp would overwrite the newer stopped/errored status on every list.
func (w *taskWatcher) persistTerminalStatus(status string) {
	w.mu.Lock()
	w.terminalStatus = status
	w.mu.Unlock()
	w.persistSupervisorStatus(status)
}

// persistDroppedEvents checkpoints an absolute counter rather than one delta
// per line. The first drop after a successful delivery and then at most one per
// log window touch tasks.json, so the visibility fix cannot turn consecutive
// excess lines from a chatty source into an unbounded write amplifier. ListTasks
// overlays the exact live count; stop flushes the final checkpoint so the disk
// fallback sees the same total after a clean shutdown.
func (w *taskWatcher) persistDroppedEvents(total int, droppedAt time.Time) {
	if err := w.sup.recordDrops(w.taskID, total, droppedAt); err != nil {
		log.WarningLog.Printf("watch task %s: failed to record %d dropped events: %v", w.taskID, total, err)
		return
	}
	w.mu.Lock()
	if total > w.persistedDrops {
		w.persistedDrops = total
	}
	w.mu.Unlock()
}

func (w *taskWatcher) flushDroppedEvents() {
	w.mu.Lock()
	dropped, persisted, droppedAt := w.dropped, w.persistedDrops, w.lastDroppedAt
	w.mu.Unlock()
	if dropped > persisted {
		w.persistDroppedEvents(dropped, droppedAt)
	}
}

// applyLiveDropState overlays the exact count held by the watcher when it
// exceeds the latest durable checkpoint. A daemon-backed list therefore never
// waits up to the one-minute checkpoint cadence to reveal loss. The status
// follows the latest observed outcome: a later successful delivery wins.
func (s *watcherSupervisor) applyLiveDropState(t *task.Task) {
	s.mu.Lock()
	w := s.watchers[t.ID]
	s.mu.Unlock()
	if w == nil {
		return
	}
	w.mu.Lock()
	dropped, droppedAt, deliveredAt, terminalStatus :=
		w.dropped, w.lastDroppedAt, w.lastDeliveredAt, w.terminalStatus
	w.mu.Unlock()
	if dropped > t.DroppedEvents {
		t.DroppedEvents = dropped
	}
	if terminalStatus != "" {
		t.LastRunStatus = terminalStatus
		return
	}
	if !droppedAt.IsZero() && !deliveredAt.After(droppedAt) &&
		(t.LastRunAt == nil || !droppedAt.Before(*t.LastRunAt)) {
		t.LastRunStatus = task.WatchRateDropStatus
	}
}

func persistWatcherDrops(taskID string, total int, droppedAt time.Time) error {
	_, err := task.RecordWatchRateDrops(taskID, total, droppedAt)
	return err
}
