package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

// TestEventQueue_EnqueuePeekAdvanceRoundTrip pins the queue's core contract:
// strict FIFO across enqueue/peek/advance, and both files removed once the
// backlog fully drains (the steady healthy state is no queue files at all).
func TestEventQueue_EnqueuePeekAdvanceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120001")

	for i := 0; i < 3; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if got := q.pendingCount(); got != 3 {
		t.Fatalf("pending = %d, want 3", got)
	}

	for i := 0; i < 3; i++ {
		ev, n, ok, err := q.peek()
		if err != nil || !ok {
			t.Fatalf("peek %d: ok=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("event-%d", i); ev.Line != want {
			t.Fatalf("peek %d = %q, want %q (FIFO order)", i, ev.Line, want)
		}
		if err := q.advance(n); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
	}

	if got := q.pendingCount(); got != 0 {
		t.Fatalf("pending after drain = %d, want 0", got)
	}
	for _, p := range []string{q.path, q.curPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("drained queue must remove %s", filepath.Base(p))
		}
	}
}

// TestEventQueue_RecoversAcrossReopen: a partially drained backlog written by
// one eventQueue is recovered by a fresh one over the same files — the daemon
// restart / reload shape. The cursor keeps delivered events delivered.
func TestEventQueue_RecoversAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120002")
	for i := 0; i < 3; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	_, n, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	if err := q.advance(n); err != nil { // deliver event-0
		t.Fatalf("advance: %v", err)
	}

	reopened := newEventQueue(dir, "ab120002")
	if got := reopened.pendingCount(); got != 2 {
		t.Fatalf("recovered pending = %d, want 2", got)
	}
	ev, _, ok, err := reopened.peek()
	if err != nil || !ok {
		t.Fatalf("reopened peek: ok=%v err=%v", ok, err)
	}
	if ev.Line != "event-1" {
		t.Fatalf("reopened head = %q, want event-1 (cursor must survive reopen)", ev.Line)
	}
}

// TestEventQueue_CorruptCursorReplaysFromStart: an unparseable cursor resets
// to 0 — redelivering pending events (at-least-once) rather than guessing or
// silently losing the backlog.
func TestEventQueue_CorruptCursorReplaysFromStart(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120003")
	for i := 0; i < 2; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := os.WriteFile(q.curPath, []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("corrupt cursor: %v", err)
	}

	reopened := newEventQueue(dir, "ab120003")
	if got := reopened.pendingCount(); got != 2 {
		t.Fatalf("pending after corrupt cursor = %d, want 2 (replay from start)", got)
	}
	ev, _, ok, err := reopened.peek()
	if err != nil || !ok || ev.Line != "event-0" {
		t.Fatalf("head after corrupt cursor = %q ok=%v err=%v, want event-0", ev.Line, ok, err)
	}
}

// TestEventQueue_DropsOldestOverEventCap: the backlog is bounded; overflow
// drops the OLDEST pending events (newest are the actionable ones after an
// outage) and counts the drops.
func TestEventQueue_DropsOldestOverEventCap(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120004")
	for i := 0; i < watcherQueueMaxEvents+7; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if got := q.pendingCount(); got != watcherQueueMaxEvents {
		t.Fatalf("pending = %d, want the %d cap", got, watcherQueueMaxEvents)
	}
	ev, _, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	if ev.Line != "event-7" {
		t.Fatalf("head = %q, want event-7 (the 7 oldest must have been dropped)", ev.Line)
	}
	q.mu.Lock()
	dropped := q.dropped
	q.mu.Unlock()
	if dropped != 7 {
		t.Fatalf("dropped counter = %d, want 7", dropped)
	}
}

// flakyDeliver fails every delivery until healed, then records successes. It
// drives the outage→recovery shape end to end through a real watcher.
type flakyDeliver struct {
	mu      sync.Mutex
	healed  atomic.Bool
	success []string
}

func (d *flakyDeliver) deliver(taskID, line string) error {
	if !d.healed.Load() {
		return errors.New("target unreachable (outage)")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.success = append(d.success, line)
	return nil
}

func (d *flakyDeliver) delivered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.success...)
}

