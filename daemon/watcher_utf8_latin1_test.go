package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestConsumeLinesNormalArmLatin1DurableCorruption is the end-to-end regression
// test for the durable-queue UTF-8 corruption in the normal (newline-terminated)
// arm of consumeLines. A watch script emitting a short latin-1 line ("caf\xe9")
// that then fails delivery routes through handleEvent -> enqueueEvent ->
// enqueueWithParkedStatus -> json.Marshal(queuedEvent{Line: line}). Before the
// fix, the normal arm passed the raw chunk to handleEvent without sanitizeUTF8,
// so encoding/json rewrote the invalid \xe9 as U+FFFD in the persisted .jsonl
// record — violating sanitizeUTF8's documented "no U+FFFD reaches the durable
// record" invariant (the sibling ErrBufferFull arm was hardened by b981be7f
// (#4664/#4655), but the common arm was not). After the fix, the invalid byte is
// dropped before marshal, so the persisted record contains "caf" (lossy but
// without U+FFFD). Mirrors TestWatcherTruncatesLongUTF8LineDurableCorruption,
// which pins the same invariant for the ErrBufferFull arm.
func TestConsumeLinesNormalArmLatin1DurableCorruption(t *testing.T) {
	// "caf\xe9\n" is a 4-byte latin-1 line well under the 64KB cap, so it
	// takes consumeLines' normal (err == nil) arm — the one the bug report
	// identifies as unsanitized. The trailing "next\n" is a second ASCII
	// event kept to also exercise the backlog-pending enqueue of a later
	// line: the first line's failed delivery leaves pendingCountFresh() > 0,
	// so "next" routes straight to enqueueEvent without a delivery attempt.
	payload := []byte("caf\xe9\nnext\n")
	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, payload, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "latin1norm01"
	script := "cat " + payloadFile + "; exit 0"
	s, rec := newTestSupervisor(t, staticTasks(watchTask(taskID, script, dir)))
	// Force every delivery to defer (errTargetBusy) so handleEvent takes the
	// delivery-failure arm and calls enqueueEvent -> json.Marshal, the
	// durable-queue path whose corruption is under test. The direct-delivery
	// success arm does not marshal and is covered by a separate test below.
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetBusy })

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	queueDir, _ := s.queueDir()
	queuePath := filepath.Join(queueDir, taskID+".jsonl")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file %s: %v", queuePath, err)
	}

	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(recordLines) < 1 {
		t.Fatalf("expected at least 1 queued record in %s, got %d", queuePath, len(recordLines))
	}

	// Regression assertion: every persisted record must round-trip to valid
	// UTF-8 with no U+FFFD. Before the fix, the first record's Line was
	// marshaled as {"line":"caf\ufffd"}.
	for _, rl := range recordLines {
		if rl == "" {
			continue
		}
		var ev queuedEvent
		if err := json.Unmarshal([]byte(rl), &ev); err != nil {
			t.Fatalf("unmarshal queue record: %v", err)
		}
		if strings.ContainsRune(ev.Line, '\ufffd') {
			t.Fatalf("persisted queue Line contains U+FFFD: %q (on-disk: %s) — normal arm of consumeLines lacks sanitizeUTF8, json.Marshal rewrote the latin-1 \\xe9", ev.Line, rl)
		}
		if !utf8.ValidString(ev.Line) {
			t.Fatalf("persisted queue Line is not valid UTF-8: %q (on-disk: %s)", ev.Line, rl)
		}
	}

	// The latin-1 \xe9 must be dropped (not replaced), matching the
	// ErrBufferFull arm's lossy-but-without-U+FFFD contract: the first
	// record's Line is "caf". This is the exact expected-vs-actual from the
	// bug report ("caf\uFFFD" before, "caf" after).
	var first queuedEvent
	if err := json.Unmarshal([]byte(recordLines[0]), &first); err != nil {
		t.Fatalf("unmarshal first queue record: %v", err)
	}
	if first.Line != "caf" {
		t.Fatalf("first persisted Line = %q, want %q (latin-1 \\xe9 dropped, not U+FFFD-substituted)", first.Line, "caf")
	}
}

