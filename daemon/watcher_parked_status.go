package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// updateWatchTaskStatus is the one task-store write seam used by watch event
// outcomes and watcher lifecycle status. Tests count calls here to prove a held
// queue head causes no full-store rewrite on retry.
var updateWatchTaskStatus = task.UpdateTaskStatus

// watchDeliveryOptions carries queue-owned occurrence state into the delivery
// hook. Only replay has a cursor to identify; live delivery passes the zero
// value and binds a successful parked write to the event when it enqueues it.
type watchDeliveryOptions struct {
	parkedStatusRecorded bool
	// commitParkedStatus makes the task-store write and queue-head annotation
	// one publication against supervisor lifecycle status. The callback is
	// admitted only for replay with a durable cursor. A live targeted park has
	// no queue identity yet, so it defers the task-store write until enqueue
	// succeeds and the drainer supplies this callback on its immediate replay.
	commitParkedStatus func(writeStatus func() error) (bool, error)
}

// watchTargetLimitError preserves the ordinary sentinel contract while telling
// the queue whether this exact occurrence's task status reached durable store.
// A failed status write stays unrecorded so a later retry can try again.
type watchTargetLimitError struct {
	statusRecorded bool
}

func (e *watchTargetLimitError) Error() string { return errTargetLimitReached.Error() }
func (e *watchTargetLimitError) Unwrap() error { return errTargetLimitReached }

func watchParkedStatusRecorded(err error) bool {
	var parked *watchTargetLimitError
	return errors.As(err, &parked) && parked.statusRecorded
}

// deliverQueuedEvent supplies the durable queue-head identity and its atomic
// publication callback to delivery.
func (w *taskWatcher) deliverQueuedEvent(ev queuedEvent, cursor eventQueueCursor) error {
	options := watchDeliveryOptions{
		parkedStatusRecorded: w.queue.parkedStatusRecorded(cursor),
		commitParkedStatus: func(writeStatus func() error) (bool, error) {
			return w.commitParkedStatus(cursor, writeStatus)
		},
	}
	if options.parkedStatusRecorded {
		// The head's parked occurrence is already durable, so its retry skips
		// commitParkedStatus — the only place a terminal publication committed
		// while the queue was unverifiable can be reconciled. The recorded head
		// resuming IS that reconcile: republish the parked status and retire
		// the latch under the same publication mutex so listings stop hiding
		// the actionable park and its later replay outcome (#4226 review).
		w.reconcileRecordedParkedHead(cursor)
	}
	return w.sup.deliver(w.taskID, ev.Line, options)
}

// reconcileRecordedParkedHead undoes the terminal publication a watcher exit
// committed while the queue could not confirm its recorded parked head: both
// halves of it — the durable row setStatus wrote and the terminalStatus
// overlay applyLiveDropState prefers over it — stay stale while retries keep
// skipping commitParkedStatus, and a later "sent" writes under the same
// overlay. Re-running the recorded head's publication restores the parked row
// and retires the latch in one statusMu transaction.
//
// The reconcile is owed only when something actually overwrote the park —
// the durable row and the terminalStatus overlay are the evidence, read under
// statusMu so the check cannot interleave with the publication it checks for.
// An ordinary recorded head whose row still reads parked owes nothing, so a
// restarted watcher's first resume does not rewrite the store, and a failed
// republish simply leaves the row non-parked so the next resume retries
// (#4226 review).
func (w *taskWatcher) reconcileRecordedParkedHead(cursor eventQueueCursor) {
	if !w.parkedStatusOverwritten() {
		return
	}
	if _, err := w.commitParkedStatus(cursor, func() error {
		// nil preserves the occurrence's original LastRunAt — this is a
		// republish of the parked outcome, not a new run.
		_, err := updateWatchTaskStatus(w.taskID, nil, TaskStatusLimitParked)
		return err
	}); err != nil {
		log.WarningLog.Printf("watch task %s: could not republish the recorded park over a stale terminal status: %v", w.taskID, err)
	}
}

// parkedStatusOverwritten reports whether a terminal publication replaced the
// recorded park's visible status — the terminalStatus overlay is latched, or
// the durable row no longer reads parked. A terminal publication commits both
// halves under statusMu, so reading under the same mutex keeps the check
// atomic with respect to it. An unreadable store cannot prove the row still
// reads parked, so it counts as overwritten and the republish is attempted.
func (w *taskWatcher) parkedStatusOverwritten() bool {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	w.mu.Lock()
	overlaid := w.terminalStatus != ""
	w.mu.Unlock()
	if overlaid {
		return true
	}
	stored, err := task.GetTask(w.taskID)
	if err != nil {
		return true
	}
	return stored.LastRunStatus != TaskStatusLimitParked
}

// commitParkedStatus publishes the user-visible parked occurrence and the
// queue-head identity that suppresses retries as one transaction with respect
// to supervisor status. The durable stores remain individually crash-safe; the
// mutex closes only the in-process interleaving where "stopped" or "errored"
// could land between them and stay visible for the entire parked episode.
func (w *taskWatcher) commitParkedStatus(cursor eventQueueCursor, writeStatus func() error) (bool, error) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if err := w.queue.markLimitParked(); err != nil {
		log.ErrorLog.Printf("failed to prepare parked watch status: %v", err)
	}
	if err := writeStatus(); err != nil {
		return false, err
	}
	if _, err := w.queue.recordParkedStatus(cursor); err != nil {
		log.ErrorLog.Printf("watch task %s: failed to record parked queue-head identity: %v", w.taskID, err)
	}
	// A terminal report may have committed before this park acquired statusMu.
	// The parked outcome is now newer on disk, so retire the in-memory terminal
	// overlay in the same publication or ListTasks would keep hiding it.
	w.mu.Lock()
	w.terminalStatus = ""
	w.mu.Unlock()
	return true, nil
}
