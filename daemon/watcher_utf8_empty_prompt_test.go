package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

// TestEnqueueEventDiscardCountsDropRecorded pins the durable-queue-boundary
// discard's drop accounting: a line whose rendered prompt trims to "" (a
// default-prompt task with a blank or whitespace-only sanitized line) is
// neither delivered nor durably retained, so the loss must be counted through
// recordEventDrop rather than vanishing silently. The in-memory dropped counter
// (the same one recordEventDrop increments for a rate-full drop, a no-queue
// loss, and a refused enqueue) is the accounting under test, so asserting it
// advanced proves the branch went through recordEventDrop.
func TestEnqueueEventDiscardCountsDropRecorded(t *testing.T) {
	s := newWatcherSupervisor()
	s.loadTasks = staticTasks(task.Task{ID: "disc01", Prompt: "", Enabled: true})
	s.recordDrops = func(string, int, time.Time) error { return nil }
	queue := newEventQueue(t.TempDir(), "disc01")
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "disc01", sup: s, queue: queue, stopCh: stopCh, draining: true}

	// A blank line under the default (empty) prompt renders to "" and is
	// discarded at the boundary; the loss is recorded via recordEventDrop.
	w.enqueueEvent("", &tailBuffer{}, false)
	if queue.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d, want 0 (default-prompt blank line is discarded, not enqueued)", queue.pendingCount())
	}
	if d := droppedCount(w); d != 1 {
		t.Fatalf("dropped = %d, want 1 (the discard path must count the lost event through recordEventDrop)", d)
	}

	// A whitespace-only sanitized line under the default prompt renders to a
	// trims-empty prompt and is likewise discarded+counted (the #4
	// whitespace-only regression shape).
	w.enqueueEvent(" ", &tailBuffer{}, false)
	if queue.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d after whitespace-only line, want 0", queue.pendingCount())
	}
	if d := droppedCount(w); d != 2 {
		t.Fatalf("dropped = %d, want 2 (each discarded branch counts through recordEventDrop)", d)
	}

	// A deliverable line is retained and is NOT a drop — the counter only
	// counts losses, proving the increment above is the discard, not enqueue.
	w.enqueueEvent("deliverable-line", &tailBuffer{}, false)
	if queue.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d, want 1 (a deliverable line is retained)", queue.pendingCount())
	}
	if d := droppedCount(w); d != 2 {
		t.Fatalf("dropped = %d, want 2 (a retained event must not be counted as a drop)", d)
	}
}

// TestEnqueueEventFailsOpenWhenTaskStoreUnreadable pins the fail-open side of
// the boundary discard: when the supervisor's loadTasks fails (a transient
// task-store outage), the template is UNKNOWN, and the boundary must NOT
// equate unknown with the default prompt and discard the event. A blank
// sanitized line is retained so the drainer can retry once the store
// recovers, rather than being permanently dropped. The discard only fires on
// a DEPENDABLE empty render; ok=false keeps the event.
func TestEnqueueEventFailsOpenWhenTaskStoreUnreadable(t *testing.T) {
	s := newWatcherSupervisor()
	s.loadTasks = func() ([]task.Task, error) { return nil, errors.New("tasks.json unreadable") }
	s.recordDrops = func(string, int, time.Time) error { return nil }
	queue := newEventQueue(t.TempDir(), "fa01")
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "fa01", sup: s, queue: queue, stopCh: stopCh, draining: true}

	w.enqueueEvent("", &tailBuffer{}, false)
	if queue.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d, want 1 (fail-open: a blank line under an unreadable task store is retained, not discarded)", queue.pendingCount())
	}
	ev, _, ok, err := queue.peek()
	if err != nil || !ok {
		t.Fatalf("peek queued fail-open event: ok=%v err=%v", ok, err)
	}
	if ev.Line != "" {
		t.Fatalf("fail-open queued Line = %q, want %q", ev.Line, "")
	}
	if d := droppedCount(w); d != 0 {
		t.Fatalf("dropped = %d, want 0 (a fail-open retain must not be counted as a drop)", d)
	}
}