// TestWatcherQueuesFailedDeliveriesAndReplaysInOrder is the #1129 core: events
// fired while deliveries fail are queued — including newer events emitted
// while a backlog exists (FIFO gating; they must never jump the queue) — and
// once deliveries succeed the whole backlog replays in emission order. The
// drained queue leaves no files behind.
func TestWatcherQueuesFailedDeliveriesAndReplaysInOrder(t *testing.T) {
	dir := t.TempDir()
	script := `echo e1; echo e2; echo e3; sleep 60`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab130001", script, dir)))
	fd := &flakyDeliver{}
	s.deliver = fd.deliver

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// All three events fire during the outage; nothing may be delivered.
	queueDir, _ := s.queueDir()
	waitUntil(t, 10*time.Second, "all three events to be queued", func() bool {
		return newEventQueue(queueDir, "ab130001").pendingCount() == 3
	})
	if got := fd.delivered(); len(got) != 0 {
		t.Fatalf("nothing must be delivered during the outage, got %v", got)
	}

	// Outage ends: the drainer replays the backlog in emission order.
	fd.healed.Store(true)
	waitUntil(t, 10*time.Second, "backlog to replay in order", func() bool {
		got := fd.delivered()
		return len(got) == 3 && got[0] == "e1" && got[1] == "e2" && got[2] == "e3"
	})

	waitUntil(t, 10*time.Second, "drained queue files to be removed", func() bool {
		_, err1 := os.Stat(filepath.Join(queueDir, "ab130001.jsonl"))
		_, err2 := os.Stat(filepath.Join(queueDir, "ab130001.cursor"))
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
}

// TestWatcherBacklogSurvivesRestart: a backlog left behind by one supervisor
// (daemon lifetime) is recovered and replayed by the next — even though the
// script emits nothing new. This is the outage-spans-a-daemon-restart shape.
func TestWatcherBacklogSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// First daemon lifetime: deliveries fail, three events queue, then stop.
	s1, _ := newTestSupervisor(t, staticTasks(watchTask("ab130002", `echo e1; echo e2; echo e3; sleep 60`, dir)))
	fd1 := &flakyDeliver{}
	s1.deliver = fd1.deliver
	queueDir, _ := s1.queueDir()
	if err := s1.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "backlog to persist", func() bool {
		return newEventQueue(queueDir, "ab130002").pendingCount() == 3
	})
	s1.Stop()

	// Second daemon lifetime: same queue dir, healthy deliveries, and a
	// script that emits nothing — replay must come purely from the recovered
	// backlog.
	s2, _ := newTestSupervisor(t, staticTasks(watchTask("ab130002", `sleep 60`, dir)))
	fd2 := &flakyDeliver{}
	fd2.healed.Store(true)
	s2.deliver = fd2.deliver
	s2.queueDir = func() (string, error) { return queueDir, nil }
	if err := s2.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "recovered backlog to replay in order", func() bool {
		got := fd2.delivered()
		return len(got) == 3 && got[0] == "e1" && got[1] == "e2" && got[2] == "e3"
	})
}

// TestWatcherStopJoinsDrainer: a stop/reload arriving mid-backoff must return
// promptly — the drainer's waits are all stop-aware, extending the #769/#797
// nothing-wedges-a-stop contract to the replay path.
func TestWatcherStopJoinsDrainer(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab130003", `echo e1; sleep 60`, dir)))
	fd := &flakyDeliver{} // never healed: the drainer is stuck retrying
	s.deliver = fd.deliver
	s.drainBaseBackoff = time.Hour // park the drainer deep in a backoff wait
	s.drainMaxBackoff = time.Hour
	queueDir, _ := s.queueDir()

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "event to be queued", func() bool {
		return newEventQueue(queueDir, "ab130003").pendingCount() == 1
	})

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop wedged on the drainer's backoff wait")
	}
}

// TestWatcherReloadRemovesDeletedTaskQueue: a deleted task's backlog is
// removed on reload (it must not replay into a recreated namesake), while a
// merely-disabled task keeps its backlog for re-enable.
func TestWatcherReloadRemovesDeletedTaskQueue(t *testing.T) {
	dir := t.TempDir()
	disabled := task.Task{ID: "ab130004", Name: "w", WatchCmd: "sleep 60", ProjectPath: dir, Enabled: false}
	s, _ := newTestSupervisor(t, staticTasks(disabled))
	queueDir, _ := s.queueDir()

	for _, id := range []string{"ab130004", "deadbeef"} {
		q := newEventQueue(queueDir, id)
		if err := q.enqueue("pending"); err != nil {
			t.Fatalf("seed queue %s: %v", id, err)
		}
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, err := os.Stat(filepath.Join(queueDir, "ab130004.jsonl")); err != nil {
		t.Fatalf("disabled task's backlog must survive reload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(queueDir, "deadbeef.jsonl")); !os.IsNotExist(err) {
		t.Fatal("deleted task's backlog must be removed on reload")
	}
}
