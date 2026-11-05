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
