package daemon

import (
	"errors"
	"sort"
	"time"
)

// Watch-task delivery-failure alarm state (#1238 fix c). The daemon tracks how
// long each watch task's event delivery to its target session has been failing
// and projects the persistent ones into the Snapshot (see snapshot.go) so the
// TUI can raise a banner — turning the 2026-07-05 silent, log-only outage into
// an alarm visible within a bounded window. The per-watcher failure fields live
// on taskWatcher (watcher.go); this file owns the threshold, the per-attempt
// bookkeeping, and the snapshot assembly.

// watcherDeliveryAlarmThreshold is how long a watch task's event delivery must
// have been failing before the daemon raises a TUI-visible alarm for it. It
// sits just past the #1237 root self-heal window (rootKillHealDelay, 2m): a
// normal root kill self-heals and delivery recovers — clearing deliverFailSince
// — before the threshold, so the routine ~2m recovery never false-alarms. A
// failure that persists materially past that window (target permanently gone,
// tmux server dead, or a misconfigured target) crosses the threshold and
// alarms. On the rare boundary where a heal lands late in the drain backoff
// cycle, the alarm may show briefly and then auto-clear on recovery — honest,
// since delivery genuinely was down past the threshold. A var so tests can
// shrink it and exercise the threshold without real waits.
var watcherDeliveryAlarmThreshold = 3 * time.Minute

// recordDeliveryResult folds one delivery attempt's outcome into the watcher's
// alarm state. A failure starts (or extends) the consecutive-failure run —
// stamping deliverFailSince on the first failure so the alarm can measure how
// long the pipeline has been down — and records the error. A success clears the
// run, which is what makes the TUI banner disappear the instant delivery
// recovers. Called after every deliver attempt on both the live path
// (handleEvent) and the replay path (drainLoop).
func (w *taskWatcher) recordDeliveryResult(now time.Time, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil || errors.Is(err, errTargetBusy) || errors.Is(err, errAtConcurrencyLimit) {
		// A success clears the failure run. A deferral (target attached, #1586)
		// clears it too: it is not a delivery failure, and the pipeline is now
		// intentionally paused, not broken — so the delivery-failure alarm (#1238)
		// must go quiet rather than keep showing a stale earlier failure that
		// would never clear while the target stays attached (deferrals never
		// deliver, so nothing else would reset it). If delivery is genuinely still
		// broken, the drainer's next real attempt after detach re-stamps the run.
		//
		// A concurrency park (#1892) is the same shape of non-failure: the task is
		// at its own configured cap and the pipeline is working exactly as asked.
		// Alarming on it would fire the delivery-failure alarm on every healthy
		// task that ever saturates its cap.
		w.deliverFailSince = time.Time{}
		w.deliverFailCount = 0
		w.deliverFailErr = ""
		return
	}
	if w.deliverFailSince.IsZero() {
		w.deliverFailSince = now
	}
	w.deliverFailCount++
	w.deliverFailErr = err.Error()
}

// noteQueueUnreadable opens (or refreshes the error on) the queue-unreadable
// alarm run (#3242). A parked replay is an outage even while live deliveries
// still succeed, so this run is tracked apart from the delivery-failure one —
// a live success must never reset the backlog alarm's clock. Called by the
// drain loop on every unreadable peek.
func (w *taskWatcher) noteQueueUnreadable(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loadFailSince.IsZero() {
		w.loadFailSince = time.Now()
	}
	w.loadFailErr = err.Error()
}

// clearQueueUnreadable ends the queue-unreadable alarm run: the backlog was
// enumerated again. It deliberately touches nothing in the delivery-failure
// run — a delivery outage that began while the queue was unreadable keeps its
// original clock.
func (w *taskWatcher) clearQueueUnreadable() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loadFailSince = time.Time{}
	w.loadFailErr = ""
}

// deliveryAlarms returns the persistent delivery-failure alarms across watch
// tasks whose repo matches repoID (empty = all repos), evaluated against now.
// A task alarms only once its consecutive delivery failures have persisted for
// at least watcherDeliveryAlarmThreshold — long enough that a normal root
// self-heal (#1237, ~2m) would have cleared, so the routine recovery window
// never false-alarms. The pending count is the queue's undelivered backlog, so
// the banner can say how many events are stuck.
func (s *watcherSupervisor) deliveryAlarms(repoID string, now time.Time) []DeliveryAlarm {
	s.mu.Lock()
	ws := make([]*taskWatcher, 0, len(s.watchers))
	for _, w := range s.watchers {
		ws = append(ws, w)
	}
	s.mu.Unlock()

	var alarms []DeliveryAlarm
	for _, w := range ws {
		if repoID != "" && w.repoID != repoID {
			continue
		}
		w.mu.Lock()
		since := w.deliverFailSince
		count := w.deliverFailCount
		lastErr := w.deliverFailErr
		loadSince := w.loadFailSince
		loadErr := w.loadFailErr
		w.mu.Unlock()
		// Two independent runs can alarm (#3242): failing deliveries, and an
		// unreadable backlog whose replay is parked — an outage even while live
		// deliveries succeed. Either one persisting past the threshold alarms;
		// when only the load run qualifies, it supplies the alarm's clock and
		// error (there are no delivery attempts to count).
		deliverAlarms := !since.IsZero() && now.Sub(since) >= watcherDeliveryAlarmThreshold
		loadAlarms := !loadSince.IsZero() && now.Sub(loadSince) >= watcherDeliveryAlarmThreshold
		if !deliverAlarms && !loadAlarms {
			continue
		}
		if !deliverAlarms {
			since, count, lastErr = loadSince, 0, loadErr
		}
		pending := 0
		pendingUnknown := false
		if w.queue != nil {
			// One atomic read: separate count/unknown calls race a concurrent
			// heal into "0 pending, known" — the exact fabricated banner the
			// unknown flag exists to prevent (#3242). The throttled retry
			// inside heals a recovered load; while the state stays unknown,
			// say so instead of projecting the zero.
			pending, pendingUnknown = w.queue.pendingState()
		}
		alarms = append(alarms, DeliveryAlarm{
			TaskID:         w.taskID,
			TaskName:       w.name,
			TargetSession:  w.targetSession,
			Pending:        pending,
			PendingUnknown: pendingUnknown,
			Consecutive:    count,
			Since:          since,
			LastError:      lastErr,
		})
	}
	sort.Slice(alarms, func(i, j int) bool { return alarms[i].TaskID < alarms[j].TaskID })
	return alarms
}