// TestEnqueueEventRenderGatedToBlankLines pins the render's cost guard: the
// loadTasks-backed render is paid only when the sanitized line trims to blank
// (the only case that can park a stuck head), not on every event. A non-blank
// line never reaches loadTasks, so a stop-drain that processes a pipeful of
// lines cannot hold a shutdown per line on a wedged task-store lock.
func TestEnqueueEventRenderGatedToBlankLines(t *testing.T) {
	s := newWatcherSupervisor()
	var loads int
	s.loadTasks = func() ([]task.Task, error) {
		loads++
		return []task.Task{{ID: "gated01", Prompt: "Triage: {{line}}", Enabled: true}}, nil
	}
	s.recordDrops = func(string, int, time.Time) error { return nil }
	queue := newEventQueue(t.TempDir(), "gated01")
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "gated01", sup: s, queue: queue, stopCh: stopCh, draining: true}

	// A non-blank line is always deliverable; the render (and its loadTasks)
	// is skipped entirely.
	w.enqueueEvent("non-blank-line", &tailBuffer{}, false)
	if loads != 0 {
		t.Fatalf("loadTasks called %d time(s) for a non-blank line, want 0 (the render is gated to blank lines)", loads)
	}
	if queue.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d, want 1 (a non-blank line is enqueued)", queue.pendingCount())
	}

	// A blank line needs the render to decide keep-vs-discard, so loadTasks
	// fires exactly once for it.
	w.enqueueEvent("", &tailBuffer{}, false)
	if loads != 1 {
		t.Fatalf("loadTasks called %d time(s) after a blank line, want 1 (the render runs only for the blank case)", loads)
	}
}

// TestDrainAdvancesEmptyPromptHead pins the drain-side half of the
// durable-queue empty-prompt discipline: a queued head whose rendered prompt
// trims to "" would re-render empty on every retry and block all later events
// (the enqueue-time discard only saw the prompt THEN — a prompt edit to the
// default between enqueue and replay parks the head). The drain recognizes the
// stable empty-prompt failure (errEmptyPrompt) and advances the head —
// counting the loss through recordEventDrop — instead of retrying the same
// empty render forever.
func TestDrainAdvancesEmptyPromptHead(t *testing.T) {
	s := newWatcherSupervisor()
	s.drainBaseBackoff = 20 * time.Millisecond
	s.drainMaxBackoff = 100 * time.Millisecond
	s.queueMaxAge = 0
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.deliver = func(_ string, line string, _ watchDeliveryOptions) error {
		return notAttempted(fmt.Errorf("event rendered an empty prompt (line %q): %w", line, errEmptyPrompt))
	}
	queue := newEventQueue(t.TempDir(), "adv01")
	if err := queue.enqueue("head-being-replayed"); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "adv01", sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	go w.drainLoop()
	waitUntil(t, 2*time.Second, "empty-prompt head to be advanced", func() bool {
		return queue.pendingCount() == 0
	})
	close(stopCh)
	w.wg.Wait()
	if d := droppedCount(w); d != 1 {
		t.Fatalf("dropped = %d, want 1 (the advanced empty-prompt head is one lost event, counted through recordEventDrop)", d)
	}
}

// TestDrainRetriesGenericNotAttempted pins that the empty-prompt advance is
// targeted: a generic pre-flight errNotAttempted that does NOT carry
// errEmptyPrompt (a transient target-gone failure) still retries and does NOT
// advance the head, preserving the never-permanent-give-up discipline (#1128).
// Only the stable empty-prompt case advances.
func TestDrainRetriesGenericNotAttempted(t *testing.T) {
	s := newWatcherSupervisor()
	s.drainBaseBackoff = 20 * time.Millisecond
	s.drainMaxBackoff = 100 * time.Millisecond
	s.queueMaxAge = 0
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.deliver = func(_ string, _ string, _ watchDeliveryOptions) error {
		return notAttempted(errors.New("target session gone; prompt not delivered"))
	}
	queue := newEventQueue(t.TempDir(), "retry01")
	if err := queue.enqueue("head-being-replayed"); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "retry01", sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	go w.drainLoop()
	// Give the drainer several backoff cycles; a generic errNotAttempted must
	// NOT advance the head.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if queue.pendingCount() == 0 {
			t.Fatalf("generic errNotAttempted advanced the head; only errEmptyPrompt should advance — the never-permanent-give-up discipline (#1128) is broken")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stopCh)
	w.wg.Wait()
	if queue.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d, want 1 (generic errNotAttempted retries, keeping the head)", queue.pendingCount())
	}
	if d := droppedCount(w); d != 0 {
		t.Fatalf("dropped = %d, want 0 (a retried pre-flight failure is not a drop)", d)
	}
}

