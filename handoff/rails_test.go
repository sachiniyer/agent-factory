package handoff

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// railsFixture returns a store pair in a fresh instances dir.
func railsFixture(t *testing.T) (*RailsStore, *Store) {
	t.Helper()
	instances := filepath.Join(t.TempDir(), "instances")
	return NewRailsStore(
		filepath.Join(instances, RailsFileName),
		filepath.Join(instances, StateFileName),
	), NewStore(filepath.Join(instances, StateFileName))
}

func TestRecordMoveRoundTrip(t *testing.T) {
	rails, _ := railsFixture(t)
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if err := rails.RecordMove("sess-1", "codex", "account-b", at); err != nil {
		t.Fatalf("RecordMove: %v", err)
	}
	if err := rails.RecordMove("sess-1", "codex", "account-c", at.Add(time.Minute)); err != nil {
		t.Fatalf("RecordMove: %v", err)
	}
	state, err := rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RecentMoves) != 2 || state.RecentMoves[0].SessionKey != "sess-1" ||
		state.RecentMoves[0].Agent != "codex" || state.RecentMoves[0].Account != "account-b" ||
		!state.RecentMoves[0].At.Equal(at) {
		t.Fatalf("RecentMoves = %+v", state.RecentMoves)
	}
	for _, tc := range [][4]string{
		{"", "codex", "a", "x"}, {"k", "", "a", "x"}, {"k", "codex", "", "x"},
	} {
		if err := rails.RecordMove(tc[0], tc[1], tc[2], at); err == nil {
			t.Fatalf("empty field accepted: %v", tc)
		}
	}
	if err := rails.RecordMove("k", "codex", "a", time.Time{}); err == nil {
		t.Fatal("zero timestamp was accepted")
	}
}

func TestSetAndClearRefused(t *testing.T) {
	rails, _ := railsFixture(t)
	until := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	for _, key := range []string{"sess-1", "sess-2"} {
		if err := rails.SetRefusedUntil(key, until); err != nil {
			t.Fatalf("SetRefusedUntil: %v", err)
		}
	}
	state, err := rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RefusedUntil) != 2 {
		t.Fatalf("RefusedUntil = %+v", state.RefusedUntil)
	}
	// A session-scoped clear drops only that session's entry.
	n, err := rails.ClearRefused("sess-1")
	if err != nil {
		t.Fatalf("ClearRefused: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d entries, want 1", n)
	}
	state, err = rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RefusedUntil) != 1 || state.RefusedUntil["sess-1"].Equal(until) {
		t.Fatalf("scoped clear hit the wrong entries: %+v", state.RefusedUntil)
	}
	// An empty scope clears everything; clearing again is not an error.
	n, err = rails.ClearRefused("")
	if err != nil {
		t.Fatalf("ClearRefused all: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
	if n, err = rails.ClearRefused(""); err != nil || n != 0 {
		t.Fatalf("re-clear = %d, %v", n, err)
	}
	if err := rails.SetRefusedUntil("", until); err == nil {
		t.Fatal("empty session key was accepted")
	}
	if err := rails.SetRefusedUntil("k", time.Time{}); err == nil {
		t.Fatal("zero until was accepted")
	}
}

func TestClearMovesIsSessionScoped(t *testing.T) {
	rails, _ := railsFixture(t)
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, mv := range [][3]string{{"sess-1", "codex", "a"}, {"sess-2", "codex", "a"}, {"sess-1", "claude", "b"}} {
		if err := rails.RecordMove(mv[0], mv[1], mv[2], at); err != nil {
			t.Fatalf("RecordMove: %v", err)
		}
	}
	n, err := rails.ClearMoves("sess-1")
	if err != nil {
		t.Fatalf("ClearMoves: %v", err)
	}
	if n != 2 {
		t.Fatalf("cleared %d, want 2", n)
	}
	state, err := rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.RecentMoves) != 1 || state.RecentMoves[0].SessionKey != "sess-2" {
		t.Fatalf("scoped clear hit the wrong entries: %+v", state.RecentMoves)
	}
}

// A reset on a home with no rails file writes nothing — absent IS the
// "no records" answer (#6776 review LOW 7).
func TestClearsLeaveAbsentFileAbsent(t *testing.T) {
	rails, _ := railsFixture(t)
	if n, err := rails.ClearMoves(""); err != nil || n != 0 {
		t.Fatalf("ClearMoves = %d, %v", n, err)
	}
	if n, err := rails.ClearRefused(""); err != nil || n != 0 {
		t.Fatalf("ClearRefused = %d, %v", n, err)
	}
	if _, err := os.Stat(rails.Path()); !os.IsNotExist(err) {
		t.Fatalf("a no-op clear materialized %s (stat err=%v)", rails.Path(), err)
	}
}

