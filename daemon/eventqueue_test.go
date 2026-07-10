package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

func advanceEventQueue(t *testing.T, q *eventQueue, cursor eventQueueCursor) {
	t.Helper()
	advanced, err := q.advance(cursor)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !advanced {
		t.Fatal("advance skipped the current queued event")
	}
}

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
		ev, cursor, ok, err := q.peek()
		if err != nil || !ok {
			t.Fatalf("peek %d: ok=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("event-%d", i); ev.Line != want {
			t.Fatalf("peek %d = %q, want %q (FIFO order)", i, ev.Line, want)
		}
		advanceEventQueue(t, q, cursor)
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

// TestEventQueue_RemoveFailureDoesNotLoseLaterEnqueue covers #1433: if the
// final queue-file remove fails, advance must leave the delivered-prefix state
// intact. A later enqueue appends behind that prefix; the next peek must see
// the later event, not re-read the already delivered record and then unlink
// the file containing both records.
func TestEventQueue_RemoveFailureDoesNotLoseLaterEnqueue(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120009")
	if err := q.enqueue("delivered-before-remove-failure"); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek first: ok=%v err=%v", ok, err)
	}

	removeErr := errors.New("simulated remove failure")
	failNextRemove := true
	q.remove = func(path string) error {
		if path == q.path && failNextRemove {
			failNextRemove = false
			return removeErr
		}
		return os.Remove(path)
	}
	advanced, err := q.advance(cursor)
	if !errors.Is(err, removeErr) {
		t.Fatalf("advance error = %v, want %v", err, removeErr)
	}
	if advanced {
		t.Fatal("advance reported success after queue-file removal failed")
	}

	if err := q.enqueue("new-after-remove-failure"); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	ev, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek second: ok=%v err=%v", ok, err)
	}
	if ev.Line != "new-after-remove-failure" {
		t.Fatalf("head after failed remove = %q, want the later event", ev.Line)
	}
	advanceEventQueue(t, q, cursor)
	if got := q.pendingCount(); got != 0 {
		t.Fatalf("pending after second advance = %d, want 0", got)
	}
	if _, err := os.Stat(q.path); !os.IsNotExist(err) {
		t.Fatalf("queue file after drain = %v, want removed", err)
	}
}

