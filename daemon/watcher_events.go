package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// The watcher event pipeline: how one stdout line becomes a delivered event
// or a durably queued one. handleEvent routes each line — direct delivery
// while the backlog is empty, durable enqueue once a backlog exists (FIFO,
// #1129) — enqueueEvent appends and wakes the drainer (watcher_drain.go), and
// deliverWatchEventWithOptions is the production delivery hook the
// supervisor's deliver func points at: it reloads the task per event so
// prompt/target_session edits apply without restarting the script.
// handleEvent routes one stdout line: with a backlog pending it is queued
// behind it (delivering directly would reorder the stream, #1129); otherwise
// it is rate-limited and delivered synchronously exactly as before, with a
// failed delivery queued for replay instead of dropped. Called from the single
// reader goroutine, so direct deliveries stay serialized in order. Lines that
// do not become a delivered event this run — rate-dropped, failed, or queued —
// go to the failure tail (#797).
func (w *taskWatcher) handleEvent(line string, tail *tailBuffer) {
	// FIFO gating: once a backlog exists, every newer event goes through the
	// queue until it drains. Backlogged enqueues bypass the rate limiter —
	// the limiter gates deliveries to the target (the drainer reserves a slot
	// per replayed event); the queue's own count/byte caps bound the backlog.
	// A live event is a real recovery point (#3242): the FRESH count heals a
	// failed load the moment storage recovers, restoring FIFO immediately; if
	// it stays unreadable this reads 0 and falls through to direct delivery ON
	// PURPOSE — enqueue refuses unknown state, so queue-routing would only
	// drop the event, and delivering beats FIFO. Cost: ordering, not loss.
	if w.queue != nil && w.queue.pendingCountFresh() > 0 {
		limitParked := w.targetLimitRequiresRetention()
		w.enqueueEvent(line, tail, limitParked)
		return
	}

	if !w.tryReserveEventSlot() {
		// A rate-full window is normally permission to drop a chatty source's
		// newest event. A targeted session already at a usage limit is different:
		// this distinct event was never attempted and must establish protected
		// backlog before the ordinary rate policy can consume it. The observer is
		// fail-closed and ordered against in-flight limit snapshots. Without a
		// queue there is nowhere to retain it, so fall through to the visible drop
		// counter and warning instead of silently claiming it was preserved.
		if w.queue != nil && w.targetLimitRequiresRetention() {
			w.enqueueEvent(line, tail, true)
			return
		}
		// One warning per window, not per drop — a flooding script must not
		// also flood the daemon log. The counter keeps the exact total.
		// Rate-dropped events are deliberately NOT queued: the limiter is
		// protective policy against a chatty script, not an outage signal.
		if dropped, logIt := w.recordEventDrop(); logIt {
			log.WarningLog.Printf("watch task %s: event rate exceeded %d/min; dropping excess events (%d dropped so far)", w.taskID, w.sup.eventsPerMinute, dropped)
		}
		tail.add(line)
		return
	}

	err := w.sup.deliver(w.taskID, line, watchDeliveryOptions{})
	w.recordDeliveryResult(time.Now(), err)
	if err != nil {
		limitParked := false
		parkedStatusRecorded := false
		switch {
		case errors.Is(err, errTargetLimitReached):
			// The target is at a known usage-limit wall. Preserve the event and
			// refund its unused rate slot, but do not raise a delivery-failure
			// alarm: the pipeline is intentionally parked until liveness clears.
			limitParked = true
			parkedStatusRecorded = watchParkedStatusRecorded(err)
			w.releaseEventSlot()
			log.InfoLog.Printf("watch task %s: target session is at a usage limit; deferring event until the limit clears", w.taskID)
		case errors.Is(err, errTargetBusy):
			// Not a failure: a TUI is attached to the target, so the event is
			// held and retried after detach rather than pasted into live typing
			// (#1586). A deferral delivers nothing, so refund the rate slot it
			// reserved above — otherwise the live attempt AND the drainer's replay
			// would each spend one, double-charging the target's per-minute budget
			// and eventually dropping events. Queue it (preserving FIFO) and log
			// quietly.
			w.releaseEventSlot()
			log.InfoLog.Printf("watch task %s: target session attached; deferring event until detach (#1586)", w.taskID)
		case errors.Is(err, errAtConcurrencyLimit):
			// Not a failure either: the task is at its max_concurrent_runs cap
			// (#1892), so nothing was created. Refund the rate slot for the same
			// reason a deferral does — the event will be delivered by the drainer,
			// which spends its own slot then, and burning one per refusal would
			// charge the cap's whole parked period against the per-minute budget.
			// Queueing it preserves FIFO and is what makes the cap queue rather than
			// drop; the drainer retries until a session finishes.
			w.releaseEventSlot()
			log.InfoLog.Printf("watch task %s: at its max_concurrent_runs limit; queueing event until a session finishes (#1892)", w.taskID)
		default:
			// A genuine failure that never got as far as the target delivered
			// nothing either, so it refunds too (#2102) — otherwise the live
			// attempt AND every drainer retry each spend a slot, leaking the
			// per-minute budget exactly when an outage needs it for recovery. Only
			// pre-flight failures qualify: a failed create or send may have landed
			// (see errNotAttempted), and a delivered event must stay charged.
			if errors.Is(err, errNotAttempted) {
				w.releaseEventSlot()
			}
			log.ErrorLog.Printf("watch task %s: failed to deliver event: %v", w.taskID, err)
		}
		w.enqueueEvent(line, tail, limitParked, parkedStatusRecorded)
	}
}

