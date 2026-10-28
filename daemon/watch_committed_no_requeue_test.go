package daemon

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A delivery that reports a committed outcome (a retained auto-create row, an
// unknown start that may have submitted the prompt) must not enter the automatic
// replay queue: replaying it could provision a second session or paste the
// prompt twice. The hook is called exactly once per event and nothing is queued.
func TestWatcherDoesNotQueueCommittedDeliveries(t *testing.T) {
	dir := t.TempDir()
	script := `echo e1; echo e2; sleep 60`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab189211", script, dir)))
	var calls atomic.Int32
	s.deliver = adaptWatchDelivery(func(taskID, line string) error {
		calls.Add(1)
		return &mutationCommittedError{err: errors.New("auto-create committed but start outcome unknown")}
	})

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "both events to be attempted", func() bool { return calls.Load() >= 2 })

	// Give a (wrongly) queued head time to be replayed before asserting.
	time.Sleep(500 * time.Millisecond)
	queueDir, _ := s.queueDir()
	if n := newEventQueue(queueDir, "ab189211").pendingCount(); n != 0 {
		t.Fatalf("a committed delivery must not be queued for replay, %d pending", n)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("each committed event must be attempted exactly once, got %d attempts", got)
	}
}
