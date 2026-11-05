package handoff

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadMissingFileIsEmptyState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if state.SchemaVersion != stateSchemaVersion || len(state.CarriedPrefixes) != 0 || state.Halted != nil {
		t.Fatalf("empty state = %+v", state)
	}
}

func TestRecordCarriedPrefixRoundTrip(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	if err := store.RecordCarriedPrefix("/home/fake/.codex/sessions/2026/10/31/rollout-a-00000000-0000-4000-8000-000000000001.jsonl", 4096); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	prefixes, err := store.CarriedPrefixes()
	if err != nil {
		t.Fatalf("CarriedPrefixes: %v", err)
	}
	if got := prefixes["/home/fake/.codex/sessions/2026/10/31/rollout-a-00000000-0000-4000-8000-000000000001.jsonl"]; got != 4096 {
		t.Fatalf("prefix = %d, want 4096", got)
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if _, err := store.Load(); err == nil {
		t.Fatal("a corrupt state file loaded cleanly — rails state must fail closed, never rebuild silently")
	}
	// A mutation over a corrupt file must also refuse rather than rebuild on
	// top of it.
	if err := store.RecordCarriedPrefix("/x", 1); err == nil {
		t.Fatal("RecordCarriedPrefix rebuilt over a corrupt file")
	}
}

func TestWrongSchemaVersionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version": 999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("a state file with a future schema_version loaded cleanly")
	}
}

func TestRecordCarriedPrefixValidates(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	if err := store.RecordCarriedPrefix("", 10); err == nil {
		t.Fatal("empty path recorded")
	}
	if err := store.RecordCarriedPrefix("/x", -1); err == nil {
		t.Fatal("negative length recorded")
	}
}

