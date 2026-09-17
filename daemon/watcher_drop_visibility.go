package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// persistWatcherStatus records a watcher lifecycle status on the task:
// "stopped", or "errored: <exit>: <first output line>" from the crash-loop
// breaker (#797). LastRunAt is preserved — it tracks event deliveries, not
// supervision changes. Passing nil for lastRunAt tells
// UpdateTaskStatusForGeneration to leave LastRunAt untouched: reading it here
// (outside the file lock) and writing it back would revert a newer timestamp a
// concurrent deliverWatchEvent committed in the gap — the TOCTOU race in #1215.
// The writer skips Program enum validation so legacy task records still receive
// status bumps (#664).
func persistWatcherStatus(taskID, taskGenerationID, status string) {
	if _, _, err := task.UpdateTaskStatusForGeneration(taskID, taskGenerationID, nil, status); err != nil {
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
	w.sup.setStatus(w.taskID, w.generationID, status)
}

// persistDroppedEvents checkpoints an absolute counter rather than one delta
// per line. The first drop after a successful delivery and then at most one per
// log window touch tasks.json, so the visibility fix cannot turn consecutive
// excess lines from a chatty source into an unbounded write amplifier. ListTasks
// overlays the exact live count; stop flushes the final checkpoint so the disk
// fallback sees the same total after a clean shutdown.
func (w *taskWatcher) persistDroppedEvents(total int, droppedAt time.Time) {
	if err := w.sup.recordDrops(w.taskID, w.generationID, total, droppedAt); err != nil {
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
	// A stale watcher still in the map holds ITS generation's evidence: the
	// overlay must not transfer its drops, drop status, or terminal status
	// onto the replacement incarnation listed under the reused ID (#4224
	// review).
	if w == nil || w.generationID != t.GenerationID {
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

func persistWatcherDrops(taskID, generationID string, total int, droppedAt time.Time) error {
	_, _, err := task.RecordWatchRateDropsForGeneration(taskID, generationID, total, droppedAt)
	return err
}

// resetWatcherDrops is the durable half of a generation rebound: the local
// copy's zeroed seed only reaches the watcher, so the row under the reused ID
// must drop the predecessor's count too — otherwise the replacement's first
// checkpoints read as stale and the next restart reseeds from them (#4224
// review). A failure leaves the in-memory fix in place and only repeats the
// pre-fix behavior on disk, so it is logged, never fatal.
func resetWatcherDrops(taskID, generationID string) error {
	_, _, err := task.ResetWatchRateDropsForGeneration(taskID, generationID)
	return err
}

// clearReboundDropSeed persists the drop reset a generation rebound applies to
// its in-memory copy of the task. The reset is generation-gated inside the
// store, so a row rebound again since the caller's snapshot is a clean refusal.
func (s *watcherSupervisor) clearReboundDropSeed(t task.Task) {
	if err := s.resetDrops(t.ID, t.GenerationID); err != nil {
		log.WarningLog.Printf("watch task %s: failed to clear inherited drop count: %v", t.ID, err)
	}
}
