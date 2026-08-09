package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestEventQueue_CompactionStopsBeforeQueueRenameWhenCursorBarrierFails proves
// the cursor reset is durable before compaction may publish a shortened queue.
// Otherwise a crash can preserve the new queue beside the old nonzero cursor;
// if that offset is also a record boundary in the new layout, reload silently
// skips pending events.
func TestEventQueue_CompactionStopsBeforeQueueRenameWhenCursorBarrierFails(t *testing.T) {
	previousThreshold := watcherQueueCompactBytes
	watcherQueueCompactBytes = 1
	t.Cleanup(func() { watcherQueueCompactBytes = previousThreshold })

	dir := t.TempDir()
	q := newEventQueue(dir, "ab313301")
	for _, line := range []string{"delivered", "pending"} {
		if err := q.enqueue(line); err != nil {
			t.Fatalf("enqueue %q: %v", line, err)
		}
	}
	originalQueue, err := os.ReadFile(q.path)
	if err != nil {
		t.Fatalf("read original queue: %v", err)
	}

	barrierCalls := 0
	q.syncDirectory = func(string) error {
		barrierCalls++
		return errors.New("simulated cursor-directory sync failure")
	}
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	advanced, err := q.advance(cursor)
	if err != nil || !advanced {
		t.Fatalf("advance after rejected compaction: advanced=%v err=%v", advanced, err)
	}
	if barrierCalls != 1 {
		t.Fatalf("strict directory barrier calls = %d, want 1 before queue rename", barrierCalls)
	}

	queueAfterFailure, err := os.ReadFile(q.path)
	if err != nil {
		t.Fatalf("read queue after rejected compaction: %v", err)
	}
	if string(queueAfterFailure) != string(originalQueue) {
		t.Fatal("compaction replaced the queue before the cursor-directory barrier succeeded")
	}
	reopened := newEventQueue(dir, "ab313301")
	event, _, ok, err := reopened.peek()
	if err != nil || !ok || event.Line != "pending" {
		t.Fatalf("reopened head = %q ok=%v err=%v, want pending", event.Line, ok, err)
	}
}

// TestEventQueue_CompactionCursorFenceCannotPersistOrphanTemp models a crash
// immediately after the cursor-directory fence. The fence must run before the
// compact temporary file is created, or that fsync also makes the temporary
// directory entry durable and startup has no task-scoped way to reclaim it.
func TestEventQueue_CompactionCursorFenceCannotPersistOrphanTemp(t *testing.T) {
	previousThreshold := watcherQueueCompactBytes
	watcherQueueCompactBytes = 1
	t.Cleanup(func() { watcherQueueCompactBytes = previousThreshold })

	dir := t.TempDir()
	const taskID = "ab313302"
	q := newEventQueue(dir, taskID)
	for _, line := range []string{"delivered", "pending"} {
		if err := q.enqueue(line); err != nil {
			t.Fatalf("enqueue %q: %v", line, err)
		}
	}
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}

	const simulatedCrash = "simulated crash after cursor-directory fence"
	q.syncDirectory = func(dir string) error {
		if err := syncEventQueueDirectory(dir); err != nil {
			return err
		}
		panic(simulatedCrash)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = q.advance(cursor)
	}()
	if recovered != simulatedCrash {
		t.Fatalf("recovered panic = %v, want %q", recovered, simulatedCrash)
	}

	matches, err := filepath.Glob(filepath.Join(dir, taskID+".compact-*"))
	if err != nil {
		t.Fatalf("glob compact files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("cursor fence made abandoned compact files durable: %v", matches)
	}
}