// #6818 final review: a held report backlog is recorded durably so af
// quota's mover line can say it — first-writer-wins, cleared on drain.
func TestNoteAndClearReportsHeld(t *testing.T) {
	rails, _ := railsFixture(t)
	since := time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)
	later := since.Add(5 * time.Minute)

	if err := rails.NoteReportsHeld(since); err != nil {
		t.Fatalf("NoteReportsHeld: %v", err)
	}
	if err := rails.NoteReportsHeld(later); err != nil {
		t.Fatalf("NoteReportsHeld second: %v", err)
	}
	state, err := rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.ReportsHeld == nil || !state.ReportsHeld.Since.Equal(since) {
		t.Fatalf("ReportsHeld = %+v — the older hold must win", state.ReportsHeld)
	}
	if err := rails.ClearReportsHeld(); err != nil {
		t.Fatalf("ClearReportsHeld: %v", err)
	}
	state, err = rails.Load()
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if state.ReportsHeld != nil {
		t.Fatalf("cleared hold still recorded: %+v", state.ReportsHeld)
	}
	if err := rails.NoteReportsHeld(time.Time{}); err == nil {
		t.Fatalf("zero since was accepted")
	}
}

// The daemon's startup reconcile calls ClearReportsHeld on every home,
// including ones the machinery never touched. A no-op clear must not write
// the file into existence — its mere presence opts the quota mover line
// in for a home that never enabled handoff (#6776).
func TestClearReportsHeldLeavesAbsentFileAbsent(t *testing.T) {
	rails, _ := railsFixture(t)
	if err := rails.ClearReportsHeld(); err != nil {
		t.Fatalf("ClearReportsHeld on untouched home: %v", err)
	}
	if _, err := os.Stat(rails.Path()); !os.IsNotExist(err) {
		t.Fatalf("ClearReportsHeld materialized %s — absent is itself the "+
			"'nothing recorded' answer (stat err=%v)", rails.Path(), err)
	}
}

