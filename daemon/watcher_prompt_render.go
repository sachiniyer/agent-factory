package daemon

import (
	"errors"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// renderWatchPromptFunc renders the durable-queue boundary's deliverability
// check for one (taskID, line) pair. The default impl is the supervisor's
// per-call renderedWatchPrompt (live arm, one task-store load per blank
// event); the stop-drain arm supplies a cachedRenderWatchPrompt closure that
// loads once across the whole drain so a pipeful of blank lines cannot wedge
// shutdown one lock-wait per line.
type renderWatchPromptFunc func(taskID, line string) (render string, dependable bool)

// renderedWatchPrompt returns line rendered through taskID's watch-prompt
// template plus whether that render is dependable, mirroring the pre-flight
// `strings.TrimSpace(prompt) == ""` rule deliverWatchEventWithOptions applies
// before any send (daemon/watcher.go:920), so the durable-queue boundary can
// decide deliverability with the same yardstick the drainer's replay will.
// The supervisor's loader is the same source deliverWatchEventWithOptions
// reloads per event, so a prompt edit between enqueue and replay is reflected
// at whichever arm reads it last; whichever arm first sees a trimspace-empty
// render drops the event, and the survivor behaves identically.
//
// The render is dependable only when the task was actually loaded: a loader
// that is unset (supervisor built by a focused test), fails, or finds the task
// gone leaves the prompt UNKNOWN, and equating unknown with the default prompt
// would permanently discard an event whose real template may yet render a
// deliverable prompt once the store recovers. The caller therefore fails OPEN
// on ok=false (retains the event) and lets the drainer retry, so the discard
// only fires on a dependable empty render — never on a default-prompt
// assumption that could lose a recoverable event. A fail-open of a genuinely
// blank line parks at worst until the drainer re-renders; the drain path's
// errEmptyPrompt advance (daemon/watcher_drain.go) bounds that park to one
// replay, so the fail-open cannot strand the queue.
func (s *watcherSupervisor) renderedWatchPrompt(taskID, line string) (string, bool) {
	if s.loadTasks == nil {
		return line, false
	}
	tasks, err := s.loadTasks()
	if err != nil {
		return line, false
	}
	return s.renderedWatchPromptFrom(tasks, true, taskID, line)
}

// cachedRenderWatchPrompt returns a render closure bound to a single loadTasks
// snapshot, so a caller that renders many blank lines in a tight loop (the
// stop-drain arm) pays for one task-store load across the whole loop rather
// than one per blank line. Production task.LoadTasks acquires the tasks.json
// flock with a fresh SchemaMigrationLockTimeout; without this cache, a finite
// stdout pipe of blank/invalid-only lines would re-acquire the lock once per
// drained event and could hold a shutdown or task reload for roughly that
// timeout multiplied by the number of blank lines. One load bounds the whole
// drain to a single lock-wait window regardless of how many blanks the pipe
// holds.
//
// The closure mirrors renderedWatchPrompt's contract exactly: a nil loader,
// a load failure, or a task not present in the snapshot leaves the render
// undependable (ok=false), so the caller fails OPEN (retains the event) and
// lets the drainer retry once the store recovers — never equating unknown
// with the default prompt and permanently dropping a recoverable event. The
// snapshot is the FIRST load's result: a stop-drain is a self-contained
// teardown operation that should not re-acquire the lock between events, and
// a prompt edit landing mid-drain would otherwise be split across the
// drained lines, while the live arm's own per-event reload would observe the
// same edit on its next event anyway.
func (s *watcherSupervisor) cachedRenderWatchPrompt() renderWatchPromptFunc {
	if s.loadTasks == nil {
		return func(_, line string) (string, bool) { return line, false }
	}
	tasks, err := s.loadTasks()
	if err != nil {
		return func(_, line string) (string, bool) { return line, false }
	}
	return func(taskID, line string) (string, bool) {
		return s.renderedWatchPromptFrom(tasks, true, taskID, line)
	}
}

// renderedWatchPromptFrom renders line through taskID's prompt template using
// an already-loaded tasks slice. The dependable flag follows renderedWatchPrompt's
// contract: a load failure (loaded=false) or a task not found in the slice
// returns (line, false) so the caller fails OPEN, never equating unknown with
// the default prompt. Extracted so renderedWatchPrompt (the live per-call
// loader) and cachedRenderWatchPrompt (the stop-drain single-load cache) defer
// to one render path and cannot drift on the "is it dependable" rule.
func (s *watcherSupervisor) renderedWatchPromptFrom(tasks []task.Task, loaded bool, taskID, line string) (string, bool) {
	if !loaded {
		return line, false
	}
	for i := range tasks {
		if tasks[i].ID == taskID {
			return task.RenderWatchPrompt(tasks[i].Prompt, line), true
		}
	}
	return line, false
}

// enqueueEventRendered is the shared durable-queue boundary: it appends the
// line to the durable backlog, wakes the drainer, and counts every loss
// through recordEventDrop, never silently. `render` supplies the prompt-render
// the blank-line discard uses to decide deliverability; the live arm
// (enqueueEvent) passes the supervisor's per-call renderedWatchPrompt so a
// live render reflects the freshest prompt, and the stop-drain arm
// (persistRemainingLimitEvents) passes a cachedRenderWatchPrompt so a finite
// pipe of blank lines pays for one task-store load across the whole drain
// rather than one per blank line.
//
// The line also lands in the run's failure tail — it did not become a
// delivered event this run (#797). With no durable queue, or when the append
// fails WITHOUT retaining the record, the event is neither delivered nor held
// and is counted through recordEventDrop, whatever delivery outcome sent it
// here — a usage-limit park, an attached target, a concurrency park, or a
// genuine failure. An error is not proof of loss: a close/flush or cap fault
// can follow a landed record, so retention decides.
//
// The live arm (`consumeLines -> handleEvent -> enqueueEvent` here) reaches
// here already `sanitizeUTF8`'d at the call site (daemon/watcher.go:642), but
// the stop-drain arm (`persistRemainingLimitEvents -> emit -> here` in
// daemon/watcher_limit_park.go) feeds the raw line straight in. The durable
// record persists via `json.Marshal(queuedEvent{Line: line})`, whose
// encoding/json rewrites invalid UTF-8 as U+FFFD (#863 class, exposed by
// #1129, #4655), so this is the shared durable-queue boundary every durable
// enqueue crosses. `sanitizeUTF8` — a no-op on valid UTF-8 — drops invalid
// bytes here so every record (not just the live arm's) is well-formed.
//
// The discard keys on the rendered prompt's deliverability, not the raw line:
// deliverWatchEventWithOptions rejects before any send when
// strings.TrimSpace(prompt) == "" (its empty-prompt pre-flight), so enrolling
// such a line parks a head every replay rejects, blocking later events. Only
// a trims-blank line can render empty, so the render is gated to the rare
// blank case instead of paid on every event — and on the stop-drain arm the
// render is cached for the whole drain so a wedged task-store lock holds the
// drain for one timeout window, not one per blank line. The render must be
// dependable to discard: a transient loadTasks failure or a gone task leaves
// the template unknown (ok=false), so fail OPEN (retain) and let the drainer
// retry once the store recovers, rather than equating unknown with the
// default prompt and permanently dropping a recoverable event. A discard is
// a loss (neither delivered nor retained), so it is counted through
// recordEventDrop like every other lost event, never silently; tailBuffer.add
// is a no-op on blank lines, so the failure tail shows nothing for a
// discarded blank.
func (w *taskWatcher) enqueueEventRendered(line string, tail *tailBuffer, limitParked bool, render renderWatchPromptFunc, parkedStatusRecorded ...bool) {
	line = sanitizeUTF8(line)
	if strings.TrimSpace(line) == "" {
		if render == nil {
			render = w.sup.renderedWatchPrompt
		}
		if r, ok := render(w.taskID, line); ok && strings.TrimSpace(r) == "" {
			if dropped, logIt := w.recordEventDrop(); logIt {
				log.WarningLog.Printf("watch task %s: dropped event whose rendered prompt is empty; it cannot be delivered or retained (%d dropped so far)", w.taskID, dropped)
			}
			return
		}
	}
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

// recordDeliveryResultUnlessEmptyPrompt folds a delivery attempt's outcome
// into the watcher's alarm state unless the attempt died on errEmptyPrompt, an
// intentional non-delivery (the rendered prompt trims to "") that
// enqueueEventRendered's boundary discard (daemon/watcher_prompt_render.go) or
// drainLoop's head advance (daemon/watcher_drain.go) removes. errEmptyPrompt is not a pipeline
// failure, so feeding it to recordDeliveryResult would stamp a delivery-failure
// run that alarms after three minutes on an empty queue with nothing left to
// retry — the same shape of non-failure errTargetBusy, errAtConcurrencyLimit,
// and errTargetLimitReached are classified as inside recordDeliveryResult
// (delivery_alarm.go). Skipping the call leaves a prior genuine failure's run
// in place, so a real outage is not masked by a stray blank line; a later
// genuine failure re-stamps the run.
func (w *taskWatcher) recordDeliveryResultUnlessEmptyPrompt(now time.Time, err error) {
	if errors.Is(err, errEmptyPrompt) {
		return
	}
	w.recordDeliveryResult(now, err)
}
