package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression tests for #3242: a failed queue load is an UNKNOWN state, not an
// empty queue. On the buggy path, load swallowed Stat/Open/Seek/scan failures
// and answered pending=0 beside a populated file. One later enqueue made
// pending=1, so the drainer's first advance "drained" the queue and unlinked
// the file — deleting every undelivered record it never enumerated.

// denyAccess chmods path to 0 and registers a cleanup that restores mode. It
// skips the test on runners where the permission bits do not deny access (root
// bypasses the DAC check — same honest-skip discipline as
// TestListDirectory_PermissionDeniedIsAnErrorNotAnEmptyList): probe must be a
// file that is unopenable while the denial holds.
func denyAccess(t *testing.T, path, probe string, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod %s to 0: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
	if f, err := os.Open(probe); err == nil {
		_ = f.Close()
		t.Skip("this runner can open a mode-0 path (running as root?); a load failure is unobservable here")
	}
}

// TestEventQueue_LoadFailureThenReplayMustNotDeleteBacklog is THE #3242
// sequence, Open-failure flavor: the daemon restarts while the queue file has a
// transient read failure (Stat succeeds, Open is denied), storage recovers, one
// failed live delivery is appended, and the drainer replays. Replay must never
// remove records the load could not enumerate.
func TestEventQueue_LoadFailureThenReplayMustNotDeleteBacklog(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "ab324201")
	for _, line := range []string{"old-1", "old-2"} {
		if err := seed.enqueue(line); err != nil {
			t.Fatalf("seed enqueue %q: %v", line, err)
		}
	}

	denyAccess(t, seed.path, seed.path, 0o644)
	q := newEventQueue(dir, "ab324201")

	// Storage recovers, then a failed live delivery lands one more event.
	if err := os.Chmod(seed.path, 0o644); err != nil {
		t.Fatalf("restore queue file mode: %v", err)
	}
	if err := q.enqueue("new-1"); err != nil {
		t.Fatalf("enqueue after recovery: %v", err)
	}

	// Replay exactly one event. On the buggy path pending was fabricated as 0
	// at load and became 1 on the enqueue, so this single advance hit zero and
	// unlinked the file with old-2 and new-1 still undelivered inside it.
	ev, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek head after recovery: ok=%v err=%v", ok, err)
	}
	if ev.Line != "old-1" {
		t.Fatalf("head after recovery = %q, want the oldest undelivered record %q", ev.Line, "old-1")
	}
	advanceEventQueue(t, q, cursor)
	if _, err := os.Stat(q.path); err != nil {
		t.Fatalf("queue file after one replayed event: %v — replay deleted records it never enumerated", err)
	}

	// The rest of the backlog must still drain in FIFO order, nothing skipped.
	got := drainAllEvents(t, q)
	want := []string{"old-2", "new-1"}
	if len(got) != len(want) {
		t.Fatalf("drained %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drained %v, want %v (FIFO)", got, want)
		}
	}
}

// TestEventQueue_StatFailureThenRecoveryPreservesCursorAndBacklog is the
// Stat-failure flavor: the whole queue directory is unreachable across a
// restart, so load learns nothing at all — not even the file size. After
// recovery, the persisted cursor must still be honored: replay resumes at the
// first UNDELIVERED record, without redelivering the delivered prefix and
// without deleting the backlog. On the buggy path the all-zero state made the
// next enqueue treat the queue as brand new: it removed the live cursor, then
// replayed from byte 0.
func TestEventQueue_StatFailureThenRecoveryPreservesCursorAndBacklog(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events")
	if err := os.MkdirAll(events, 0o755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	seed := newEventQueue(events, "ab324202")
	for _, line := range []string{"old-1", "old-2", "old-3"} {
		if err := seed.enqueue(line); err != nil {
			t.Fatalf("seed enqueue %q: %v", line, err)
		}
	}
	// Deliver old-1 so a real cursor is persisted beside a pending backlog.
	ev, cursor, ok, err := seed.peek()
	if err != nil || !ok || ev.Line != "old-1" {
		t.Fatalf("seed peek: line=%q ok=%v err=%v", ev.Line, ok, err)
	}
	advanceEventQueue(t, seed, cursor)

	denyAccess(t, events, seed.path, 0o755)
	q := newEventQueue(events, "ab324202")

	if err := os.Chmod(events, 0o755); err != nil {
		t.Fatalf("restore events dir mode: %v", err)
	}
	if err := q.enqueue("new-1"); err != nil {
		t.Fatalf("enqueue after recovery: %v", err)
	}

	got := drainAllEvents(t, q)
	want := []string{"old-2", "old-3", "new-1"}
	if len(got) != len(want) {
		t.Fatalf("drained %v, want %v (cursor lost or backlog deleted)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drained %v, want %v (replay must resume at the cursor, in FIFO order)", got, want)
		}
	}
}