// enqueueEvent appends the line to the durable backlog and wakes the drainer.
// The line also lands in the run's failure tail — it did not become a
// delivered event this run (#797). With no durable queue, or when the append
// fails WITHOUT retaining the record, the event is neither delivered nor held
// and is counted through recordEventDrop, whatever delivery outcome sent it
// here — a usage-limit park, an attached target, a concurrency park, or a
// genuine failure. An error is not proof of loss: a close/flush or cap fault
// can follow a landed record, so retention decides.
func (w *taskWatcher) enqueueEvent(line string, tail *tailBuffer, limitParked bool, parkedStatusRecorded ...bool) {
	tail.add(line)
	if w.queue == nil {
		if dropped, logIt := w.recordEventDrop(); logIt {
			log.WarningLog.Printf("watch task %s: undelivered event cannot be retained; durable event queue unavailable (%d dropped so far)", w.taskID, dropped)
		}
		return
	}
	statusRecorded := len(parkedStatusRecorded) > 0 && parkedStatusRecorded[0]
	retained, err := w.queue.enqueueWithParkedStatus(line, limitParked, statusRecorded)
	if err != nil {
		if retained {
			// The record is in the backlog despite the degraded write; it will
			// replay, so it is no loss — but the drainer still needs waking.
			log.ErrorLog.Printf("watch task %s: queued event for replay, but the queue reported a degraded append: %v", w.taskID, err)
			w.ensureDrainer()
			return
		}
		log.ErrorLog.Printf("watch task %s: failed to queue event for replay; dropping it: %v", w.taskID, err)
		w.recordEventDrop()
		return
	}
	w.ensureDrainer()
}

// ensureDrainer starts the drain goroutine unless one is live or a stop is in
// flight. Callers: enqueueEvent (an event just queued) and run (a backlog
// recovered from disk).
func (w *taskWatcher) ensureDrainer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.draining || w.stopRequested() {
		return
	}
	w.draining = true
	w.wg.Add(1)
	go w.drainLoop()
}

// stopDraining clears the draining flag so a later enqueue can start a fresh
// drainer.
func (w *taskWatcher) stopDraining() {
	w.mu.Lock()
	w.draining = false
	w.mu.Unlock()
}

// deliverWatchEvent is the queue-less entry used by focused delivery tests.
// Production supplies its queue-head identity through the options-aware half.
func deliverWatchEvent(taskID, line string) error {
	return deliverWatchEventWithOptions(taskID, line, watchDeliveryOptions{})
}

// deliverWatchEventWithOptions is the production delivery hook: it re-loads the
// task (so prompt/target_session edits apply without restarting the script),
// renders {{line}}, routes through the same delivery path cron fires use, and
// records the run status (#664 path).
func deliverWatchEventWithOptions(taskID, line string, options watchDeliveryOptions) error {
	// The three pre-flight checks below fail before anything is created or sent,
	// so they are tagged notAttempted and the caller refunds their rate slot
	// (#2102). Everything past them can fail with the delivery already in
	// flight, and stays charged.
	t, err := task.GetTask(taskID)
	if err != nil {
		return notAttempted(fmt.Errorf("failed to load task: %w", err))
	}
	if !t.Enabled {
		return notAttempted(fmt.Errorf("task %s is disabled", taskID))
	}
	prompt := task.RenderWatchPrompt(t.Prompt, line)
	if strings.TrimSpace(prompt) == "" {
		return notAttempted(fmt.Errorf("event rendered an empty prompt (line %q)", line))
	}
	status, promptRetained, err := deliverTaskPromptOutcome(t, prompt, true)
	if err != nil {
		return err
	}
	if status == StatusDeferredAttached {
		// A TUI is attached full-screen to the target session; the delivery was
		// held so it can't paste into and submit the user's in-progress input
		// (#1586). Signal the caller (handleEvent / drainLoop) to re-queue and
		// retry after detach — the event is neither lost nor delivered into live
		// typing. errTargetBusy is exempt from the delivery-failure alarm and
		// logged quietly, since a deferral is expected, not an outage.
		return errTargetBusy
	}
	// A parked replay retries the same queue head on the base cadence. The queue
	// sequence, not mutable LastRunStatus presentation, says whether this exact
	// occurrence was already recorded. A later success still writes normally.
	// A live targeted park has no sequence until handleEvent durably enqueues it;
	// writing the visible status here would leave a false park if that append
	// fails. Defer only that shape: the drainer immediately retries the queued
	// head with a cursor and publishes through commitParkedStatus. Create-per-run
	// parks retain their prompt on the created session and still write here.
	statusRecorded := status == TaskStatusLimitParked && options.parkedStatusRecorded
	deferParkedStatus := status == TaskStatusLimitParked && !promptRetained && options.commitParkedStatus == nil
	if !deferParkedStatus && (status != TaskStatusLimitParked || !statusRecorded) {
		now := time.Now()
		writeStatus := func() error {
			_, err := updateWatchTaskStatus(taskID, &now, status)
			return err
		}
		var err error
		if status == TaskStatusLimitParked && options.commitParkedStatus != nil {
			statusRecorded, err = options.commitParkedStatus(writeStatus)
		} else {
			err = writeStatus()
			statusRecorded = status == TaskStatusLimitParked && err == nil
		}
		if err != nil {
			log.ErrorLog.Printf("failed to update task status: %v", err)
		}
	}
	if status == TaskStatusLimitParked && !promptRetained {
		// A targeted watch owns distinct external data, so its queue must replay.
		// A create-per-run watch already stored this prompt on the one parked
		// session; queueing it too would create duplicate sessions on every retry.
		return &watchTargetLimitError{statusRecorded: statusRecorded}
	}
	return nil
}
