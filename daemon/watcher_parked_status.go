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
	// admitted only for replay with a durable cursor; live delivery publishes
	// synchronously on the reader path before runOnce can report process exit.
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
	return w.sup.deliver(w.taskID, ev.Line, options)
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
	return true, nil
}
