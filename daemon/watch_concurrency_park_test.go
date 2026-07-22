package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The watcher half of the concurrency limit (#1892): what happens to an event
// the cap refuses. The manager's admission decision is covered in
// watch_concurrency_test.go; here the delivery hook stands in for a saturated
// task so the watcher's live and replay paths can be driven deterministically.
//
// The contract these pin: a parked event is QUEUED, never dropped, and the
// backlog replays in emission order once slots free.

// cappedDeliver is a delivery hook that refuses while the task is "at its cap",
// exactly as deliverTaskPrompt does when the manager declines admission.
type cappedDeliver struct {
	mu           sync.Mutex
	atLimit      bool
	accepted     []string
	refused      int
	pauseStarted chan struct{}
	pauseRelease chan struct{}
}

func (d *cappedDeliver) deliver(taskID, line string) error {
	d.mu.Lock()
	if d.atLimit {
		d.refused++
		started, release := d.pauseStarted, d.pauseRelease
		d.pauseStarted, d.pauseRelease = nil, nil
		d.mu.Unlock()
		if started != nil {
			close(started)
			<-release
		}
		return errAtConcurrencyLimit
	}
	d.accepted = append(d.accepted, line)
	d.mu.Unlock()
	return nil
}

func (d *cappedDeliver) freeSlots() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.atLimit = false
}

func (d *cappedDeliver) delivered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.accepted...)
}

func (d *cappedDeliver) refusedAttempts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused
}

func (d *cappedDeliver) pauseNextAttempt() (<-chan struct{}, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	d.pauseStarted = started
	d.pauseRelease = release
	var once sync.Once
	return started, func() { once.Do(func() { close(release) }) }
}

// TestWatcherQueuesEventsParkedByConcurrencyLimit is the issue's core promise:
// "Watch events above the limit are durably queued in FIFO order and are not
// dropped." Every event fires while the task is at its cap, so every one must
// land in the durable backlog and replay in emission order once a session
// finishes — no drops, no reordering.
func TestWatcherQueuesEventsParkedByConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	script := `echo e1; echo e2; echo e3; echo e4; sleep 60`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab189201", script, dir)))
	cd := &cappedDeliver{atLimit: true}
	s.deliver = cd.deliver

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// The task is saturated: every event queues, nothing is delivered, nothing is
	// dropped on the floor.
	queueDir, _ := s.queueDir()
	waitUntil(t, 10*time.Second, "all four parked events to be queued", func() bool {
		return newEventQueue(queueDir, "ab189201").pendingCount() == 4
	})
	if got := cd.delivered(); len(got) != 0 {
		t.Fatalf("nothing may be delivered while the task is at its cap, got %v", got)
	}

	// A session finishes: the backlog drains in emission order.
	cd.freeSlots()
	waitUntil(t, 10*time.Second, "the parked backlog to replay in order", func() bool {
		got := cd.delivered()
		return len(got) == 4 && got[0] == "e1" && got[1] == "e2" && got[2] == "e3" && got[3] == "e4"
	})
}

// TestConcurrencyParkDoesNotAlarm: a task sitting at its own configured cap is
// healthy, so it must never raise the delivery-failure alarm (#1238). Treating a
// park as a failure would put a red banner in front of every user who set a cap
// and then saturated it — alarming on the feature working. It also clears an
// earlier failure run, exactly as the #1586 attach deferral does: the pipeline is
// now intentionally paused, and a stale failure would otherwise never clear while
// the cap held.
func TestConcurrencyParkDoesNotAlarm(t *testing.T) {
	w := &taskWatcher{taskID: "ab189203", name: "watch-ab189203"}
	t0 := time.Now()

	// A real failure opens an alarm window.
	w.recordDeliveryResult(t0, errAtConcurrencyLimit)
	if !w.deliverFailSince.IsZero() || w.deliverFailCount != 0 {
		t.Fatalf("a concurrency park must not open a failure run: since=%v count=%d", w.deliverFailSince, w.deliverFailCount)
	}

	// And it clears a failure run that a genuine outage had opened.
	w.recordDeliveryResult(t0, errors.New("target unreachable"))
	if w.deliverFailSince.IsZero() {
		t.Fatal("a real delivery failure must open a failure run")
	}
	w.recordDeliveryResult(t0.Add(time.Minute), errAtConcurrencyLimit)
	if !w.deliverFailSince.IsZero() || w.deliverFailCount != 0 {
		t.Fatalf("a concurrency park must clear a stale failure run: since=%v count=%d", w.deliverFailSince, w.deliverFailCount)
	}
}

