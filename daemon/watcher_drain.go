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

// watcherParkLogInterval bounds how often a drainer repeats an unchanged park
// notice. A park is a HOLD, not a failure: the drainer re-checks it every
// drainBaseBackoff (10s), so a watch task sitting at its cap — or held while a
// TUI stays attached — emitted ~360 identical lines an hour about a state that
// had not changed. That is the #1910 hot-loop shape (465 identical errors), and
// unbounded repetition of a known state is what trains people to scroll past the
// log. One line on entering the park, then a reminder at most this often for as
// long as it holds.
const watcherParkLogInterval = 5 * time.Minute

// parkLogThrottle bounds repeated notices about an unchanged park.
//
// Keyed on the park REASON, not the rendered message: the pending count moves
// while parked, so throttling on message text would let a busy queue defeat the
// throttle entirely. A change of reason logs immediately — at-cap and
// target-attached are different facts, and the transition between them is the
// interesting moment — and reset() re-arms after a delivery lands so the next
// episode is never swallowed by the previous one's window.
type parkLogThrottle struct {
	reason string
	at     time.Time
}

// allow reports whether a park notice for reason should be logged now, recording
// it when so. The first notice of an episode always logs.
func (p *parkLogThrottle) allow(reason string, now time.Time) bool {
	if reason == p.reason && now.Sub(p.at) < watcherParkLogInterval {
		return false
	}
	p.reason, p.at = reason, now
	return true
}

// reset re-arms the throttle: the park ended, so the next one is a new episode.
func (p *parkLogThrottle) reset() { p.reason = "" }

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
	// Park-notice throttle. Local to this goroutine — the drain loop is the only
	// writer, so this needs no lock, and the live path (handleEvent) logs a park at
	// most once per episode anyway: its FIFO gate sends later events straight to
	// the queue without attempting a delivery.
	var parkLog parkLogThrottle
	// Account for the replay on EVERY exit, not just the drained-empty one
	// (#1789). Expiring advances the cursor irreversibly — the record is gone
	// and no later session can report it — so a stop landing in one of the
	// stop-aware waits, or a parked queue error, must still say what was
	// consumed. Deferred after wg.Done's registration, so it runs before it:
	// a stop's Wait cannot return until the summary is out.
	defer func() {
		if replayed > 0 || expired > 0 {
			log.InfoLog.Printf("watch task %s: replayed %d queued event(s), expired %d older than %s", w.taskID, replayed, expired, w.sup.queueMaxAge)
		}
	}()
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
			// Counted here and logged by the exit summary above, never silent.
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
			if errors.Is(err, errAtConcurrencyLimit) {
				// The task is at its max_concurrent_runs cap (#1892): nothing was
				// created and nothing is wrong. Refund the rate slot this attempt
				// reserved (a refusal delivers nothing, and a long park would
				// otherwise burn the whole per-minute budget on retries), then poll at
				// the BASE cadence rather than the growing failure backoff so the
				// backlog starts moving promptly once a session finishes. The head
				// event stays at the head, so FIFO holds across the park.
				w.releaseEventSlot()
				if parkLog.allow("cap", time.Now()) {
					log.InfoLog.Printf("watch task %s: at its max_concurrent_runs limit; holding %d queued event(s) until a session finishes — repeats at most every %s while this holds (#1892)", w.taskID, w.queue.pendingCount(), watcherParkLogInterval)
				}
				if !w.sleepStopAware(w.sup.drainBaseBackoff) {
					w.stopDraining()
					return
				}
				continue
			}
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
				if parkLog.allow("attached", time.Now()) {
					log.InfoLog.Printf("watch task %s: target session attached; holding %d queued event(s) until detach — repeats at most every %s while this holds (#1586)", w.taskID, w.queue.pendingCount(), watcherParkLogInterval)
				}
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
		// The park is over — re-arm the notice so the NEXT episode logs on entry
		// instead of being silently swallowed by the previous one's throttle window.
		parkLog.reset()
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
