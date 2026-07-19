package daemon

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// Rate-slot refund discipline (#2102). tryReserveEventSlot spends one of the
// task's 10-events/min slots before every delivery attempt; the slot must come
// back iff the attempt delivered NOTHING. These tests pin both halves of that
// rule — the refund, and the deliberate non-refund when a failure may have
// landed — on the live path and the replay path.

// newRateSlotWatcher builds a watcher wired to deliver, with a real queue in a
// temp dir. stopRequested is pre-armed so ensureDrainer never spawns a drainer
// behind the assertions; the drain-path test drives drainLoop itself.
func newRateSlotWatcher(t *testing.T, taskID string, deliver func(string, string) error) *taskWatcher {
	t.Helper()
	s := newWatcherSupervisor()
	s.eventsPerMinute = 10
	s.deliver = deliver
	return &taskWatcher{
		sup:      s,
		taskID:   taskID,
		queue:    newEventQueue(t.TempDir(), taskID),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		draining: false,
	}
}

// spentSlots reports how much of the per-minute budget is currently consumed.
func spentSlots(w *taskWatcher) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.eventTimes)
}

// seedDisabledWatchTask writes a DISABLED watch task to the real task store, so
// the production deliverWatchEvent hook fails its pre-flight check and provably
// reaches neither the manager nor the target session.
func seedDisabledWatchTask(t *testing.T, taskID string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedTargetSession(t, repo, "captain")
	if err := task.AddTask(task.Task{
		ID:            taskID,
		Name:          "gh-issues",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: "captain",
		ProjectPath:   repo,
		Enabled:       false,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// TestWatcherHandleEvent_RefundsRateSlotWhenNothingWasDelivered is the #2102
// regression on the live path: a genuine error that failed pre-flight (here a
// disabled task) delivers nothing, exactly like a deferral, so it must refund
// the slot it reserved. Before the fix only deferrals refunded, so every failed
// attempt burned budget the drainer's retries then had to compete for.
func TestWatcherHandleEvent_RefundsRateSlotWhenNothingWasDelivered(t *testing.T) {
	seedDisabledWatchTask(t, "cafe2102")
	_, sends := stubTaskDelivery(t)

	w := newRateSlotWatcher(t, "cafe2102", deliverWatchEvent)
	close(w.stopCh) // no drainer behind the assertions
	w.handleEvent("new issue #9", &tailBuffer{})

	if len(*sends) != 0 {
		t.Fatalf("a disabled task must not receive a delivery, got %+v", *sends)
	}
	if got := spentSlots(w); got != 0 {
		t.Fatalf("rate slots spent = %d, want 0 (nothing was delivered, so the slot must be refunded)", got)
	}
	// The event is still queued for replay — refunding must not drop it.
	if got := w.queue.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1 (a failed delivery is queued for replay)", got)
	}
}

// TestWatcherDrain_RefundsRateSlotWhenNothingWasDelivered is the #2102
// regression on the replay path. The drain loop retries for as long as an
// outage lasts, so an unrefunded slot per retry is where the leak compounds:
// the budget the recovery needs is spent on attempts that reached nothing.
func TestWatcherDrain_RefundsRateSlotWhenNothingWasDelivered(t *testing.T) {
	seedDisabledWatchTask(t, "cafe2105")

	attempted := make(chan struct{})
	var once sync.Once
	w := newRateSlotWatcher(t, "cafe2105", func(taskID, line string) error {
		err := deliverWatchEvent(taskID, line)
		once.Do(func() { close(attempted) })
		return err
	})
	// Long backoffs: exactly one replay attempt happens before the stop below,
	// so the assertion counts one attempt's slot, not a race with retries.
	w.sup.drainBaseBackoff = 10 * time.Second
	w.sup.drainMaxBackoff = 10 * time.Second
	if err := w.queue.enqueue("new issue #9"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	w.wg.Add(1)
	go w.drainLoop()
	select {
	case <-attempted:
	case <-time.After(10 * time.Second):
		t.Fatal("drainer never attempted the queued event")
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wg.Wait()

	if got := spentSlots(w); got != 0 {
		t.Fatalf("rate slots spent after a failed replay = %d, want 0 (the retry must not double-charge)", got)
	}
	if got := w.queue.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1 (an undelivered event stays queued)", got)
	}
}

// TestWatcherHandleEvent_KeepsRateSlotWhenDeliveryMayHaveLanded pins the other
// half of the rule, and is why this is not a blanket refund. A failed create or
// send crosses the control socket and delivery is keystrokes-then-submit, so
// the error can surface AFTER the prompt landed. Refunding that would let a
// delivered event escape the per-minute budget the limiter exists to enforce,
// so an ambiguous failure stays charged.
func TestWatcherHandleEvent_KeepsRateSlotWhenDeliveryMayHaveLanded(t *testing.T) {
	sendErr := fmt.Errorf("failed to deliver prompt to target session %q: %w", "captain", errors.New("connection reset by peer"))
	w := newRateSlotWatcher(t, "cafe2103", func(string, string) error { return sendErr })
	close(w.stopCh)

	w.handleEvent("new issue #9", &tailBuffer{})

	if got := spentSlots(w); got != 1 {
		t.Fatalf("rate slots spent = %d, want 1 (a failure that may have landed must stay charged)", got)
	}
}

// TestWatcherHandleEvent_SuccessfulDeliveryConsumesRateSlot guards against
// over-refunding: a delivery that actually landed spends its slot, which is the
// whole point of the per-minute budget.
func TestWatcherHandleEvent_SuccessfulDeliveryConsumesRateSlot(t *testing.T) {
	w := newRateSlotWatcher(t, "cafe2104", func(string, string) error { return nil })
	close(w.stopCh)

	w.handleEvent("new issue #9", &tailBuffer{})

	if got := spentSlots(w); got != 1 {
		t.Fatalf("rate slots spent = %d, want 1 (a delivered event must consume its slot)", got)
	}
	if got := w.queue.pendingCount(); got != 0 {
		t.Fatalf("pending = %d, want 0 (a delivered event is not queued)", got)
	}
}