// reservedRateSlots reports how many reservations the task's sliding rate window
// currently holds.
func watcherForTask(s *watcherSupervisor, taskID string) *taskWatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchers[taskID]
}

func reservedRateSlots(s *watcherSupervisor, taskID string) int {
	return watcherReservedRateSlots(watcherForTask(s, taskID))
}

func watcherReservedRateSlots(w *taskWatcher) int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.eventTimes)
}

// TestConcurrencyParkRefundsRateSlots guards the interaction between the cap and
// the 10/min rate limiter — the two limits this feature had to integrate with
// rather than stack on. A park delivers nothing, so it must refund the rate slot
// its attempt reserved.
//
// The drainer is where this bites. It reserves a slot for every replay attempt,
// and a task sitting at its cap retries on the base backoff for as long as the
// cap holds — so without a refund each retry permanently spends a slot that
// delivered nothing. Minutes of parking would exhaust the per-minute budget on
// pure refusals and then throttle the real deliveries the moment a session
// finished, which is the same reasoning that makes the #1586 attach deferral
// refund its slot.
//
// The live path burns at most one slot no matter how long the park lasts —
// handleEvent's FIFO gate routes every event after the first straight to the
// queue without consulting the limiter — so the drainer's refunds are what this
// asserts: a task parked over many retries holds NO reservations.
func TestConcurrencyParkRefundsRateSlots(t *testing.T) {
	dir := t.TempDir()
	script := `echo e1; echo e2; sleep 60`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab189202", script, dir)))
	cd := &cappedDeliver{atLimit: true}
	s.deliver = cd.deliver

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Both events park onto the durable backlog rather than being dropped.
	queueDir, _ := s.queueDir()
	waitUntil(t, 10*time.Second, "both parked events to be queued", func() bool {
		return newEventQueue(queueDir, "ab189202").pendingCount() == 2
	})
	if dropped := s.droppedEvents("ab189202"); dropped != 0 {
		t.Fatalf("a concurrency park must never drop an event; %d were dropped", dropped)
	}

	// Pause one retry inside delivery, after its slot was reserved but before the
	// refusal returns to the drainer and releases it. This is the window the old
	// fixed-delay assertion occasionally sampled on macOS: one in-flight slot is
	// legitimate and is not evidence of a leak.
	started, release := cd.pauseNextAttempt()
	defer release()
	<-started
	if held := reservedRateSlots(s, "ab189202"); held != 1 {
		t.Fatalf("in-flight parked retry holds %d rate reservations, want exactly 1", held)
	}

	// Judge completed retries, not an arbitrary wall-clock sample. Let ten more
	// refused attempts finish, then stop and join every watcher goroutine before
	// inspecting the captured watcher. Without the refund, completed attempts
	// accumulate slots; with it, the joined watcher holds none.
	completedBefore := cd.refusedAttempts()
	release()
	waitUntil(t, 10*time.Second, "ten more parked retries to complete", func() bool {
		return cd.refusedAttempts() >= completedBefore+10
	})
	w := watcherForTask(s, "ab189202")
	if w == nil {
		t.Fatal("watcher disappeared before the completed retries could be inspected")
	}
	s.Stop()
	if held := watcherReservedRateSlots(w); held != 0 {
		t.Fatalf("completed parked retries leaked %d rate reservation(s); want 0", held)
	}
}