// TestConsumeLinesNormalArmLatin1BacklogPendingCorruption is the end-to-end
// regression test for the backlog-pending steady-state shape: once one event is
// queued, every subsequent line routes directly to enqueueEvent -> json.Marshal
// without a delivery attempt (handleEvent's backlog-pending arm). A later latin-1
// line on this path was persisted with U+FFFD before the fix (the bug report's
// seq:2 record); it must now be sanitized at the normal arm before it reaches
// handleEvent.
func TestConsumeLinesNormalArmLatin1BacklogPendingCorruption(t *testing.T) {
	// The first valid-ASCII line fails delivery and establishes a backlog
	// (pendingCountFresh() > 0); the second latin-1 line then routes via the
	// backlog-pending arm straight to json.Marshal with no delivery attempt.
	payload := []byte("valid-ascii-line\ncaf\xe9\n")
	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, payload, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "latin1back02"
	script := "cat " + payloadFile + "; exit 0"
	s, rec := newTestSupervisor(t, staticTasks(watchTask(taskID, script, dir)))
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetBusy })

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	queueDir, _ := s.queueDir()
	queuePath := filepath.Join(queueDir, taskID+".jsonl")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file %s: %v", queuePath, err)
	}

	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(recordLines) < 2 {
		t.Fatalf("expected at least 2 queued records in %s, got %d", queuePath, len(recordLines))
	}

	for _, rl := range recordLines {
		if rl == "" {
			continue
		}
		var ev queuedEvent
		if err := json.Unmarshal([]byte(rl), &ev); err != nil {
			t.Fatalf("unmarshal queue record: %v", err)
		}
		if strings.ContainsRune(ev.Line, '\ufffd') {
			t.Fatalf("persisted queue Line contains U+FFFD: %q (on-disk: %s) — backlog-pending arm enqueued a normal-arm latin-1 line without sanitizeUTF8", ev.Line, rl)
		}
		if !utf8.ValidString(ev.Line) {
			t.Fatalf("persisted queue Line is not valid UTF-8: %q (on-disk: %s)", ev.Line, rl)
		}
	}

	// The second record is the latin-1 line routed via the backlog-pending
	// arm (no delivery attempted); sanitizeUTF8 drops the \xe9 so its Line is
	// "caf", and the first record is the verbatim ASCII line.
	var first, second queuedEvent
	if err := json.Unmarshal([]byte(recordLines[0]), &first); err != nil {
		t.Fatalf("unmarshal first queue record: %v", err)
	}
	if err := json.Unmarshal([]byte(recordLines[1]), &second); err != nil {
		t.Fatalf("unmarshal second queue record: %v", err)
	}
	if first.Line != "valid-ascii-line" {
		t.Fatalf("first persisted Line = %q, want %q", first.Line, "valid-ascii-line")
	}
	if second.Line != "caf" {
		t.Fatalf("backlog-pending persisted Line = %q, want %q (latin-1 \\xe9 dropped, not U+FFFD-substituted)", second.Line, "caf")
	}
}