func TestEventQueue_CursorRemoveFailureResetsBeforeFreshEnqueue(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120010")
	for _, line := range []string{"first", "second"} {
		if err := q.enqueue(line); err != nil {
			t.Fatalf("enqueue %q: %v", line, err)
		}
	}
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek first: ok=%v err=%v", ok, err)
	}
	advanceEventQueue(t, q, cursor)

	failCursorRemove := true
	q.remove = func(path string) error {
		if path == q.curPath && failCursorRemove {
			failCursorRemove = false
			return errors.New("simulated cursor remove failure")
		}
		return os.Remove(path)
	}
	_, cursor, ok, err = q.peek()
	if err != nil || !ok {
		t.Fatalf("peek second: ok=%v err=%v", ok, err)
	}
	advanceEventQueue(t, q, cursor)

	raw, err := os.ReadFile(q.curPath)
	if err != nil {
		t.Fatalf("read reset cursor: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "0" {
		t.Fatalf("cursor after failed remove = %q, want 0", got)
	}
	if err := q.enqueue("after-drain"); err != nil {
		t.Fatalf("enqueue after drain: %v", err)
	}

	reopened := newEventQueue(dir, "ab120010")
	if got := reopened.pendingCount(); got != 1 {
		t.Fatalf("reopened pending = %d, want 1", got)
	}
	ev, _, ok, err := reopened.peek()
	if err != nil || !ok || ev.Line != "after-drain" {
		t.Fatalf("reopened head = %q ok=%v err=%v, want after-drain", ev.Line, ok, err)
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
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	advanceEventQueue(t, q, cursor) // deliver event-0

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

// TestEventQueue_MaxLineEscapeHeavyRoundTrip is the Greptile P1 regression on
// #1136: a maxWatchLineBytes line whose JSON-escaped record is several times
// larger (quotes, backslashes, control chars, multibyte at the boundary) must
// survive enqueue → reopen → peek → advance intact. The original load() used a
// bufio.Scanner whose token cap sat just above the RAW line size, so an
// escape-inflated record silently ended the recovery scan — losing exactly the
// events durability promised to keep.
func TestEventQueue_MaxLineEscapeHeavyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120005")

	// Escape-heavy body: every 4-byte unit escapes to ~14 JSON bytes
	// (`"` → \", `\` → \\, tab → \t, 0x01 → ), inflating well past any
	// 64KB token cap; a multibyte rune lands at the very end of the cap.
	unit := "\"\\\t\x01"
	body := strings.Repeat(unit, (maxWatchLineBytes-4)/len(unit))
	line := body + "…" // 3-byte rune at the tail
	if len(line) > maxWatchLineBytes {
		t.Fatalf("test bug: line %d bytes exceeds the %d cap", len(line), maxWatchLineBytes)
	}

	if err := q.enqueue("first"); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	if err := q.enqueue(line); err != nil {
		t.Fatalf("enqueue max line: %v", err)
	}
	if err := q.enqueue("last"); err != nil {
		t.Fatalf("enqueue last: %v", err)
	}

	// Reopen: recovery must count all three records despite the oversized one.
	reopened := newEventQueue(dir, "ab120005")
	if got := reopened.pendingCount(); got != 3 {
		t.Fatalf("recovered pending = %d, want 3 (oversized record must not end the scan)", got)
	}
	for _, want := range []string{"first", line, "last"} {
		ev, cursor, ok, err := reopened.peek()
		if err != nil || !ok {
			t.Fatalf("peek: ok=%v err=%v", ok, err)
		}
		if ev.Line != want {
			t.Fatalf("replayed line corrupted: got %d bytes, want %d bytes (first divergence at %d)",
				len(ev.Line), len(want), firstDivergence(ev.Line, want))
		}
		advanceEventQueue(t, reopened, cursor)
	}
}

func firstDivergence(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestEventQueue_TornTrailingRecordTruncated: a torn append (daemon died
// mid-write, no trailing newline) is discarded on reopen and the file
// truncated back to a record boundary, so the next enqueue cannot glue two
// records into one corrupt line.
func TestEventQueue_TornTrailingRecordTruncated(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120006")
	if err := q.enqueue("whole"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f, err := os.OpenFile(q.path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"ts":"2026-07-04T`); err != nil { // no newline: torn
		t.Fatalf("write torn tail: %v", err)
	}
	_ = f.Close()

	reopened := newEventQueue(dir, "ab120006")
	if got := reopened.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1 (torn tail is not an event)", got)
	}
	if err := reopened.enqueue("after"); err != nil {
		t.Fatalf("enqueue after torn tail: %v", err)
	}
	ev, cursor, ok, err := reopened.peek()
	if err != nil || !ok || ev.Line != "whole" {
		t.Fatalf("head = %q ok=%v err=%v, want whole", ev.Line, ok, err)
	}
	advanceEventQueue(t, reopened, cursor)
	ev, _, ok, err = reopened.peek()
	if err != nil || !ok || ev.Line != "after" {
		t.Fatalf("second record = %q ok=%v err=%v, want intact %q", ev.Line, ok, err, "after")
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

// TestEventQueue_StaleAdvanceAfterOverflowDropDoesNotSkipBacklog covers the
// #1262 race: replay peeks the head, live enqueues overflow-drop that same
// head, then the stale replay advance arrives. The stale advance must not
// move the new head; every remaining queued event must still replay once.
func TestEventQueue_StaleAdvanceAfterOverflowDropDoesNotSkipBacklog(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "ab120008")
	for i := 0; i < 2; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	ev, stale, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek stale head: ok=%v err=%v", ok, err)
	}
	if ev.Line != "event-0" {
		t.Fatalf("stale head = %q, want event-0", ev.Line)
	}

	for i := 2; i <= watcherQueueMaxEvents; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if got := q.pendingCount(); got != watcherQueueMaxEvents {
		t.Fatalf("pending after overflow = %d, want %d", got, watcherQueueMaxEvents)
	}

	advanced, err := q.advance(stale)
	if err != nil {
		t.Fatalf("stale advance: %v", err)
	}
	if advanced {
		t.Fatal("stale advance consumed the new queue head")
	}

	for i := 1; i <= watcherQueueMaxEvents; i++ {
		ev, cursor, ok, err := q.peek()
		if err != nil || !ok {
			t.Fatalf("peek event-%d: ok=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("event-%d", i); ev.Line != want {
			t.Fatalf("backlog skipped/reordered: got %q, want %q", ev.Line, want)
		}
		advanceEventQueue(t, q, cursor)
	}
	if got := q.pendingCount(); got != 0 {
		t.Fatalf("pending after replay = %d, want 0", got)
	}
}

// TestEventQueue_CompactsDeliveredPrefix (#1129 PR 4): once the delivered
// prefix outgrows watcherQueueCompactBytes, advance rewrites the file down to
// its pending suffix — offset back to 0, remaining events intact and in
// order, and the state survives a reopen (the crash-recovery path).
func TestEventQueue_CompactsDeliveredPrefix(t *testing.T) {
	prev := watcherQueueCompactBytes
	watcherQueueCompactBytes = 128
	t.Cleanup(func() { watcherQueueCompactBytes = prev })

	dir := t.TempDir()
	q := newEventQueue(dir, "ab120007")
	for i := 0; i < 10; i++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	sizeBefore := q.size

	// Consume until the delivered prefix crosses the threshold; compaction
	// must kick in and reset the offset while events remain pending.
	for i := 0; i < 5; i++ {
		_, cursor, ok, err := q.peek()
		if err != nil || !ok {
			t.Fatalf("peek %d: ok=%v err=%v", i, ok, err)
		}
		advanceEventQueue(t, q, cursor)
	}

	q.mu.Lock()
	offset, size, pending := q.offset, q.size, q.pending
	q.mu.Unlock()
	if pending != 5 {
		t.Fatalf("pending = %d, want 5", pending)
	}
	// Compaction fires whenever the delivered prefix crosses the threshold,
	// so the surviving prefix is always bounded by it (advances after the
	// last compaction may legitimately re-accumulate up to the threshold).
	if offset > watcherQueueCompactBytes {
		t.Fatalf("offset = %d, want <= %d (compaction must bound the delivered prefix)", offset, watcherQueueCompactBytes)
	}
	if size >= sizeBefore {
		t.Fatalf("file did not shrink: %d -> %d bytes", sizeBefore, size)
	}

	// The compacted queue must survive a reopen and drain the survivors in
	// order.
	reopened := newEventQueue(dir, "ab120007")
	if got := reopened.pendingCount(); got != 5 {
		t.Fatalf("reopened pending = %d, want 5", got)
	}
	for i := 5; i < 10; i++ {
		ev, cursor, ok, err := reopened.peek()
		if err != nil || !ok {
			t.Fatalf("reopened peek %d: ok=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("event-%d", i); ev.Line != want {
			t.Fatalf("compaction reordered/corrupted: got %q, want %q", ev.Line, want)
		}
		advanceEventQueue(t, reopened, cursor)
	}
}

// TestEventQueue_RecoversFromStaleCursorAfterCompactionCrash (#1537): a daemon
// crash AFTER compaction's rename but BEFORE the cursor write used to pair the
// small compacted file with a PRE-compaction offset pointing into the middle of
// a record. load()'s old off<=size check passed it, every subsequent read then
// failed with "corrupt event record", and the drainer parked forever. The queue
// must instead recover by replaying the pending suffix from the start.
func TestEventQueue_RecoversFromStaleCursorAfterCompactionCrash(t *testing.T) {
	dir := t.TempDir()

	// Hand-build the exact on-disk state a crash between compaction's rename and
	// its cursor write leaves behind: the queue file holds ONLY the pending
	// suffix (events 5..9, as if the delivered prefix was already compacted
	// away), while the cursor file still holds a stale PRE-compaction offset that
	// lands mid-record within the now-smaller file. Enqueueing the suffix into a
	// fresh queue produces that compacted file without depending on real
	// compaction's byte-threshold timing.
	q0 := newEventQueue(dir, "ab153701")
	for i := 5; i < 10; i++ {
		if err := q0.enqueue(fmt.Sprintf("event-%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	q0.mu.Lock()
	size := q0.size
	q0.mu.Unlock()

	// Offset 3 sits inside event-5's record: 0 < 3 < size, and the byte before
	// it is not '\n', so it clears off<=size but is not a record boundary.
	staleOffset := int64(3)
	if staleOffset >= size {
		t.Fatalf("test bug: stale offset %d must be < compacted size %d", staleOffset, size)
	}
	curPath := filepath.Join(dir, "ab153701.cursor")
	if err := os.WriteFile(curPath, []byte(fmt.Sprintf("%d", staleOffset)), 0644); err != nil {
		t.Fatalf("write stale cursor: %v", err)
	}

	// Reopen: the queue must recover and redeliver events 5..9 in order rather
	// than parking the drainer on an unreadable head.
	reopened := newEventQueue(dir, "ab153701")
	if got := reopened.pendingCount(); got != 5 {
		t.Fatalf("recovered pending = %d, want 5", got)
	}
	for i := 5; i < 10; i++ {
		ev, cursor, ok, err := reopened.peek()
		if err != nil || !ok {
			t.Fatalf("recovered peek %d: ok=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("event-%d", i); ev.Line != want {
			t.Fatalf("recovery reordered/corrupted: got %q, want %q", ev.Line, want)
		}
		advanceEventQueue(t, reopened, cursor)
	}
	if got := reopened.pendingCount(); got != 0 {
		t.Fatalf("pending after recovery drain = %d, want 0", got)
	}
}

// TestWatcherDrainExpiresAgedEvents (#1129 PR 4): queued events older than
// the retention bound are expired at drain time — never delivered — and a
// fresh event still replays. Expiry empties the queue files like a normal
// drain.
func TestWatcherDrainExpiresAgedEvents(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab130005", `sleep 60`, dir)))
	fd := &flakyDeliver{}
	fd.healed.Store(true)
	s.deliver = fd.deliver
	// A generous bound: the "fresh" event has the whole window to be delivered,
	// which no CI stall approaches — while the stale events are backdated an
	// hour, an enormous margin past it. The seam replaces the old
	// "200ms sleep > 150ms max-age" race, which crossed the boundary
	// unpredictably under arm64/CI load.
	s.queueMaxAge = 5 * time.Second
	queueDir, _ := s.queueDir()

	// Two events queued before the watcher exists, backdated an hour so they
	// are unambiguously past the retention bound — no real-time sleep.
	seed := newEventQueue(queueDir, "ab130005")
	seed.now = func() time.Time { return time.Now().Add(-time.Hour) }
	for _, line := range []string{"stale-1", "stale-2"} {
		if err := seed.enqueue(line); err != nil {
			t.Fatalf("seed enqueue: %v", err)
		}
	}
	// A fresh third event stamped at real now must still be delivered.
	seed.now = time.Now
	if err := seed.enqueue("fresh"); err != nil {
		t.Fatalf("seed enqueue fresh: %v", err)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	waitUntil(t, 10*time.Second, "the fresh event to replay and stale ones to expire", func() bool {
		got := fd.delivered()
		return len(got) == 1 && got[0] == "fresh"
	})
	waitUntil(t, 10*time.Second, "the expired queue files to be removed", func() bool {
		_, err := os.Stat(filepath.Join(queueDir, "ab130005.jsonl"))
		return os.IsNotExist(err)
	})
	if got := fd.delivered(); len(got) != 1 {
		t.Fatalf("stale events must never be delivered, got %v", got)
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
