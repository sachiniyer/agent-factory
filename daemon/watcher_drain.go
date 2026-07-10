package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// sleepStopAware waits d, or returns false immediately when a stop arrives:
// the drainer must never be the thing a stop/reload waits on (the same
// stop-awareness discipline as the run loop's backoff waits).
func (w *taskWatcher) sleepStopAware(d time.Duration) bool {
	select {
	case <-w.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

// drainLoop replays the backlog oldest-first through the same deliver hook as
// live events. Strictly FIFO: live events enqueue behind the backlog (see
// handleEvent), so emission order survives an outage. Every wait is
// stop-aware; a failed replay backs off from drainBaseBackoff to
// drainMaxBackoff and then holds that cadence for as long as the failure
// persists; never a
// permanent give-up (#1128 discipline), because an outage is indistinguishable
// from a broken target while it lasts. A corrupt queue parks the replay with
// an ERROR rather than guessing at record boundaries.
func (w *taskWatcher) drainLoop() {
	defer w.wg.Done()
	backoff := w.sup.drainBaseBackoff
	replayed, expired := 0, 0
	for {
		if w.stopRequested() {
			w.stopDraining()
			return
		}
		ev, cursor, ok, err := w.queue.peek()
		if err != nil {
			log.ErrorLog.Printf("watch task %s: cannot read queued event; replay parked until the next reload: %v", w.taskID, err)
			w.stopDraining()
			return
		}
		if ok && w.sup.queueMaxAge > 0 && time.Since(ev.TS) > w.sup.queueMaxAge {
			// Retention (#1129): an event older than the age bound is expired
			// instead of delivered — a prompt about a days-old notification is
			// noise, and re-sweepable sources re-emit on their next poll.
			// Counted and logged below, never silent.
			advanced, err := w.queue.advance(cursor)
			if err != nil {
				log.ErrorLog.Printf("watch task %s: failed to expire aged queued event; replay parked until the next reload: %v", w.taskID, err)
				w.stopDraining()
				return
			}
			if !advanced {
				continue
			}
			expired++
			continue
		}
		if !ok {
			if replayed > 0 || expired > 0 {
				log.InfoLog.Printf("watch task %s: replayed %d queued event(s), expired %d older than %s", w.taskID, replayed, expired, w.sup.queueMaxAge)
			}
			w.stopDraining()
			// Close the wake-up race: an event enqueued after the empty peek
			// but before draining cleared would otherwise strand until the
			// next enqueue. Our wg slot is still held, so the re-spawn's Add
			// can never race a stop's Wait.
			if w.queue.pendingCount() > 0 {
				w.ensureDrainer()
			}
			return
		}
		if !w.tryReserveEventSlot() {
			// The shared rate window is full (live deliveries count too);
			// wait for it to roll, stop-aware.
			if !w.sleepStopAware(w.sup.drainBaseBackoff) {
				w.stopDraining()
				return
			}
			continue
		}
		if err := w.sup.deliver(w.taskID, ev.Line); err != nil {
			w.recordDeliveryResult(time.Now(), err)
			if errors.Is(err, errTargetBusy) {
				// Not a delivery failure: a TUI is attached to the target, so the
				// backlog is held until detach rather than pasted into live typing
				// (#1586). A deferral sends nothing, so refund the rate slot this
				// attempt reserved above — otherwise every retry during the attach
				// would burn the target's per-minute budget and could starve real
				// deliveries once the user detaches. Poll at the base cadence (never
				// the growing failure backoff) so delivery resumes promptly on
				// detach, and log quietly since a deferral is expected, not an outage.
				w.releaseEventSlot()
				log.InfoLog.Printf("watch task %s: target session attached; holding %d queued event(s) until detach (#1586)", w.taskID, w.queue.pendingCount())
				if !w.sleepStopAware(w.sup.drainBaseBackoff) {
					w.stopDraining()
					return
				}
				continue
			}
			log.WarningLog.Printf("watch task %s: replay delivery failed (%d event(s) pending); retrying in %s: %v", w.taskID, w.queue.pendingCount(), backoff, err)
			if !w.sleepStopAware(backoff) {
				w.stopDraining()
				return
			}
			backoff *= 2
			if backoff > w.sup.drainMaxBackoff {
				backoff = w.sup.drainMaxBackoff
			}
			continue
		}
		w.recordDeliveryResult(time.Now(), nil)
		advanced, err := w.queue.advance(cursor)
		if err != nil {
			log.ErrorLog.Printf("watch task %s: failed to advance the event queue; replay parked until the next reload: %v", w.taskID, err)
			w.stopDraining()
			return
		}
		if !advanced {
			continue
		}
		replayed++
		backoff = w.sup.drainBaseBackoff
	}
}
