package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// failLimitMarkerWrites makes every .limit-parked write fail while the JSONL
// stays appendable. A directory at the marker path refuses the atomic rename
// for any user, root included, which a read-only events directory does not.
func failLimitMarkerWrites(t *testing.T, q *eventQueue) {
	t.Helper()
	if err := os.Mkdir(q.limitPath, 0o755); err != nil {
		t.Fatalf("block limit marker path: %v", err)
	}
}

// TestEventQueue_MarkerWriteFailureSuppressesCapEviction pins the #4226 review
// finding: when the protected enqueue that discovers the limit pushes an
// ordinary backlog past a cap and its marker write fails, the queue must not
// evict the oldest distinct event. limitParked stays false on that failure, so
// cap enforcement read the backlog as unprotected before the reader could
// observe the fail-closed block.
func TestEventQueue_MarkerWriteFailureSuppressesCapEviction(t *testing.T) {
	cases := []struct {
		name       string
		seedCount  int
		line       func(i int) string
		closeFails bool
	}{
		{
			name:      "event cap",
			seedCount: watcherQueueMaxEvents,
			line:      func(i int) string { return fmt.Sprintf("event-%03d", i) },
		},
		{
			name:      "byte cap",
			seedCount: 4,
			line:      func(i int) string { return fmt.Sprintf("event-%03d-%s", i, strings.Repeat("x", 60*1024)) },
		},
		{
			// enqueue's full-write-then-close-failure branch enforces caps too.
			name:       "event cap after close failure",
			seedCount:  watcherQueueMaxEvents,
			line:       func(i int) string { return fmt.Sprintf("event-%03d", i) },
			closeFails: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newEventQueue(t.TempDir(), "marker-fail-cap")
			for i := 0; i < tc.seedCount; i++ {
				if err := q.enqueue(tc.line(i)); err != nil {
					t.Fatalf("seed ordinary event %d: %v", i, err)
				}
			}
			if got := q.pendingCount(); got != tc.seedCount {
				t.Fatalf("ordinary backlog already evicted while seeding: pending=%d", got)
			}
			failLimitMarkerWrites(t, q)
			if tc.closeFails {
				q.appendRecord = func(path string, rec []byte) (int, error) {
					n, err := appendRecordToFile(path, rec)
					if err != nil {
						return n, err
					}
					return n, errors.New("deferred writeback failed on close")
				}
			}

			retained, err := q.enqueueWithParkedStatus(tc.line(tc.seedCount), true, false)
			if err == nil {
				t.Fatal("a failed marker write must still surface an error")
			}
			if !retained {
				t.Fatal("the parked event was dropped although the backlog stayed appendable")
			}
			if got, want := q.pendingCount(), tc.seedCount+1; got != want {
				t.Fatalf("cap enforcement evicted a distinct event after the marker write failed: pending=%d, want %d", got, want)
			}
			ev, _, ok, err := q.peek()
			if err != nil || !ok {
				t.Fatalf("peek head: ok=%v err=%v", ok, err)
			}
			if ev.Line != tc.line(0) {
				t.Fatalf("oldest ordinary event was evicted: head=%.20q", ev.Line)
			}
			if blocked, unknown := q.limitBackpressureState(); !blocked || !unknown {
				t.Fatalf("reader must block fail-closed over the unprotected backlog: blocked=%v unknown=%v", blocked, unknown)
			}
		})
	}
}

// TestEventQueue_MarkerWriteFailureRetainsAgainstRetentionPolicy covers the
// sibling reader of the same state: retainLimitParked drives the drainer's age
// expiry and the stop-time pipe decision, and must not answer "unprotected"
// while a failed marker write still guards a backlog. It must not outlive that
// backlog either — an ordinary event entering the empty queue inherits nothing.
func TestEventQueue_MarkerWriteFailureRetainsAgainstRetentionPolicy(t *testing.T) {
	q := newEventQueue(t.TempDir(), "marker-fail-retain")
	failLimitMarkerWrites(t, q)
	if _, err := q.enqueueWithParkedStatus("held-for-limit", true, false); err == nil {
		t.Fatal("a failed marker write must still surface an error")
	}
	if !q.retainLimitParked() {
		t.Fatal("a backlog guarded by a failed marker write was reported unprotected")
	}

	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	advanceEventQueue(t, q, cursor)
	if q.retainLimitParked() {
		t.Fatal("protection outlived the backlog the failed marker write guarded")
	}

	// A parked enqueue that fails both its marker write and its append leaves
	// the failure recorded on an empty queue. The next ordinary event must
	// start unprotected rather than adopt it once pending becomes nonzero.
	failLimitMarkerWrites(t, q)
	q.appendRecord = func(string, []byte) (int, error) { return 0, errors.New("disk full") }
	if retained, err := q.enqueueWithParkedStatus("lost-parked", true, false); retained || err == nil {
		t.Fatalf("append failure was not reported as a drop: retained=%v err=%v", retained, err)
	}
	q.appendRecord = appendRecordToFile
	if err := q.enqueue("ordinary-after-drain"); err != nil {
		t.Fatalf("ordinary enqueue: %v", err)
	}
	if q.retainLimitParked() {
		t.Fatal("an ordinary event inherited protection from a moot marker failure")
	}
	if blocked, _ := q.limitBackpressureState(); blocked {
		t.Fatal("an ordinary event inherited the fail-closed block from a moot marker failure")
	}
}

// TestWatcherDrainDeliversAgedEventWhenMarkerWriteFailed drives the drain loop
// against that state: the limit has lifted, so the live target observation no
// longer protects the aged head. A durable marker would have it delivered; a
// marker write that failed must not turn it into an ordinary 72-hour expiry.
func TestWatcherDrainDeliversAgedEventWhenMarkerWriteFailed(t *testing.T) {
	const taskID = "a4226c01"
	queue := newEventQueue(t.TempDir(), taskID)
	failLimitMarkerWrites(t, queue)
	queue.now = func() time.Time { return time.Now().Add(-73 * time.Hour) }
	if _, err := queue.enqueueWithParkedStatus("aged-parked-event", true, false); err == nil {
		t.Fatal("a failed marker write must still surface an error")
	}
	if got := queue.pendingCount(); got != 1 {
		t.Fatalf("parked event not retained: pending=%d", got)
	}

	s := newWatcherSupervisor()
	s.setStatus = func(string, string) {}
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.observeTargetLimit = func(string) (bool, error) { return false, nil }
	s.queueMaxAge = 72 * time.Hour
	s.drainBaseBackoff = time.Millisecond
	var mu sync.Mutex
	var delivered []string
	s.deliver = func(_, line string, _ watchDeliveryOptions) error {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, line)
		return nil
	}
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: taskID, sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	go w.drainLoop()
	drained := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		close(stopCh)
		<-drained
		t.Fatal("drain loop did not finish the one-event backlog")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 || delivered[0] != "aged-parked-event" {
		t.Fatalf("aged event guarded by a failed marker write was expired instead of delivered: delivered=%q", delivered)
	}
}
