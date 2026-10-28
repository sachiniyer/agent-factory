package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestSanitizeUTF8 pins sanitizeUTF8: it drops invalid UTF-8 byte sequences —
// including a trailing partial rune, and a lone invalid/continuation byte
// anywhere in the string — so the result is always valid UTF-8 (no U+FFFD),
// leaves already-valid input (including all-ASCII at exactly the byte cap)
// untouched, and never drops a valid trailing character: a chunk whose tail is
// already a whole rune keeps its full length. This is the unit guard for the
// fix; the end-to-end tests below prove the sanitized line reaches the
// persisted .jsonl record without U+FFFD.
func TestSanitizeUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii short", "ascii ok", "ascii ok"},
		{"ascii at full cap unchanged", strings.Repeat("x", maxWatchLineBytes), strings.Repeat("x", maxWatchLineBytes)},
		{"complete runes unchanged", "世界🚀", "世界🚀"},

		// 2-byte rune (\xc3\xa9 = é) split at the lead byte.
		{"2-byte split at lead", "x\xc3", "x"},
		// 3-byte rune (\xe4\xb8\xad = 中) split at the lead byte, and after the
		// lead + 1 continuation byte.
		{"3-byte split at lead", "x\xe4", "x"},
		{"3-byte split after 1 continuation", "x\xe4\xb8", "x"},
		// 4-byte rune (\xf0\x9f\x9a\x80 = 🚀) split at every internal position;
		// the after-2-continuations case is three invalid bytes, all dropped.
		{"4-byte split at lead", "x\xf0", "x"},
		{"4-byte split after 1 continuation", "x\xf0\x9f", "x"},
		{"4-byte split after 2 continuations", "x\xf0\x9f\x9a", "x"},

		// A full rune immediately before the partial rune is retained; only the
		// trailing partial rune is dropped.
		{"full rune then partial", "世\xe4", "世"},
		{"whole runes then 4-byte partial", "世🚀\xf0\x9f", "世🚀"},

		// A boundary that lands exactly after a complete rune leaves the input
		// unchanged (valid UTF-8, nothing to trim).
		{"boundary after complete rune", strings.Repeat("世", maxWatchLineBytes/3), strings.Repeat("世", maxWatchLineBytes/3)},

		// Invalid bytes elsewhere in the chunk — not just a trailing partial
		// rune — are stripped, and a tail that is already a whole rune is NOT
		// shortened by one character. These are the cases the prior tail-only
		// walk-back mishandled: it left the mid-string byte invalid and dropped
		// a trailing valid char for nothing. Each result here is valid UTF-8.
		{"lone invalid byte mid-string", "a\xffb", "ab"},
		{"invalid mid-string keeps trailing chars", "a\xffhello", "ahello"},
		{"lone continuation byte at start", "\x80abc", "abc"},
		{"invalid bytes then partial rune", "a\xffb\xe2\x82", "ab"},
		{"all invalid bytes", "\x80\x80\x80", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeUTF8(tc.in)
			if got != tc.want {
				t.Fatalf("sanitize(%q) = %q (% x), want %q (% x)", tc.in, got, []byte(got), tc.want, []byte(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result not valid UTF-8: % x", []byte(got))
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("result contains U+FFFD: %q", got)
			}
		})
	}

	// At the real byte cap: an ASCII line of exactly maxWatchLineBytes is the
	// non-regression case TestWatcherTruncatesLongLines pins — it must come
	// back unchanged so the full 64KB cap is preserved for ASCII.
	ascii := strings.Repeat("a", maxWatchLineBytes)
	if got := sanitizeUTF8(ascii); len(got) != maxWatchLineBytes || got != ascii {
		t.Fatalf("ASCII line at cap mutated: len=%d (want %d)", len(got), maxWatchLineBytes)
	}

	// A CJK line truncated at the cap such that the boundary is the lead byte of
	// a 3-byte rune: that lone lead byte is invalid and is dropped, so the
	// result is exactly one byte shorter than the cap and remains valid UTF-8.
	cjk := strings.Repeat("x", maxWatchLineBytes-1) + "\xe4"
	got := sanitizeUTF8(cjk)
	if len(got) != maxWatchLineBytes-1 {
		t.Fatalf("CJK lead-byte split: len=%d, want %d", len(got), maxWatchLineBytes-1)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("CJK lead-byte split: result not valid UTF-8: % x", []byte(got[len(got)-4:]))
	}

	// A 4-byte emoji split after two continuation bytes at the cap is three
	// invalid bytes, all dropped, so the result is three bytes shorter than the
	// cap and remains valid UTF-8.
	emoji := strings.Repeat("x", maxWatchLineBytes-3) + "\xf0\x9f\x9a"
	got = sanitizeUTF8(emoji)
	if len(got) != maxWatchLineBytes-3 {
		t.Fatalf("4-byte emoji 2-continuation split: len=%d, want %d", len(got), maxWatchLineBytes-3)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("4-byte emoji split: result not valid UTF-8: % x", []byte(got[len(got)-4:]))
	}
}

