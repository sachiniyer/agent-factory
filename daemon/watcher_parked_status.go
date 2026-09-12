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
	prepareParkedStatus  func() error
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

// deliverQueuedEvent supplies the durable queue-head identity to delivery and,
// after a successful task-store write, records that identity beside the queue.
// The preparation marker lands before the task write so a watcher exit cannot
// overwrite the parked presentation in the narrow gap between those writes.
func (w *taskWatcher) deliverQueuedEvent(ev queuedEvent, cursor eventQueueCursor) error {
	options := watchDeliveryOptions{
		parkedStatusRecorded: w.queue.parkedStatusRecorded(cursor),
		prepareParkedStatus:  w.queue.markLimitParked,
	}
	err := w.sup.deliver(w.taskID, ev.Line, options)
	if !errors.Is(err, errTargetLimitReached) || !watchParkedStatusRecorded(err) || options.parkedStatusRecorded {
		return err
	}
	if _, recordErr := w.queue.recordParkedStatus(cursor); recordErr != nil {
		log.ErrorLog.Printf("watch task %s: failed to record parked queue-head identity: %v", w.taskID, recordErr)
	}
	return err
}