// TestEventQueue_LoadFailurePeekReportsErrorNotEmpty pins the drainer-facing
// contract: while the load has failed and storage is still unreadable, peek
// must surface an error (the drainer parks on it) — never answer as a
// successfully drained empty queue. A failed read is not an empty result.
func TestEventQueue_LoadFailurePeekReportsErrorNotEmpty(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "ab324203")
	if err := seed.enqueue("only"); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}

	denyAccess(t, seed.path, seed.path, 0o644)
	q := newEventQueue(dir, "ab324203")

	if _, _, ok, err := q.peek(); err == nil {
		t.Fatalf("peek answered ok=%v err=nil on a queue it could not read; a failed load must surface an error, not an empty queue", ok)
	}
}

// TestWatcherDrainRecoversAfterLoadFailureWithoutNewEvents pins the recovery
// path for #3242 against the #1128 never-a-permanent-give-up discipline: a
// backlog behind an unreadable queue file must replay once storage heals, even
// though the script emits nothing new — the drainer itself must keep
// re-attempting recovery instead of parking forever after its first failed
// peek (a backlog stranded until the next daemon reload is a silent outage).
func TestWatcherDrainRecoversAfterLoadFailureWithoutNewEvents(t *testing.T) {
	dir := t.TempDir()
	// Shrink the load-retry throttle to the harness's fast drain cadence so
	// recovery latency is test-speed, not the production 5s.
	oldInterval := eventQueueLoadRetryInterval
	eventQueueLoadRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { eventQueueLoadRetryInterval = oldInterval })

	// First daemon lifetime: deliveries fail, three events queue, then stop.
	s1, _ := newTestSupervisor(t, staticTasks(watchTask("ab324205", `echo e1; echo e2; echo e3; sleep 60`, dir)))
	fd1 := &flakyDeliver{}
	s1.deliver = fd1.deliver
	queueDir, _ := s1.queueDir()
	if err := s1.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "backlog to persist", func() bool {
		return newEventQueue(queueDir, "ab324205").pendingCount() == 3
	})
	s1.Stop()

	// Second daemon lifetime starts while the queue file is unreadable. The
	// script emits nothing and touches a sentinel, so "sentinel exists" proves
	// the watcher's run loop — and its backlog gate — already ran with the
	// load still failing.
	queuePath := filepath.Join(queueDir, "ab324205.jsonl")
	denyAccess(t, queuePath, queuePath, 0o644)
	s2, _ := newTestSupervisor(t, staticTasks(watchTask("ab324205", `touch "`+dir+`/started"; sleep 60`, dir)))
	fd2 := &flakyDeliver{}
	fd2.healed.Store(true)
	s2.deliver = fd2.deliver
	s2.queueDir = func() (string, error) { return queueDir, nil }
	if err := s2.Reload(); err != nil {
		t.Fatalf("Reload second lifetime: %v", err)
	}
	waitUntil(t, 10*time.Second, "watch script to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	// Storage heals. No live event will ever arrive to trigger a retry — the
	// drainer must recover the backlog on its own cadence and replay in order.
	if err := os.Chmod(queuePath, 0o644); err != nil {
		t.Fatalf("restore queue file mode: %v", err)
	}
	waitUntil(t, 10*time.Second, "recovered backlog to replay after storage heals", func() bool {
		got := fd2.delivered()
		return len(got) == 3 && got[0] == "e1" && got[1] == "e2" && got[2] == "e3"
	})
}

// TestEventQueue_LoadFailureErrorsCarryTypedSentinel pins the classification
// contract the drainer relies on (#3242 review round 2): every load-failure
// refusal must be errors.Is-identifiable from the error value alone, because
// the drainer classifies transient-outage vs corruption from the error it
// already holds — re-reading queue state after the fact races a concurrent
// heal, and a stale read there would park a recovered backlog permanently.
func TestEventQueue_LoadFailureErrorsCarryTypedSentinel(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "ab324208")
	if err := seed.enqueue("only"); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}
	_, cursor, ok, err := seed.peek()
	if err != nil || !ok {
		t.Fatalf("seed peek: ok=%v err=%v", ok, err)
	}

	denyAccess(t, seed.path, seed.path, 0o644)
	q := newEventQueue(dir, "ab324208")

	if _, _, _, err := q.peek(); !errors.Is(err, errEventQueueLoadFailed) {
		t.Fatalf("peek error must carry errEventQueueLoadFailed; got %v", err)
	}
	if err := q.enqueue("x"); !errors.Is(err, errEventQueueLoadFailed) {
		t.Fatalf("enqueue error must carry errEventQueueLoadFailed; got %v", err)
	}
	if _, err := q.advance(cursor); !errors.Is(err, errEventQueueLoadFailed) {
		t.Fatalf("advance error must carry errEventQueueLoadFailed; got %v", err)
	}
}