// TestConsumeLinesNormalArmValidUTF8DurableUnchanged is the non-regression guard
// for the durable path: a normal-arm line of well-formed multi-byte UTF-8 that
// routes to the durable queue must pass through sanitizeUTF8 unchanged (it is a
// no-op on valid UTF-8), so no valid bytes are lost and no U+FFFD is introduced.
// This pins that the fix does not over-sanitize well-formed non-ASCII content.
func TestConsumeLinesNormalArmValidUTF8DurableUnchanged(t *testing.T) {
	payload := []byte("café\n世界\n")
	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, payload, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "validutf801"
	script := "cat " + payloadFile + "; exit 0"
	s, rec := newTestSupervisor(t, staticTasks(watchTask(taskID, script, dir)))
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetBusy })

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	queueDir, _ := s.queueDir()
	queuePath := filepath.Join(queueDir, taskID+".jsonl")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}

	want := []string{"café", "世界"}
	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(recordLines) < len(want) {
		t.Fatalf("expected at least %d queued records, got %d", len(want), len(recordLines))
	}
	for i, rl := range recordLines[:len(want)] {
		var ev queuedEvent
		if err := json.Unmarshal([]byte(rl), &ev); err != nil {
			t.Fatalf("unmarshal queue record %d: %v", i, err)
		}
		if ev.Line != want[i] {
			t.Fatalf("record %d persisted Line = %q, want %q (valid UTF-8 must pass through unchanged)", i, ev.Line, want[i])
		}
		if strings.ContainsRune(ev.Line, '\ufffd') {
			t.Fatalf("record %d persisted Line contains U+FFFD: %q", i, ev.Line)
		}
		if !utf8.ValidString(ev.Line) {
			t.Fatalf("record %d persisted Line is not valid UTF-8: %q", i, ev.Line)
		}
	}
}

// TestConsumeLinesNormalArmLatin1EmptyLineDiscarded is the regression test for
// the second Codex inline concern on the sanitize call at daemon/watcher.go:642:
// a normal-arm line of only invalid bytes (for example "\xff\n") collapses to ""
// under sanitizeUTF8. Without an explicit discard, handleEvent would still
// invoke deliverWatchEventWithOptions on the empty line — the rendered prompt
// collapses to "" too, delivery fails as an empty prompt before any send, and
// handleEvent enqueues the empty line through enqueueEvent. The drainer then
// re-renders the same empty head forever, never dequeuing it, and every later
// valid event stays blocked behind it until retention or overflow removes the
// head. The durable-queue boundary in enqueueEvent now discards the empty
// result rather than enrolling a permanently undeliverable event, so a
// subsequent valid line lands at the queue head unblocked.
func TestConsumeLinesNormalArmLatin1EmptyLineDiscarded(t *testing.T) {
	// "\xff\n" sanitizes to "" (the only byte is invalid UTF-8) under the
	// normal (err == nil) arm of consumeLines; "valid-ascii-line\n" takes the
	// same arm and must remain durable-enqueueable behind the discarded empty
	// result. Before the boundary discard, the queue would persist a
	// `{"line":""}` head record plus a `valid-ascii-line` record parked behind
	// it. After the discard, only the valid record remains.
	payload := []byte("\xff\nvalid-ascii-line\n")
	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, payload, 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "latin1empty01"
	script := "cat " + payloadFile + "; exit 0"
	s, rec := newTestSupervisor(t, staticTasks(watchTask(taskID, script, dir)))
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetBusy })

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	queueDir, _ := s.queueDir()
	queuePath := filepath.Join(queueDir, taskID+".jsonl")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file %s: %v", queuePath, err)
	}

	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(recordLines) != 1 || recordLines[0] == "" {
		t.Fatalf("expected exactly 1 non-blank queued record in %s (the all-invalid \\xff line is discarded at the durable-queue boundary), got %d: %q", queuePath, len(recordLines), recordLines)
	}
	var ev queuedEvent
	if err := json.Unmarshal([]byte(recordLines[0]), &ev); err != nil {
		t.Fatalf("unmarshal queue record: %v", err)
	}
	if ev.Line != "valid-ascii-line" {
		t.Fatalf("persisted Line = %q, want %q (the all-invalid \\xff line was sanitized to empty and discarded; this valid record must be the queue head, not blocked behind a permanently undeliverable empty record)", ev.Line, "valid-ascii-line")
	}
	if strings.ContainsRune(ev.Line, '\ufffd') {
		t.Fatalf("persisted queue Line contains U+FFFD: %q", ev.Line)
	}
	if !utf8.ValidString(ev.Line) {
		t.Fatalf("persisted queue Line is not valid UTF-8: %q", ev.Line)
	}
}