func TestConcurrentMutationsBothSurvive(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join("/home/fake/.codex/sessions/2026/10/31/", string(rune('a'+i))+".jsonl")
			if err := store.RecordCarriedPrefix(path, int64(i+1)); err != nil {
				t.Errorf("RecordCarriedPrefix %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	prefixes, err := store.CarriedPrefixes()
	if err != nil {
		t.Fatalf("CarriedPrefixes: %v", err)
	}
	if len(prefixes) != 8 {
		t.Fatalf("prefixes = %v, want 8 entries — a racing mutator was clobbered", prefixes)
	}
}

func TestHaltedAtReadsTheRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	haltedAt := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	raw := `{"schema_version": 1, "halted": {"at": "2026-10-31T04:00:00Z", "reason": "move-failed"}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	halt, err := NewStore(path).HaltedAt()
	if err != nil {
		t.Fatalf("HaltedAt: %v", err)
	}
	if halt == nil || !halt.At.Equal(haltedAt) || halt.Reason != "move-failed" {
		t.Fatalf("halt = %+v", halt)
	}
}

// A watermark only ever grows: a re-carry that verifies a shorter covered
// prefix must not shrink the quarantine onto bytes an earlier, longer copy
// put there — records in that span are still the source account's (#6802
// review).
func TestRecordCarriedPrefixNeverShrinks(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	path := "/home/fake/.codex/sessions/2026/11/10/rollout-x.jsonl"
	if err := store.RecordCarriedPrefix(path, 100); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	if err := store.RecordCarriedPrefix(path, 40); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	prefixes, err := store.CarriedPrefixes()
	if err != nil {
		t.Fatalf("CarriedPrefixes: %v", err)
	}
	if prefixes[path] != 100 {
		t.Fatalf("prefix = %d, want 100 — a smaller re-carry must not shrink the quarantine", prefixes[path])
	}
	if err := store.RecordCarriedPrefix(path, 140); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	prefixes, err = store.CarriedPrefixes()
	if err != nil {
		t.Fatalf("CarriedPrefixes: %v", err)
	}
	if prefixes[path] != 140 {
		t.Fatalf("prefix = %d, want 140 — a larger copy always extends the mark", prefixes[path])
	}
}

// #6818 final review: a held report backlog is recorded durably so af
// quota's mover line can say it — first-writer-wins, cleared on drain.
func TestNoteAndClearReportsHeld(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	since := time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)
	later := since.Add(5 * time.Minute)

	if err := store.NoteReportsHeld(since); err != nil {
		t.Fatalf("NoteReportsHeld: %v", err)
	}
	if err := store.NoteReportsHeld(later); err != nil {
		t.Fatalf("NoteReportsHeld second: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.ReportsHeld == nil || !state.ReportsHeld.Since.Equal(since) {
		t.Fatalf("ReportsHeld = %+v — the older hold must win", state.ReportsHeld)
	}
	if err := store.ClearReportsHeld(); err != nil {
		t.Fatalf("ClearReportsHeld: %v", err)
	}
	state, err = store.Load()
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if state.ReportsHeld != nil {
		t.Fatalf("cleared hold still recorded: %+v", state.ReportsHeld)
	}
	if err := store.NoteReportsHeld(time.Time{}); err == nil {
		t.Fatalf("zero since was accepted")
	}
}

// The daemon's startup reconcile calls ClearReportsHeld on every home,
// including ones the machinery never touched. A no-op clear must not write
// the file into existence — its mere presence opts the quota mover line
// in for a home that never enabled handoff (#6776).
func TestClearReportsHeldLeavesAbsentFileAbsent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	if err := store.ClearReportsHeld(); err != nil {
		t.Fatalf("ClearReportsHeld on untouched home: %v", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("ClearReportsHeld materialized %s — absent is itself the "+
			"'nothing recorded' answer (stat err=%v)", store.Path(), err)
	}
}

func TestReportsHeldWithoutSinceFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema_version": 1, "reports_held": {}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Load(); err == nil {
		t.Fatalf("reports_held without since was accepted")
	}
}

// The rails mutators (#6776 slice 5): recent-move records feed the
// switch-loop counter and the account credits, refusal backoff feeds the
// defer rail, and the halt record is the on_failure="halt" landing. Every
// one of these must round-trip durably and fail closed on garbage — the
// file is the rails' only memory across a daemon restart.
func TestRecordMoveRoundTrip(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if err := store.RecordMove("sess-1", "account-b", at); err != nil {
		t.Fatalf("RecordMove: %v", err)
	}
	if err := store.RecordMove("sess-1", "account-c", at.Add(time.Minute)); err != nil {
		t.Fatalf("RecordMove: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RecentMoves) != 2 || state.RecentMoves[0].SessionKey != "sess-1" ||
		state.RecentMoves[0].Account != "account-b" || !state.RecentMoves[0].At.Equal(at) {
		t.Fatalf("RecentMoves = %+v", state.RecentMoves)
	}
	if err := store.RecordMove("", "a", at); err == nil {
		t.Fatal("empty session key was accepted")
	}
	if err := store.RecordMove("k", "", at); err == nil {
		t.Fatal("empty account was accepted")
	}
	if err := store.RecordMove("k", "a", time.Time{}); err == nil {
		t.Fatal("zero timestamp was accepted")
	}
}

func TestSetAndClearRefused(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	until := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	for _, key := range []string{"sess-1", "sess-2"} {
		if err := store.SetRefusedUntil(key, until); err != nil {
			t.Fatalf("SetRefusedUntil: %v", err)
		}
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RefusedUntil) != 2 {
		t.Fatalf("RefusedUntil = %+v", state.RefusedUntil)
	}
	// A session-scoped clear drops only that session's entry.
	n, err := store.ClearRefused("sess-1")
	if err != nil {
		t.Fatalf("ClearRefused: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d entries, want 1", n)
	}
	state, err = store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RefusedUntil) != 1 || state.RefusedUntil["sess-1"].Equal(until) {
		t.Fatalf("scoped clear hit the wrong entries: %+v", state.RefusedUntil)
	}
	// An empty scope clears everything; clearing again is not an error.
	n, err = store.ClearRefused("")
	if err != nil {
		t.Fatalf("ClearRefused all: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
	if n, err = store.ClearRefused(""); err != nil || n != 0 {
		t.Fatalf("re-clear = %d, %v", n, err)
	}
	if err := store.SetRefusedUntil("", until); err == nil {
		t.Fatal("empty session key was accepted")
	}
	if err := store.SetRefusedUntil("k", time.Time{}); err == nil {
		t.Fatal("zero until was accepted")
	}
}

func TestClearMovesIsSessionScoped(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, mv := range [][2]string{{"sess-1", "a"}, {"sess-2", "a"}, {"sess-1", "b"}} {
		if err := store.RecordMove(mv[0], mv[1], at); err != nil {
			t.Fatalf("RecordMove: %v", err)
		}
	}
	n, err := store.ClearMoves("sess-1")
	if err != nil {
		t.Fatalf("ClearMoves: %v", err)
	}
	if n != 2 {
		t.Fatalf("cleared %d, want 2", n)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RecentMoves) != 1 || state.RecentMoves[0].SessionKey != "sess-2" {
		t.Fatalf("scoped clear hit the wrong entries: %+v", state.RecentMoves)
	}
}

func TestRecordAndClearHalt(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	if err := store.RecordHalt("move-failed", at); err != nil {
		t.Fatalf("RecordHalt: %v", err)
	}
	halt, err := store.HaltedAt()
	if err != nil {
		t.Fatalf("HaltedAt: %v", err)
	}
	if halt == nil || halt.Reason != "move-failed" || !halt.At.Equal(at) {
		t.Fatalf("halt = %+v", halt)
	}
	cleared, err := store.ClearHalt()
	if err != nil {
		t.Fatalf("ClearHalt: %v", err)
	}
	if cleared == nil || cleared.Reason != "move-failed" {
		t.Fatalf("cleared = %+v — the caller must learn what it removed", cleared)
	}
	if halt, err := store.HaltedAt(); err != nil || halt != nil {
		t.Fatalf("cleared halt still recorded: %+v, %v", halt, err)
	}
	if err := store.RecordHalt("", at); err == nil {
		t.Fatal("a reasonless halt was accepted — a halt must never be silent about why")
	}
	if err := store.RecordHalt("x", time.Time{}); err == nil {
		t.Fatal("a timeless halt was accepted")
	}
}

func TestHaltWithoutReasonFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema_version": 1, "halted": {"at": "2026-11-06T08:00:00Z"}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("a halted record without a reason was accepted")
	}
}

func TestPruneExpiredDropsOnlyDeadRecords(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := store.SetRefusedUntil("old", now.Add(-time.Minute)); err != nil {
		t.Fatalf("SetRefusedUntil: %v", err)
	}
	if err := store.SetRefusedUntil("live", now.Add(time.Hour)); err != nil {
		t.Fatalf("SetRefusedUntil: %v", err)
	}
	for _, mv := range []struct {
		key string
		at  time.Time
	}{{"in", now.Add(-10 * time.Minute)}, {"out", now.Add(-time.Hour)}} {
		if err := store.RecordMove(mv.key, "a", mv.at); err != nil {
			t.Fatalf("RecordMove: %v", err)
		}
	}
	if err := store.PruneExpired(30*time.Minute, now); err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RefusedUntil) != 1 || state.RefusedUntil["live"].IsZero() {
		t.Fatalf("live backoff was pruned: %+v", state.RefusedUntil)
	}
	if len(state.RecentMoves) != 1 || state.RecentMoves[0].SessionKey != "in" {
		t.Fatalf("in-window move was pruned: %+v", state.RecentMoves)
	}
	// An unbounded window keeps every move — the counters only read
	// in-window entries, so nothing is lost by keeping the history.
	if err := store.PruneExpired(0, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("PruneExpired unbounded: %v", err)
	}
	state, err = store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RecentMoves) != 1 {
		t.Fatalf("unbounded window dropped moves: %+v", state.RecentMoves)
	}
	if len(state.RefusedUntil) != 0 {
		t.Fatalf("the expired backoff outlived the clock: %+v", state.RefusedUntil)
	}
}