// TestEventQueue_InitialLoadFailureCountsAgainstRetryThrottle pins the retry
// throttle on the constructor path (#3242 review round 2): the failed load
// newEventQueue itself performs is a real disk attempt and must arm the
// throttle, so the run loop's immediate pendingCount answers from the recorded
// error instead of issuing a second consecutive blocking disk attempt — on a
// hung network mount that doubles every watcher's startup stall. Observable
// consequence pinned here: even if storage heals right after construction,
// recovery waits out eventQueueLoadRetryInterval rather than landing on the
// very next call.
func TestEventQueue_InitialLoadFailureCountsAgainstRetryThrottle(t *testing.T) {
	dir := t.TempDir()
	// A huge interval makes the assertion airtight: any disk retry inside this
	// test means the constructor's attempt never armed the throttle at all.
	oldInterval := eventQueueLoadRetryInterval
	eventQueueLoadRetryInterval = time.Hour
	t.Cleanup(func() { eventQueueLoadRetryInterval = oldInterval })

	seed := newEventQueue(dir, "ab324206")
	if err := seed.enqueue("only"); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}

	denyAccess(t, seed.path, seed.path, 0o644)
	q := newEventQueue(dir, "ab324206") // this failed attempt must arm the throttle

	// Storage heals immediately — microseconds after the constructor's attempt.
	if err := os.Chmod(seed.path, 0o644); err != nil {
		t.Fatalf("restore queue file mode: %v", err)
	}

	if got := q.pendingCount(); got != 0 || !q.loadFailed() {
		t.Fatalf("pendingCount=%d loadFailed=%v right after a failed constructor load; the initial attempt must count against the retry throttle", got, q.loadFailed())
	}
}

// TestDeliveryAlarms_UnreadableQueueRaisesAlarmWithoutDeliveries pins the last
// leg of the #3242 operator projection (review round 2): a daemon that starts
// while a POPULATED queue file is unreadable, with a healthy target and a
// script that emits nothing new, must still raise the delivery alarm — a
// parked replay IS a delivery outage. deliverFailSince historically moved only
// on delivery attempts, and this scenario performs none, so the
// pending-unknown banner built for exactly this state was unreachable in it.
func TestDeliveryAlarms_UnreadableQueueRaisesAlarmWithoutDeliveries(t *testing.T) {
	dir := t.TempDir()
	oldThreshold := watcherDeliveryAlarmThreshold
	watcherDeliveryAlarmThreshold = 100 * time.Millisecond
	t.Cleanup(func() { watcherDeliveryAlarmThreshold = oldThreshold })

	// First lifetime: three events queue behind failing deliveries, then stop.
	s1, _ := newTestSupervisor(t, staticTasks(watchTask("ab324207", `echo e1; echo e2; echo e3; sleep 60`, dir)))
	fd1 := &flakyDeliver{}
	s1.deliver = fd1.deliver
	queueDir, _ := s1.queueDir()
	if err := s1.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "backlog to persist", func() bool {
		return newEventQueue(queueDir, "ab324207").pendingCount() == 3
	})
	s1.Stop()

	// Second lifetime starts with the queue file unreadable and a script that
	// emits nothing; the sentinel proves the watcher's run loop is up.
	queuePath := filepath.Join(queueDir, "ab324207.jsonl")
	denyAccess(t, queuePath, queuePath, 0o644)
	s2, _ := newTestSupervisor(t, staticTasks(watchTask("ab324207", `touch "`+dir+`/started"; sleep 60`, dir)))
	fd2 := &flakyDeliver{}
	fd2.healed.Store(true)
	s2.deliver = fd2.deliver
	s2.queueDir = func() (string, error) { return queueDir, nil }
	if err := s2.Reload(); err != nil {
		t.Fatalf("Reload second lifetime: %v", err)
	}
	waitUntil(t, 10*time.Second, "watch script to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	// No delivery will ever be attempted, so only the load failure itself can
	// open the alarm run. It must, once past the (shrunk) threshold — and it
	// must say the count is unknown rather than fabricate a zero.
	waitUntil(t, 10*time.Second, "delivery alarm for the unreadable queue", func() bool {
		for _, a := range s2.deliveryAlarms("", time.Now()) {
			if a.TaskID == "ab324207" && a.PendingUnknown {
				return true
			}
		}
		return false
	})
	if got := fd2.delivered(); len(got) != 0 {
		t.Fatalf("no delivery should have been attempted; delivered %v", got)
	}
}

// TestEventQueue_LoadFailureAdvanceRefuses pins the consume-side guard: a
// cursor minted before the state went unknown must not consume anything from a
// queue whose load failed. Refusing with an error parks the drainer; silently
// answering "re-peek" (false, nil) lets the caller carry on against a state
// nobody could enumerate.
func TestEventQueue_LoadFailureAdvanceRefuses(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "ab324204")
	for _, line := range []string{"old-1", "old-2"} {
		if err := seed.enqueue(line); err != nil {
			t.Fatalf("seed enqueue %q: %v", line, err)
		}
	}
	_, cursor, ok, err := seed.peek()
	if err != nil || !ok {
		t.Fatalf("seed peek: ok=%v err=%v", ok, err)
	}

	denyAccess(t, seed.path, seed.path, 0o644)
	q := newEventQueue(dir, "ab324204")

	if advanced, err := q.advance(cursor); err == nil {
		t.Fatalf("advance(advanced=%v) accepted a cursor against an unknown queue state; it must refuse with an error", advanced)
	}
}