// TestWatcherTruncatesLongUTF8LineDurableCorruption is the end-to-end
// regression test for the durable-queue UTF-8 truncation bug in consumeLines.
// A delivery that returns errTargetBusy makes handleEvent route the truncated
// line to enqueueEvent -> enqueueWithParkedStatus -> json.Marshal(queuedEvent
// {Line: line}). Before the fix, the 64KB chunk ended mid-rune, so encoding/json
// rewrote the half-rune as U+FFFD in the persisted .jsonl file. After the fix,
// the partial trailing rune is trimmed BEFORE marshal, so the persisted record
// must NOT contain U+FFFD.
func TestWatcherTruncatesLongUTF8LineDurableCorruption(t *testing.T) {
	// 65535 'x' bytes then 中 (\xe4\xb8\xad): the byte cap lands on the lead byte
	// \xe4 of the 3-byte rune, splitting it. The durable path must trim that
	// lone lead byte before marshal.
	const prefixLen = maxWatchLineBytes - 1
	var content bytes.Buffer
	content.Grow(prefixLen + 3 + len("\nnext\n"))
	for i := 0; i < prefixLen; i++ {
		content.WriteByte('x')
	}
	content.WriteString("中")
	content.WriteString("\nnext\n")

	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, content.Bytes(), 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "utf80d01"
	script := "cat " + payloadFile + "; exit 0"
	s, rec := newTestSupervisor(t, staticTasks(watchTask(taskID, script, dir)))
	// Force every delivery to defer (errTargetBusy) so handleEvent takes the
	// error arm (daemon/watcher.go) and calls enqueueEvent -> json.Marshal, the
	// durable-queue path whose corruption is under test. The direct-delivery
	// success arm does NOT marshal and is not a corruption vector.
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetBusy })

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	// Inspect the persisted .jsonl queue file — the actual artefact.
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

	var ev queuedEvent
	if err := json.Unmarshal([]byte(recordLines[0]), &ev); err != nil {
		t.Fatalf("unmarshal first queue record: %v", err)
	}

	// Regression assertion: the persisted Line must NOT contain U+FFFD. Before
	// the fix, json.Marshal rewrote the partial lead byte as \ufffd.
	if strings.Contains(ev.Line, "\uFFFD") {
		t.Fatalf("persisted queue Line contains U+FFFD at index %d: json.Marshal rewrote the partial rune split by the byte-cap. last bytes of ev.Line: % x",
			strings.Index(ev.Line, "\uFFFD"), []byte(ev.Line[len(ev.Line)-20:]))
	}
	// The lead byte of the split rune is dropped, so the retained line is exactly
	// one byte shorter than the cap and is whole-rune ASCII. This documents the
	// new contract: non-ASCII truncation may trim up to 3 bytes (one partial
	// rune) off the cap.
	if len(ev.Line) != maxWatchLineBytes-1 {
		t.Fatalf("trimmed durable Line length = %d, want %d (cap minus the partial lead byte)", len(ev.Line), maxWatchLineBytes-1)
	}
	if !utf8.ValidString(ev.Line) {
		t.Fatalf("persisted durable Line is not valid UTF-8: last bytes % x", []byte(ev.Line[len(ev.Line)-20:]))
	}
}

