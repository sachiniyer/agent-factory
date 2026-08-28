package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
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

// seedEnabledWatchTask is seedDisabledWatchTask's ENABLED sibling: the
// production deliverWatchEvent hook passes its pre-flight checks and reaches the
// delivery path, so a test can drive the target-session delivery for real.
func seedEnabledWatchTask(t *testing.T, taskID, target string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedTargetSession(t, repo, target)
	if err := task.AddTask(task.Task{
		ID:            taskID,
		Name:          "gh-issues",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: target,
		ProjectPath:   repo,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return repo
}

// TestNotAttemptedSurvivesRPCFlattening is the #2501 wire contract, and the exact
// gap the first cut of the fix had: notAttempted() was minted inside the Manager,
// but the watch path reaches the Manager over the daemon's own control socket
// (deliverPromptForTask), and net/rpc flattens a *notAttemptedError to a plain
// string — so errors.Is against the sentinel stays FALSE on the way back and the
// slot never refunds. The marker in the wire text is what survives; this pins it.
// Mirrors TestIsAtConcurrencyLimitErrSurvivesRPCFlattening (#1892).
func TestNotAttemptedSurvivesRPCFlattening(t *testing.T) {
	preflight := notAttempted(fmt.Errorf("target session %q is being deleted; prompt not delivered", "captain"))

	// Exactly what net/rpc does: reconstitute the server error from its text only.
	flattened := fmt.Errorf("%s", preflight.Error())

	// The precondition the first cut missed — assert it, so this test cannot pass
	// for the wrong reason the way an in-process check did.
	if errors.Is(flattened, errNotAttempted) {
		t.Fatal("precondition failed: flattening did not destroy the type, so this proves nothing about the wire")
	}
	if !isNotAttemptedErr(flattened) {
		t.Fatal("a pre-flight failure must still be recognizable after net/rpc flattens it to a string (#2501)")
	}
	// The other half of the rule: an ambiguous send failure must NOT be refunded.
	if isNotAttemptedErr(fmt.Errorf("failed to send prompt: connection reset by peer")) {
		t.Fatal("an ambiguous send failure must not be mistaken for a pre-flight not-attempted failure")
	}
}

// TestWatcherHandleEvent_RefundsAcrossTheRPCHop is the end-to-end #2501
// regression: it drives the REAL deliverWatchEvent → deliverTaskPrompt path with
// deliverPromptForTask returning a FLATTENED pre-flight error — what actually
// comes back over the control socket — and asserts the slot is refunded. This is
// the layer the source-level test skipped: it fails if the re-mint in
// deliverTaskPrompt is missing, even though the Manager still wraps correctly.
func TestWatcherHandleEvent_RefundsAcrossTheRPCHop(t *testing.T) {
	seedEnabledWatchTask(t, "cafe2501", "captain")

	// The manager's pre-flight error, flattened by net/rpc to a bare string — the
	// type is gone; only the marker text remains.
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (string, error) {
		wire := notAttempted(fmt.Errorf("target session %q is being deleted; prompt not delivered", "captain")).Error()
		return "", fmt.Errorf("%s", wire)
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	w := newRateSlotWatcher(t, "cafe2501", deliverWatchEvent)
	close(w.stopCh) // no drainer behind the assertions

	w.handleEvent("new issue #9", &tailBuffer{})

	if got := spentSlots(w); got != 0 {
		t.Fatalf("rate slots spent = %d, want 0.\n\n"+
			"A pre-flight failure came back flattened over net/rpc; deliverTaskPrompt must "+
			"re-mint notAttempted from the wire marker so the watcher refunds — otherwise the "+
			"budget drains over an outage, which is #2501.", got)
	}
	if got := w.queue.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1 (a failed delivery is queued for replay)", got)
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

// TestWatcherHandleEvent_RefundsWhenRootAgentRefusesPreFlight is the #3477
// regression at the layer the leak is actually paid for. Both refusals in
// deliverToReemergingRoot are pre-flight — the manager decides before a byte
// reaches any pane — so the slot the watcher reserved must come back. It did not,
// because the refusals said "event not delivered" while the wire protocol
// re-mints the sentinel from "prompt not delivered".
//
// Nothing here is a copied string: deliverPromptForTask routes through the REAL
// manager and reconstitutes its error from TEXT, which is exactly what net/rpc
// does on the daemon's own control socket. A regression that rewords either
// message back into a near-miss fails this test with spentSlots = 1 — the slot
// leak the issue reports, and the leak that drains a monitor task's per-minute
// budget for as long as the root outage lasts.
func TestWatcherHandleEvent_RefundsWhenRootAgentRefusesPreFlight(t *testing.T) {
	for i, tc := range rootRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoPath := tc.build(t)

			taskID := fmt.Sprintf("cafe3477%d", i)
			if err := task.AddTask(task.Task{
				ID:            taskID,
				Name:          "root-monitor",
				Prompt:        "Triage: {{line}}",
				WatchCmd:      "watch.sh",
				TargetSession: session.RootSessionTitle,
				ProjectPath:   repoPath,
				Enabled:       true,
				CreatedAt:     time.Now(),
			}); err != nil {
				t.Fatalf("seed task: %v", err)
			}

			// The daemon's own control socket, faithfully: the watch path reaches
			// the manager over net/rpc, which rebuilds the server error from its
			// text and destroys *notAttemptedError on the way.
			var wire string
			origDeliver := deliverPromptForTask
			deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
				status, err := manager.DeliverPrompt(req)
				if err == nil {
					return status, nil
				}
				wire = err.Error()
				return "", fmt.Errorf("%s", wire)
			}
			t.Cleanup(func() { deliverPromptForTask = origDeliver })

			w := newRateSlotWatcher(t, taskID, deliverWatchEvent)
			close(w.stopCh) // no drainer behind the assertions

			w.handleEvent("new issue #9", &tailBuffer{})

			// Prove the delivery took the root path under test. Without this a
			// fixture that drifted into some other refusal — one that already
			// carries the marker — would refund and "pass" while #3477 stood.
			if !strings.Contains(wire, tc.wantIn) {
				t.Fatalf("precondition: the delivery did not take the %q path; wire text: %q", tc.wantIn, wire)
			}
			if got := spentSlots(w); got != 0 {
				t.Fatalf("rate slots spent = %d, want 0.\n\n"+
					"The manager refused BEFORE sending anything, so nothing was delivered and the slot must be "+
					"refunded. It was not: the refusal came back over net/rpc as plain text, and that text did not "+
					"carry %q, so isNotAttemptedErr could not re-mint the sentinel and the watcher charged the "+
					"attempt anyway (#3477). Every retry during the outage charges another.\nwire text: %q",
					got, notDeliveredMarker, wire)
			}
			if got := w.queue.pendingCount(); got != 1 {
				t.Fatalf("pending = %d, want 1 (a failed delivery is queued for replay; refunding must not drop it)", got)
			}
		})
	}
}