// TestHandleEventEmptyPromptDoesNotStampAlarmRun pins the live-side half of the
// alarm discipline: a blank line under the default prompt renders empty, so
// deliver returns errEmptyPrompt and handleEvent must not record it through
// recordDeliveryResult as a delivery failure — otherwise a watcher whose only
// events are blank/sanitized-to-empty lines alarms after three minutes on an
// empty queue with nothing left to retry. enqueueEvent's boundary discard
// (TestEnqueueEventDiscardCountsDropRecorded) disposes of the event; the
// alarm-side guard keeps the failure run clear.
func TestHandleEventEmptyPromptDoesNotStampAlarmRun(t *testing.T) {
	s := newWatcherSupervisor()
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.loadTasks = staticTasks(task.Task{ID: "alarm-live01", Prompt: "", Enabled: true})
	s.deliver = func(_ string, line string, _ watchDeliveryOptions) error {
		return notAttempted(fmt.Errorf("event rendered an empty prompt (line %q): %w", line, errEmptyPrompt))
	}
	queue := newEventQueue(t.TempDir(), "alarm-live01")
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "alarm-live01", sup: s, queue: queue, stopCh: stopCh, draining: true}

	w.handleEvent("", &tailBuffer{})

	if d := droppedCount(w); d != 1 {
		t.Fatalf("dropped = %d, want 1 (the blank line is discarded at the queue boundary)", d)
	}
	if queue.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d, want 0 (the discarded blank line is not enqueued)", queue.pendingCount())
	}
	w.mu.Lock()
	since, count := w.deliverFailSince, w.deliverFailCount
	w.mu.Unlock()
	if !since.IsZero() || count != 0 {
		t.Fatalf("errEmptyPrompt stamped a delivery-failure run: since=%v count=%d (an intentional drop must not alarm)", since, count)
	}
}

// TestDrainEmptyPromptDoesNotStampAlarmRun pins the drain-side half of the
// alarm discipline (the advance itself is pinned by
// TestDrainAdvancesEmptyPromptHead): an errEmptyPrompt replay result is an
// intentional non-delivery (the rendered prompt trims to ""), not a pipeline
// outage, so the drain loop must not record it through recordDeliveryResult —
// otherwise a watcher whose queue head renders empty alarms after three
// minutes even though the head was advanced and nothing is left to retry.
func TestDrainEmptyPromptDoesNotStampAlarmRun(t *testing.T) {
	s := newWatcherSupervisor()
	s.drainBaseBackoff = 20 * time.Millisecond
	s.drainMaxBackoff = 100 * time.Millisecond
	s.queueMaxAge = 0
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.deliver = func(_ string, line string, _ watchDeliveryOptions) error {
		return notAttempted(fmt.Errorf("event rendered an empty prompt (line %q): %w", line, errEmptyPrompt))
	}
	queue := newEventQueue(t.TempDir(), "alarm-drain01")
	if err := queue.enqueue("head-being-replayed"); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "alarm-drain01", sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	go w.drainLoop()
	waitUntil(t, 2*time.Second, "empty-prompt head to be advanced", func() bool {
		return queue.pendingCount() == 0
	})
	close(stopCh)
	w.wg.Wait()
	if d := droppedCount(w); d != 1 {
		t.Fatalf("dropped = %d, want 1 (the advanced empty-prompt head is one lost event)", d)
	}
	w.mu.Lock()
	since, count := w.deliverFailSince, w.deliverFailCount
	w.mu.Unlock()
	if !since.IsZero() || count != 0 {
		t.Fatalf("errEmptyPrompt stamped a delivery-failure run: since=%v count=%d (an intentional drop must not alarm)", since, count)
	}
}