// TestWatcherTruncatesLongASCIILineDurableQueueNoTrim is the ASCII
// non-regression guard on the durable-queue path: an overlong ASCII line that
// routes to the durable queue must keep exactly maxWatchLineBytes bytes (the
// trim helper is a no-op for valid UTF-8), preserving the contract
// TestWatcherTruncatesLongLines pins for the success path.
func TestWatcherTruncatesLongASCIILineDurableQueueNoTrim(t *testing.T) {
	var content bytes.Buffer
	for i := 0; i < maxWatchLineBytes+1; i++ {
		content.WriteByte('x')
	}
	content.WriteString("\nnext\n")

	payloadFile := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadFile, content.Bytes(), 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	dir := t.TempDir()
	const taskID = "utf80d03"
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
	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var ev queuedEvent
	if err := json.Unmarshal([]byte(recordLines[0]), &ev); err != nil {
		t.Fatalf("unmarshal first queue record: %v", err)
	}
	if strings.Contains(ev.Line, "\uFFFD") {
		t.Fatalf("ASCII durable Line contains U+FFFD: %q", ev.Line)
	}
	if len(ev.Line) != maxWatchLineBytes {
		t.Fatalf("ASCII durable Line length = %d, want %d (full cap preserved, no trim)", len(ev.Line), maxWatchLineBytes)
	}
	if strings.Trim(ev.Line, "x") != "" {
		t.Fatalf("ASCII durable Line corrupted: %q", ev.Line[:40])
	}
}

// TestPersistRemainingLimitEventsUTF8TruncCorruption is the regression test for
// the second occurrence: persistRemainingLimitEvents's ErrBufferFull arm
// (daemon/watcher_limit_park.go) takes string(chunk) raw and calls emit ->
// enqueueEvent -> json.Marshal, the same defect as consumeLines. The durable
// record must not contain U+FFFD after the trim.
func TestPersistRemainingLimitEventsUTF8TruncCorruption(t *testing.T) {
	queueDir := t.TempDir()
	const taskID = "a4223103"
	queue := newEventQueue(queueDir, taskID)
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: taskID, sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh}

	// 65535 'x' bytes then 中 (\xe4\xb8\xad): the cap lands on the lead byte.
	const prefixLen = maxWatchLineBytes - 1
	var content bytes.Buffer
	for i := 0; i < prefixLen; i++ {
		content.WriteByte('x')
	}
	content.WriteString("中")
	content.WriteString("\nnext\n")

	br := bufio.NewReaderSize(strings.NewReader(content.String()), maxWatchLineBytes)
	w.persistRemainingLimitEvents(br, &tailBuffer{})

	queuePath := filepath.Join(queueDir, taskID+".jsonl")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	recordLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for _, rl := range recordLines {
		var ev queuedEvent
		if err := json.Unmarshal([]byte(rl), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if strings.HasPrefix(ev.Line, "x") && len(ev.Line) > 100 {
			if strings.Contains(ev.Line, "\uFFFD") {
				t.Fatalf("persistRemainingLimitEvents: persisted long Line contains U+FFFD at index %d: same defect as consumeLines. last bytes=% x",
					strings.Index(ev.Line, "\uFFFD"), []byte(ev.Line[len(ev.Line)-20:]))
			}
			if len(ev.Line) != maxWatchLineBytes-1 {
				t.Fatalf("persistRemainingLimitEvents: trimmed Line length = %d, want %d", len(ev.Line), maxWatchLineBytes-1)
			}
			if !utf8.ValidString(ev.Line) {
				t.Fatalf("persistRemainingLimitEvents: Line not valid UTF-8: % x", []byte(ev.Line[len(ev.Line)-20:]))
			}
			return // found the long record, verified — pass
		}
	}
	t.Fatalf("persistRemainingLimitEvents: long record not found in queue file")
}
