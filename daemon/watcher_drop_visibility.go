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

// tryReserveEventSlot applies the per-task delivery rate limit: prune the
// sliding window, then reserve one slot if the window has room. Live deliveries
// and drainer replays share the window, so their combined pressure never
// exceeds eventsPerMinute.
//
// This rate limit and the max_concurrent_runs cap (#1892) are orthogonal. This
// one drops excess events from a chatty source; the cap queues them. Once the
// first cap refusal creates a backlog, handleEvent's FIFO gate queues every
// later event without consulting this limiter, so they cannot both be the
// binding constraint.
func (w *taskWatcher) tryReserveEventSlot() bool {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	cut := 0
	for cut < len(w.eventTimes) && now.Sub(w.eventTimes[cut]) >= time.Minute {
		cut++
	}
	w.eventTimes = w.eventTimes[cut:]
	if len(w.eventTimes) >= w.sup.eventsPerMinute {
		return false
	}
	w.eventTimes = append(w.eventTimes, now)
	return true
}

// releaseEventSlot refunds a rate slot when a deferral or a pre-flight failure
// delivered nothing. The live path and drainer share the window, but it counts
// reservations rather than identity, so removing one newest timestamp per
// refund keeps the total exact regardless of which goroutine reserved it.
func (w *taskWatcher) releaseEventSlot() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.eventTimes); n > 0 {
		w.eventTimes = w.eventTimes[:n-1]
	}
}

// persistTerminalStatus publishes a terminal outcome to both live readers and
// the task store. The parked-head check and live latch share statusMu with the
// drainer's parked publication: whichever outcome commits last controls both
// surfaces, rather than preserving the task-store park while the in-memory
// overlay independently hides it.
func (w *taskWatcher) persistTerminalStatus(status string) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if w.parkedHeadSupersedesSupervisorStatus(status) {
		return
	}
	w.mu.Lock()
	w.terminalStatus = status
	w.mu.Unlock()
	w.sup.setStatus(w.taskID, status)
}

// persistSupervisorStatus keeps a confirmed usage-limit occurrence as the
// task's visible status while its queue head is still held. The command's stop
// or crash is logged independently; queue replay eventually replaces the park
// on success.
func (w *taskWatcher) persistSupervisorStatus(status string) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if w.parkedHeadSupersedesSupervisorStatus(status) {
		return
	}
	w.sup.setStatus(w.taskID, status)
}

// parkedHeadSupersedesSupervisorStatus is the queue side of statusMu's
// publication. Only POSITIVE evidence of a recorded parked head defers a
// supervisor report: the parked occurrence's durable marker survives a status
// overwrite and is re-derived on recovery, so publishing a real terminal
// outcome over an unverifiable queue loses only presentation in a window the
// queue is already broken — while suppressing it has no recovery path and
// would leave an ordinary backlog showing stale status past a permanently
// stopped watcher. Caller holds statusMu.
func (w *taskWatcher) parkedHeadSupersedesSupervisorStatus(status string) bool {
	if w.queue == nil {
		return false
	}
	recorded, err := w.queue.headParkedStatusRecorded()
	if err != nil {
		log.WarningLog.Printf("watch task %s: cannot verify parked queue-head status; publishing %q rather than hiding a real lifecycle outcome: %v", w.taskID, status, err)
		return false
	}
	return recorded
}

// countEventDrop records one event that will be neither delivered nor durably
// retained and returns the values the caller needs to rate-limit its warning
// and decide whether to checkpoint. Every refusal that loses an event goes
// through this so dropped_events cannot diverge by which branch produced it:
// the rate-full drop, a limit park with no durable queue, and any later loss
// all increment the same counter under the same lock discipline.
func (w *taskWatcher) countEventDrop(now time.Time) (dropped int, outcomeChanged, logIt bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped++
	dropped = w.dropped
	outcomeChanged = w.lastDroppedAt.IsZero() || w.lastDeliveredAt.After(w.lastDroppedAt)
	w.lastDroppedAt = now
	logIt = now.Sub(w.lastDropLog) >= time.Minute
	if logIt {
		w.lastDropLog = now
	}
	return dropped, outcomeChanged, logIt
}

// recordEventDrop is the one accounting exit for an event that will be neither
// delivered nor durably retained: every branch that loses one — rate-full, no
// durable queue, a refused enqueue, a stop drain into an unreadable queue —
// counts it here, so dropped_events cannot miss a branch (#4226 review). It
// checkpoints on the first drop after a delivery and once per log window, and
// reports whether the caller's rate-limited warning is due.
func (w *taskWatcher) recordEventDrop() (dropped int, logIt bool) {
	now := time.Now()
	dropped, outcomeChanged, logIt := w.countEventDrop(now)
	if logIt || outcomeChanged {
		w.persistDroppedEvents(dropped, now)
	}
	return dropped, logIt
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