// droppedCount reads w.dropped under its lock for the discard-accounting tests.
func droppedCount(w *taskWatcher) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// TestPersistRemainingLimitEventsCachesTaskLoadAcrossBlankLines pins the
// stop-drain caching of the boundary render: persistRemainingLimitEvents loads
// the task store ONCE for the whole drain and reuses that snapshot for every
// drained blank line, rather than re-acquiring the tasks.json flock per blank
// event. Production task.LoadTasks blocks up to SchemaMigrationLockTimeout on a
// wedged lock; without the cache, a finite stdout pipe of N blank/invalid-only
// lines could delay a shutdown or task reload by N × that timeout. The test
// feeds a pipeful of distinct blank lines (`""`, `" "`, `"\t"`) and asserts
// loadTasks fired exactly once across all of them. The chosen prompt
// (Triage: {{line}}) renders to a non-empty trim on every blank input, so the
// cached render is actually consulted (the discard path does NOT fire) and
// every drained blank line is retained — pinning that the cache is on the
// render the boundary uses, not on a silent short-circuit.
func TestPersistRemainingLimitEventsCachesTaskLoadAcrossBlankLines(t *testing.T) {
	const taskID = "stopcach01"
	var loads int
	s := newWatcherSupervisor()
	s.loadTasks = func() ([]task.Task, error) {
		loads++
		return []task.Task{{ID: taskID, Prompt: "Triage: {{line}}", Enabled: true}}, nil
	}
	s.recordDrops = func(string, int, time.Time) error { return nil }
	queue := newEventQueue(t.TempDir(), taskID)
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: taskID, sup: s, queue: queue, stopCh: stopCh}

	// Four distinct blank lines (sanitize then TrimSpace all == ""); each
	// reaches the boundary render, and the cached render is bound to a single
	// loadTasks snapshot.
	br := bufio.NewReaderSize(strings.NewReader("\n \n\t\n\n"), maxWatchLineBytes)
	w.persistRemainingLimitEvents(br, &tailBuffer{})

	if loads != 1 {
		t.Fatalf("loadTasks called %d time(s) for a stop drain of 4 blank lines, want exactly 1 (the render is cached across the whole stop drain so a wedged tasks-file lock can hold shutdown by at most one SchemaMigrationLockTimeout window, not one per blank)", loads)
	}
	if got := queue.pendingCount(); got != 4 {
		t.Fatalf("pendingCount = %d after stop drain of 4 blank lines, want 4 (Triage: {{line}} renders non-empty for blank input, so the cached render is consulted and retains every drained blank rather than short-circuiting the discard)", got)
	}
}

// TestPersistRemainingLimitEventsCacheIsLazyForNonBlankDrains pins the lazy
// side of the stop-drain cache: a stop drain of purely non-blank lines never
// reaches the boundary render (sanitizeTrim sees non-blank), so the cached
// loadTasks must NOT fire at all — the drain stays lock-free exactly as it did
// before the cache landed. The slow lock-wait the cache bounds is paid only
// for drains that actually need it; non-blank pipes are unchanged.
func TestPersistRemainingLimitEventsCacheIsLazyForNonBlankDrains(t *testing.T) {
	const taskID = "stopcach02"
	var loads int
	s := newWatcherSupervisor()
	s.loadTasks = func() ([]task.Task, error) {
		loads++
		return []task.Task{{ID: taskID, Prompt: "event: {{line}}", Enabled: true}}, nil
	}
	s.recordDrops = func(string, int, time.Time) error { return nil }
	queue := newEventQueue(t.TempDir(), taskID)
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: taskID, sup: s, queue: queue, stopCh: stopCh}

	br := bufio.NewReaderSize(strings.NewReader("prefetched-one\nprefetched-two\npartial"), maxWatchLineBytes)
	w.persistRemainingLimitEvents(br, &tailBuffer{})

	if loads != 0 {
		t.Fatalf("loadTasks called %d time(s) for a stop drain of non-blank lines, want 0 (the cache loads lazily only when a blank line is encountered; a non-blank drain stays lock-free)", loads)
	}
	if got := queue.pendingCount(); got != 2 {
		t.Fatalf("pendingCount = %d after stop drain of two non-blank lines, want 2 (the two newline-terminated lines persist; the partial trailing chunk is not an event)", got)
	}
}
