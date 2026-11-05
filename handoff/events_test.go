package handoff

// events_test.go pins the shadow log's file discipline and the edge
// tracker's dedup rules: day-rotated JSONL under the ledger's lock shape,
// seven-day pruning, control-stripped titles, and emit-on-change /
// re-announce-on-heartbeat semantics.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventLogAppendWritesDayFileAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }

	rows := []EventRow{
		{TS: now, Kind: EventWall, SessionKey: "sess-1", Agent: "codex",
			Title: "line one\nline two\x07with-bell", From: "work",
			ReasonCode: "usage-reading", Params: map[string]string{"confirmed": "true"}, Mode: "shadow"},
		{TS: now.Add(time.Second), Kind: EventNoAccount, SessionKey: "sess-1",
			ReasonCode: "no-eligible-candidate", Mode: "shadow"},
	}
	if err := log.Append(rows); err != nil {
		t.Fatalf("Append: %v", err)
	}
	file := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), raw)
	}
	var first EventRow
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not JSON: %v", err)
	}
	if first.Kind != EventWall || first.ReasonCode != "usage-reading" || first.From != "work" {
		t.Fatalf("line 0 = %+v", first)
	}
	if strings.ContainsAny(first.Title, "\n\x07") {
		t.Fatalf("title carried control characters: %q", first.Title)
	}
	if first.Mode != "shadow" {
		t.Fatalf("mode = %q", first.Mode)
	}
}

func TestEventLogAppendGroupsByRowDay(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	before := time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second) // lands 2026-11-04
	log.now = func() time.Time { return after }
	if err := log.Append([]EventRow{
		{TS: before, Kind: EventWall, SessionKey: "s", ReasonCode: "wall"},
		{TS: after, Kind: EventWall, SessionKey: "s", ReasonCode: "wall"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, want := range []string{"auto-handoff-events-20261103.jsonl", "auto-handoff-events-20261104.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Fatalf("expected %s: %v", want, err)
		}
	}
}

func TestEventLogPrunesPastRetention(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	old := filepath.Join(dir, "auto-handoff-events-20261101.jsonl") // 9 days back
	recent := filepath.Join(dir, "auto-handoff-events-20261105.jsonl")
	other := filepath.Join(dir, "tasks.json")
	for _, f := range []string{old, recent, other} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old event file survived pruning")
	}
	for _, keep := range []string{recent, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("non-expired file %s was pruned: %v", keep, err)
		}
	}
}

// #6817 review: a symlinked day file is never written through — the log
// follows the AF-home rule, not whatever the link points at.
func TestEventLogAppendRefusesSymlinkedDayFile(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dayFile := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	if err := os.Symlink(target, dayFile); err != nil {
		t.Fatal(err)
	}
	err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}})
	if err == nil {
		t.Fatal("Append wrote through a symlinked day file")
	}
	raw, _ := os.ReadFile(target)
	if len(raw) != len("{}\n") {
		t.Fatalf("append reached the symlink target: %q", raw)
	}
}

// #6817 review: the sibling lock file prunes on the same stamp as the data
// file — otherwise stale locks accumulate past retention.
func TestEventLogPrunesStaleLockFiles(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	staleLock := filepath.Join(dir, "auto-handoff-events-20261101.jsonl.lock")
	if err := os.WriteFile(staleLock, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("stale lock file survived pruning")
	}
}

// #6817 review: a day file past the byte cap takes no more rows — bounded
// growth over a silently unbounded log.
func TestEventLogDayCapHoldsRows(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	if err := os.WriteFile(dayFile, make([]byte, eventDayMaxBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	// Past the day cap, Append ERRORS rather than silently dropping — the
	// caller's edge stays uncommitted so the row retries next tick (#6817
	// round-2 review).
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err == nil {
		t.Fatalf("Append past the day cap returned nil error")
	}
	if info, err := os.Stat(dayFile); err != nil || info.Size() != eventDayMaxBytes {
		t.Fatalf("capped day file changed: size=%d err=%v", info.Size(), err)
	}
}

func TestEdgeTrackerEmitsOnChangeThenSuppresses(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "usage-reading", From: "work"}}
	move := []Event{{Kind: EventMove, Reason: "wall", From: "work", To: "personal"}}

	if got := tr.Filter("s", wall, now, ReannounceTest); len(got) != 1 {
		t.Fatalf("first wall verdict suppressed")
	}
	if got := tr.Filter("s", wall, now.Add(time.Minute), ReannounceTest); got != nil {
		t.Fatalf("unchanged verdict re-emitted inside the heartbeat")
	}
	if got := tr.Filter("s", move, now.Add(2*time.Minute), ReannounceTest); len(got) != 1 {
		t.Fatalf("changed verdict did not emit an edge")
	}
	// Same signature, past the heartbeat → the standing verdict re-announces.
	if got := tr.Filter("s", move, now.Add(2*time.Minute).Add(ReannounceTest+time.Second), ReannounceTest); len(got) != 1 {
		t.Fatalf("unchanged verdict did not re-announce after the heartbeat")
	}
}

func TestEdgeTrackerSilenceIsAnEdgeNotAnEvent(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "wall"}}

	tr.Filter("s", wall, now, time.Hour)
	// noop (no events) is a signature change → recorded, emits nothing.
	if got := tr.Filter("s", nil, now.Add(time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("silence emitted rows")
	}
	// The NEXT wall is a fresh edge even inside the heartbeat window.
	if got := tr.Filter("s", wall, now.Add(2*time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("wall after a noop transition did not re-emit")
	}
}

func TestEdgeTrackerPruneDropsGoneSessions(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "wall"}}
	tr.Filter("gone", wall, now, time.Hour)
	tr.Prune(map[string]bool{"live": true})
	if got := tr.Filter("gone", wall, now.Add(time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("a session that left and returned did not re-announce")
	}
}

// ReannounceTest mirrors the wiring's heartbeat constant for this leaf's
// internal tests (the exported one lives in the engine package).
const ReannounceTest = 30 * time.Minute

// #6817 review: a crash-torn tail must not corrupt the next row — an
// unterminated final line is dropped before the append, never concatenated.
func TestEventLogAppendRepairsUnterminatedTail(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261112.jsonl")
	good := `{"ts":"2026-11-12T07:59:00Z","kind":"wall","reason":"hit"}`
	if err := os.WriteFile(dayFile, []byte(good+"\n"+`{"ts":"2026-11-12T07:59`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall", Title: "next"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, err := os.ReadFile(dayFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("day file has %d lines, want 2: %q", len(lines), raw)
	}
	if lines[0] != good {
		t.Fatalf("good row mutated: %q", lines[0])
	}
	var decoded EventRow
	if err := json.Unmarshal([]byte(lines[1]), &decoded); err != nil {
		t.Fatalf("appended row is not valid JSON (tail corrupted it): %v", err)
	}
	if decoded.Title != "next" {
		t.Fatalf("appended row lost its fields: %+v", decoded)
	}
}

// A file that is nothing but a partial line truncates to empty rather than
// refusing every subsequent append.
func TestEventLogAppendTruncatesWholePartialFile(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261112.jsonl")
	if err := os.WriteFile(dayFile, []byte(`{"ts":"partial, no newline`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, _ := os.ReadFile(dayFile)
	if !strings.HasPrefix(string(raw), "{") || !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("file does not hold exactly one clean row: %q", raw)
	}
}
