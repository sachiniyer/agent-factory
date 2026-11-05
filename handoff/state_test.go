package handoff

import (
	"encoding/json"
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

// A reset on a home with no file is a no-op, not a materialization: absent
// IS the "no halt" answer, and writing an empty state file would opt the
// quota mover line in for a home that never enabled handoff (#6776).
func TestClearHaltLeavesAbsentFileAbsent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "instances", StateFileName))
	cleared, err := store.ClearHalt()
	if err != nil || cleared != nil {
		t.Fatalf("ClearHalt on untouched home = %+v, %v", cleared, err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("ClearHalt materialized %s (stat err=%v)", store.Path(), err)
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

// The durable contract going forward: a read-modify-write preserves every
// top-level field the writing binary does not model, so a later slice's
// fields survive an older writer (#6776 review).
func TestMutatePreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema_version": 1, "future_thing": {"a": [1, 2]}, "carried_prefix_len": {"/x": 8}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.RecordCarriedPrefix("/y", 4); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(written, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["future_thing"]; !ok {
		t.Fatalf("mutate dropped the field it does not model: %s", written)
	}
	// And a field the mutation CLEARED must not resurrect through the
	// preserved copy: ClearHalt writes no halted key at all.
	if err := store.RecordHalt("why", time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClearHalt(); err != nil {
		t.Fatal(err)
	}
	written, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields = map[string]json.RawMessage{}
	if err := json.Unmarshal(written, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["halted"]; ok {
		t.Fatalf("cleared halt resurrected through field preservation: %s", written)
	}
}

// LOW-5 / #3672: a symlinked record file is a tampering signal — reads
// refuse to follow it, never load whatever it points at.
func TestLoadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"schema_version": 1, "halted": {"at": "2026-11-06T08:00:00Z", "reason": "x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "instances", StateFileName)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(link).Load(); err == nil {
		t.Fatal("a symlinked state file was followed")
	}
}

// The downgrade round trip with v1.0.345's exact field set: the release
// models {schema_version, carried_prefix_len, halted} and rewrites only
// those on every carry — so on downgrade the shared file loses anything
// extra while the rails file survives untouched. This is why the rails
// live in their own file (#6776 review).
func TestDowngradeRoundTripKeepsRailsAndHalt(t *testing.T) {
	dir := t.TempDir()
	instances := filepath.Join(dir, "instances")
	if err := os.MkdirAll(instances, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(instances, StateFileName)
	railsPath := filepath.Join(instances, RailsFileName)

	// A current build's home: records in BOTH files plus a field a future
	// slice will add.
	stateRaw := `{"schema_version": 1, "carried_prefix_len": {"/a": 12}, "halted": {"at": "2026-11-06T08:00:00Z", "reason": "x"}, "future_field": true}`
	if err := os.WriteFile(statePath, []byte(stateRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	railsRaw := `{"schema_version": 1, "recent_moves": [{"session_key": "s1", "agent": "codex", "account": "work", "at": "2026-11-06T09:00:00Z"}], "reports_held": {"since": "2026-11-06T09:30:00Z"}}`
	if err := os.WriteFile(railsPath, []byte(railsRaw), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate v1.0.345's writer: it decodes exactly its field set and
	// rewrites ONLY that — unknown keys do not survive its mutation.
	type oldState struct {
		SchemaVersion   int              `json:"schema_version"`
		CarriedPrefixes map[string]int64 `json:"carried_prefix_len,omitempty"`
		Halted          *Halt            `json:"halted,omitempty"`
	}
	var old oldState
	read, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read, &old); err != nil {
		t.Fatal(err)
	}
	old.CarriedPrefixes["/b"] = 99 // the carry's watermark write
	v345Out, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, v345Out, 0o600); err != nil {
		t.Fatal(err)
	}

	// The downgrade dropped the future field from the state file — that is
	// the documented cost — but every modeled field survived, including
	// the halt.
	after, err := NewStore(statePath).Load()
	if err != nil {
		t.Fatalf("v1.0.345 output does not load on the new build: %v", err)
	}
	if after.Halted == nil || after.CarriedPrefixes["/a"] != 12 || after.CarriedPrefixes["/b"] != 99 {
		t.Fatalf("downgraded state lost modeled records: %+v", after)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(v345Out, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["future_field"]; ok {
		t.Fatal("the old-binary simulation kept an unknown field — the fixture is wrong")
	}

	// The rails file is untouched — a file the release never opens cannot
	// lose records to its rewrite.
	railsRead, err := os.ReadFile(railsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(railsRead) != railsRaw {
		t.Fatalf("downgrade touched the rails file: %s", railsRead)
	}
	rails, err := NewRailsStore(railsPath, statePath).Load()
	if err != nil {
		t.Fatalf("rails did not load after downgrade: %v", err)
	}
	if len(rails.RecentMoves) != 1 || rails.RecentMoves[0].Agent != "codex" || rails.ReportsHeld == nil {
		t.Fatalf("rails records did not survive: %+v", rails)
	}
}