func TestReportsHeldWithoutSinceFailsClosed(t *testing.T) {
	rails, _ := railsFixture(t)
	if err := os.MkdirAll(filepath.Dir(rails.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rails.Path(), []byte(`{"schema_version": 1, "reports_held": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rails.Load(); err == nil {
		t.Fatalf("reports_held without since was accepted")
	}
}

func TestPruneExpiredDropsOnlyDeadRecords(t *testing.T) {
	rails, _ := railsFixture(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if err := rails.SetRefusedUntil("old", now.Add(-time.Minute)); err != nil {
		t.Fatalf("SetRefusedUntil: %v", err)
	}
	if err := rails.SetRefusedUntil("live", now.Add(time.Hour)); err != nil {
		t.Fatalf("SetRefusedUntil: %v", err)
	}
	for _, mv := range []struct {
		key string
		at  time.Time
	}{{"in", now.Add(-10 * time.Minute)}, {"out", now.Add(-time.Hour)}} {
		if err := rails.RecordMove(mv.key, "codex", "a", mv.at); err != nil {
			t.Fatalf("RecordMove: %v", err)
		}
	}
	if err := rails.PruneExpired(30*time.Minute, now); err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	state, err := rails.Load()
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
	if err := rails.PruneExpired(0, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("PruneExpired unbounded: %v", err)
	}
	state, err = rails.Load()
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

// PruneExpired is a GC, not a writer: an untouched home stays file-free.
func TestPruneExpiredLeavesAbsentFileAbsent(t *testing.T) {
	rails, _ := railsFixture(t)
	if err := rails.PruneExpired(30*time.Minute, time.Now()); err != nil {
		t.Fatalf("PruneExpired on untouched home: %v", err)
	}
	if _, err := os.Stat(rails.Path()); !os.IsNotExist(err) {
		t.Fatalf("PruneExpired materialized %s (stat err=%v)", rails.Path(), err)
	}
}

// A pre-split home kept its rails records inside the state file: an absent
// rails file lifts them at load, and the next mutation writes the rails
// file and strips the migrated keys — one upgrade path, no operator step.
func TestLiftMigratesPresplitRecords(t *testing.T) {
	rails, state := railsFixture(t)
	if err := os.MkdirAll(filepath.Dir(state.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema_version": 1, "carried_prefix_len": {"/a": 5}, "recent_moves": [{"session_key": "s1", "account": "work", "at": "2026-11-06T09:00:00Z"}], "refused_until": {"s2": "2026-11-06T11:00:00Z"}, "reports_held": {"since": "2026-11-06T09:30:00Z"}}`
	if err := os.WriteFile(state.Path(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	lifted, err := rails.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(lifted.RecentMoves) != 1 || lifted.RecentMoves[0].Account != "work" ||
		len(lifted.RefusedUntil) != 1 || lifted.ReportsHeld == nil {
		t.Fatalf("lifted = %+v", lifted)
	}
	// The lift writes nothing — a read stays a read.
	if _, err := os.Stat(rails.Path()); !os.IsNotExist(err) {
		t.Fatalf("Load materialized the rails file (stat err=%v)", err)
	}
	// The next mutation converges: rails gets the records, the state file
	// loses the migrated keys and keeps what is its own.
	if err := rails.SetRefusedUntil("s3", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetRefusedUntil: %v", err)
	}
	stateFields := map[string]json.RawMessage{}
	written, err := os.ReadFile(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(written, &stateFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"recent_moves", "refused_until", "reports_held", "unrecorded_carries"} {
		if _, ok := stateFields[key]; ok {
			t.Fatalf("migrated key %s still sits in the state file: %s", key, written)
		}
	}
	if _, ok := stateFields["carried_prefix_len"]; !ok {
		t.Fatalf("the migration ate a watermark: %s", written)
	}
	after, err := rails.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RecentMoves) != 1 || len(after.RefusedUntil) != 2 || after.ReportsHeld == nil {
		t.Fatalf("post-migration rails = %+v", after)
	}
}

// The marker exists to outlive exactly the corruption that ate the
// watermark write: a corrupt STATE file must not refuse it — that file
// holds no readable records to preserve. A corrupt RAILS file, on the
// other hand, still fails closed.
func TestNoteUnrecordedCarryLandsOverACorruptStateFile(t *testing.T) {
	rails, state := railsFixture(t)
	if err := os.MkdirAll(filepath.Dir(state.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rails.NoteUnrecordedCarry("/rollout-a", time.Now()); err != nil {
		t.Fatalf("the marker must outlive the corrupt watermark file: %v", err)
	}
	got := mustLoadRails(t, rails).UnrecordedCarries
	if len(got) != 1 {
		t.Fatalf("marker not recorded: %+v", got)
	}
	// But a corrupt rails file fails closed like every other record.
	if err := os.WriteFile(rails.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rails.NoteUnrecordedCarry("/rollout-b", time.Now()); err == nil {
		t.Fatal("the marker rebuilt over a corrupt rails file")
	}
}

func TestUnrecordedCarriesQuarantineWholeFile(t *testing.T) {
	rails, state := railsFixture(t)
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if err := rails.NoteUnrecordedCarry("/rollout-a", at); err != nil {
		t.Fatalf("NoteUnrecordedCarry: %v", err)
	}
	if err := state.RecordCarriedPrefix("/rollout-b", 64); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	snap := Snapshot{State: mustLoad(t, state), Rails: mustLoadRails(t, rails)}
	effective := snap.EffectiveCarriedPrefixes()
	if effective["/rollout-a"] != math.MaxInt64 {
		t.Fatalf("unrecorded file is not fully quarantined: %v", effective["/rollout-a"])
	}
	if effective["/rollout-b"] != 64 {
		t.Fatalf("recorded watermark lost: %v", effective)
	}
	// A recorded watermark wins over a stale unrecorded marker — the real
	// boundary replaces the blanket distrust.
	if err := state.RecordCarriedPrefix("/rollout-a", 128); err != nil {
		t.Fatal(err)
	}
	snap = Snapshot{State: mustLoad(t, state), Rails: mustLoadRails(t, rails)}
	if got := snap.EffectiveCarriedPrefixes()["/rollout-a"]; got != 128 {
		t.Fatalf("recorded watermark lost to the marker: %d", got)
	}
	// And clearing the marker writes nothing when there is none.
	if err := rails.ClearUnrecordedCarry("/rollout-c"); err != nil {
		t.Fatal(err)
	}
	if err := rails.ClearUnrecordedCarry("/rollout-a"); err != nil {
		t.Fatal(err)
	}
	if got := mustLoadRails(t, rails).UnrecordedCarries; len(got) != 0 {
		t.Fatalf("marker survived its clear: %+v", got)
	}
}

func mustLoad(t *testing.T, s *Store) State {
	t.Helper()
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustLoadRails(t *testing.T, s *RailsStore) Rails {
	t.Helper()
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Same preservation contract as the state file: an unknown top-level field
// survives a rewrite, a cleared known field does not resurrect.
func TestRailsMutatePreservesUnknownFields(t *testing.T) {
	rails, _ := railsFixture(t)
	if err := os.MkdirAll(filepath.Dir(rails.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rails.Path(), []byte(`{"schema_version": 1, "future_thing": {"a": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rails.SetRefusedUntil("s1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(rails.Path())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(written, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["future_thing"]; !ok {
		t.Fatalf("rails mutate dropped an unknown field: %s", written)
	}
}
