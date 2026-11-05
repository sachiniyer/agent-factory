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
