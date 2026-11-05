package handoff

import (
	"encoding/json"
	"fmt"
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
	if _, err := rails.NoteUnrecordedCarry("/rollout-a", "/acct", 64, time.Now()); err != nil {
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
	if _, err := rails.NoteUnrecordedCarry("/rollout-b", "/acct", 64, time.Now()); err == nil {
		t.Fatal("the marker rebuilt over a corrupt rails file")
	}
}

// A quarantined rollout distrusts exactly the span its failed carry
// copied: the marker's covered length bounds the poison and wins over a
// stale recorded watermark — a carry that landed after it may have taken
// fresh source bytes past the recorded boundary. Retirement needs a
// watermark that REACHES the span: a shorter re-carry lifts distrust it
// cannot account for (Codex review on the merge semantics and on
// coverage-aware retirement, #6852).
func TestUnrecordedCarriesQuarantineTheCopiedSpan(t *testing.T) {
	rails, state := railsFixture(t)
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if _, err := rails.NoteUnrecordedCarry("/rollout-a", "/acct", 200, at); err != nil {
		t.Fatalf("NoteUnrecordedCarry: %v", err)
	}
	if err := state.RecordCarriedPrefix("/rollout-b", 64); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	snap := Snapshot{State: mustLoad(t, state), Rails: mustLoadRails(t, rails)}
	effective := snap.EffectiveCarriedPrefixes()
	if effective["/rollout-a"] != 200 {
		t.Fatalf("unrecorded span not quarantined: %v", effective["/rollout-a"])
	}
	if effective["/rollout-b"] != 64 {
		t.Fatalf("recorded watermark lost: %v", effective)
	}
	if err := state.RecordCarriedPrefix("/rollout-a", 128); err != nil {
		t.Fatal(err)
	}
	snap = Snapshot{State: mustLoad(t, state), Rails: mustLoadRails(t, rails)}
	if got := snap.EffectiveCarriedPrefixes()["/rollout-a"]; got != 200 {
		t.Fatalf("stale watermark overrode the quarantine: %d", got)
	}
	// A watermark SHORTER than the quarantined span retires nothing — the
	// bytes past it are still unaccounted source claims.
	if retired, err := rails.ClearUnrecordedCarry("/rollout-a", 128); err != nil || retired {
		t.Fatalf("a short watermark retired the marker: retired=%v err=%v", retired, err)
	}
	if got := mustLoadRails(t, rails).UnrecordedCarries["/rollout-a"].Covered; got != 200 {
		t.Fatalf("marker lost to a short watermark: %d", got)
	}
	// An absent marker clears trivially; one the watermark REACHES retires
	// and the recorded boundary governs again.
	if retired, err := rails.ClearUnrecordedCarry("/rollout-c", 10); err != nil || !retired {
		t.Fatalf("absent marker blocked the clear: retired=%v err=%v", retired, err)
	}
	if retired, err := rails.ClearUnrecordedCarry("/rollout-a", 256); err != nil || !retired {
		t.Fatalf("covering watermark did not retire the marker: retired=%v err=%v", retired, err)
	}
	snap = Snapshot{State: mustLoad(t, state), Rails: mustLoadRails(t, rails)}
	if got := snap.EffectiveCarriedPrefixes()["/rollout-a"]; got != 128 {
		t.Fatalf("retired marker still quarantines: %d", got)
	}
	if got := mustLoadRails(t, rails).UnrecordedCarries; len(got) != 0 {
		t.Fatalf("marker survived its clear: %+v", got)
	}
}

// A re-quarantine only ever WIDENS the covered span — the earlier failed
// carry's bytes are still unaccounted — while the first timestamp stands
// as when the span first went unrecorded.
func TestNoteUnrecordedCarryOnlyWidensTheSpan(t *testing.T) {
	rails, _ := railsFixture(t)
	first := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if _, err := rails.NoteUnrecordedCarry("/r", "/acct", 100, first); err != nil {
		t.Fatal(err)
	}
	if _, err := rails.NoteUnrecordedCarry("/r", "/acct", 300, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := mustLoadRails(t, rails).UnrecordedCarries["/r"]
	if got.Covered != 300 || !got.At.Equal(first) {
		t.Fatalf("marker = %+v, want covered 300 from %v", got, first)
	}
	if _, err := rails.NoteUnrecordedCarry("/r", "/acct", 50, first.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := mustLoadRails(t, rails).UnrecordedCarries["/r"]; got.Covered != 300 {
		t.Fatalf("a shorter re-quarantine shrank the span: %+v", got)
	}
}

// The first quarantining builds wrote the marker as a bare timestamp —
// its covered length is unknown, so it decodes as whole-file distrust and
// no watermark can retire it short of the file's end.
func TestUnrecordedCarryDecodesTheLegacyTimestampForm(t *testing.T) {
	rails, _ := railsFixture(t)
	if err := os.MkdirAll(filepath.Dir(rails.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema_version":1,"unrecorded_carries":{"/r.jsonl":"2026-11-05T18:00:00Z"}}`
	if err := os.WriteFile(rails.Path(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := rails.Load()
	if err != nil {
		t.Fatalf("legacy marker did not decode: %v", err)
	}
	marker := got.UnrecordedCarries["/r.jsonl"]
	if marker.Covered != math.MaxInt64 {
		t.Fatalf("legacy marker covered = %d, want MaxInt64 (whole-file distrust)", marker.Covered)
	}
	if snap := (Snapshot{Rails: got}); snap.EffectiveCarriedPrefixes()["/r.jsonl"] != math.MaxInt64 {
		t.Fatal("legacy marker did not quarantine the whole file")
	}
	if retired, err := rails.ClearUnrecordedCarry("/r.jsonl", 1<<40); err != nil || retired {
		t.Fatalf("a finite watermark retired a boundless marker: retired=%v err=%v", retired, err)
	}
}

// At the entry bound the quarantine NEVER evicts — a dropped marker
// re-trusts a file whose poison span is still on disk. The carry that
// cannot get a marker flags its ACCOUNT home instead: every standing
// marker stays, the overflowed file gets no marker, and the account is
// untrusted for retraction wholesale until a covering watermark retires
// the flag (#6852 review).
func TestNoteUnrecordedCarryOverflowFlagsTheAccountNeverEvicts(t *testing.T) {
	rails, _ := railsFixture(t)
	liveDir := t.TempDir()
	accountDir := filepath.Join(liveDir, "account-home")
	oldest := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := map[string]time.Time{}
	for i := 0; i < unrecordedCarryCap; i++ {
		entries[fmt.Sprintf("/gone/%d", i)] = oldest.Add(time.Duration(i) * time.Minute)
	}
	raw, err := json.Marshal(map[string]any{
		"schema_version":     1,
		"unrecorded_carries": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rails.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rails.Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The carry that lands over capacity gets NO marker — and no marker
	// is evicted to make room for one. (The seeded bare timestamps are
	// the legacy marker form — they decode with covered = MaxInt64.)
	newPath := filepath.Join(accountDir, "sessions", "new-carry.jsonl")
	at := time.Now()
	quarantined, err := rails.NoteUnrecordedCarry(newPath, accountDir, 64, at)
	if err != nil {
		t.Fatalf("NoteUnrecordedCarry: %v", err)
	}
	if !quarantined {
		t.Fatal("an overflowed carry must report the account quarantine")
	}
	got := mustLoadRails(t, rails)
	if len(got.UnrecordedCarries) != unrecordedCarryCap {
		t.Fatalf("the cap evicted or admitted a marker: %d entries", len(got.UnrecordedCarries))
	}
	if _, ok := got.UnrecordedCarries[newPath]; ok {
		t.Fatal("the overflowed carry got a marker anyway")
	}
	flag, ok := got.QuarantinedAccounts[filepath.Clean(accountDir)]
	if !ok {
		t.Fatalf("the account was not flagged: %+v", got.QuarantinedAccounts)
	}
	if flag.Path != newPath || flag.Covered != 64 || flag.At.IsZero() {
		t.Fatalf("overflow flag = %+v, want path %q covered 64 with a timestamp", flag, newPath)
	}
	if !(Snapshot{Rails: got}).AccountUntrusted(accountDir) {
		t.Fatal("AccountUntrusted did not see the overflow flag")
	}
}

// The overflow flag's Path/Covered let a covering watermark retire it by
// the same rule a marker follows — but a second overflow on a DIFFERENT
// file makes the account's poison untrackable: the flag then survives
// even a boundless watermark (nothing honest can name which file it
// covered).
func TestAccountQuarantineRetiresAndDemotesOnSecondOverflow(t *testing.T) {
	rails, _ := railsFixture(t)
	accountDir := filepath.Join(t.TempDir(), "account-home")
	capEntries := map[string]time.Time{}
	for i := 0; i < unrecordedCarryCap; i++ {
		capEntries[fmt.Sprintf("/gone/%d", i)] = time.Now()
	}
	raw, err := json.Marshal(map[string]any{
		"schema_version":     1,
		"unrecorded_carries": capEntries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rails.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rails.Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(accountDir, "rollout-1.jsonl")
	if _, err := rails.NoteUnrecordedCarry(first, accountDir, 100, time.Now()); err != nil {
		t.Fatal(err)
	}

	// A watermark short of the overflow's span retires nothing; one that
	// covers it lifts the account flag entirely.
	if retired, err := rails.ClearUnrecordedCarry(first, 50); err != nil || retired {
		t.Fatalf("short watermark retired the overflow: retired=%v err=%v", retired, err)
	}
	if !(Snapshot{Rails: mustLoadRails(t, rails)}).AccountUntrusted(accountDir) {
		t.Fatal("account unflagged by a short watermark")
	}
	if retired, err := rails.ClearUnrecordedCarry(first, 150); err != nil || !retired {
		t.Fatalf("covering watermark kept the flag: retired=%v err=%v", retired, err)
	}
	if (Snapshot{Rails: mustLoadRails(t, rails)}).AccountUntrusted(accountDir) {
		t.Fatal("a covering watermark did not lift the account flag")
	}

	// Overflow twice on different files: the flag turns untrackable and
	// no single-file watermark can lift it.
	if _, err := rails.NoteUnrecordedCarry(first, accountDir, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(accountDir, "rollout-2.jsonl")
	if _, err := rails.NoteUnrecordedCarry(second, accountDir, 20, time.Now()); err != nil {
		t.Fatal(err)
	}
	flag := mustLoadRails(t, rails).QuarantinedAccounts[filepath.Clean(accountDir)]
	if flag.Path != "" {
		t.Fatalf("a second overflow kept a file boundary: %+v", flag)
	}
	// A per-path clear has nothing named to retire — and the untrackable
	// account flag stands regardless of any watermark.
	if _, err := rails.ClearUnrecordedCarry(first, math.MaxInt64); err != nil {
		t.Fatalf("ClearUnrecordedCarry: %v", err)
	}
	if _, err := rails.ClearUnrecordedCarry(second, math.MaxInt64); err != nil {
		t.Fatalf("ClearUnrecordedCarry: %v", err)
	}
	if !(Snapshot{Rails: mustLoadRails(t, rails)}).AccountUntrusted(accountDir) {
		t.Fatal("an untrackable account flag was lifted by a watermark")
	}
}

// The merged snapshot unions account overflow flags across the two reads
// — an interleaved commit can only flag MORE accounts, never unflag one.
func TestLoadSnapshotUnionsQuarantinedAccounts(t *testing.T) {
	dir := t.TempDir()
	railReads := []Rails{
		{QuarantinedAccounts: map[string]AccountQuarantine{dir: {At: time.Now()}}},
		{},
	}
	ri := 0
	snap, _, _ := loadSnapshot(
		func() (State, error) { return State{}, nil },
		func() (Rails, error) { r := railReads[ri]; ri++; return r, nil })
	if !snap.AccountUntrusted(dir) {
		t.Fatal("an account flag present in the first cut was lost to the merge")
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

// A carry commit landing between the two naive file reads pairs a retired
// quarantine marker with the watermark it superseded: bytes the commit
// just disowned would count toward the target's wall. LoadSnapshot reads
// each file twice and merges worst-case — watermarks by per-path maximum,
// markers by union — so an interleaved commit can only ever over-exclude
// claims (Codex P2 on #6852).
func TestLoadSnapshotMergesAcrossAnInterleavedCommit(t *testing.T) {
	marked := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	// The scripted interleave: r1 sees the marker, s1 the old watermark,
	// then the commit lands — s2 sees the new watermark, r2 the cleared
	// marker. (loadSnapshot reads rails, state, rails, state.)
	states := []State{
		{CarriedPrefixes: map[string]int64{"/r.jsonl": 10}},
		{CarriedPrefixes: map[string]int64{"/r.jsonl": 42}},
	}
	railReads := []Rails{
		{UnrecordedCarries: map[string]UnrecordedCarry{"/r.jsonl": {At: marked, Covered: math.MaxInt64}}},
		{},
	}
	si, ri := 0, 0
	snap, stateErr, railsErr := loadSnapshot(
		func() (State, error) { s := states[si]; si++; return s, nil },
		func() (Rails, error) { r := railReads[ri]; ri++; return r, nil })
	if stateErr != nil || railsErr != nil {
		t.Fatalf("errs: %v %v", stateErr, railsErr)
	}
	if got := snap.EffectiveCarriedPrefixes()["/r.jsonl"]; got != math.MaxInt64 {
		t.Fatalf("effective prefix = %d, want MaxInt64 — a marker retired mid-read must still quarantine", got)
	}

	// The same watermark tear WITHOUT a marker in either read: the larger
	// boundary applies — the consistent post-commit cut, not the stale one.
	si, ri = 0, 0
	railReads[0] = Rails{}
	snap, stateErr, railsErr = loadSnapshot(
		func() (State, error) { s := states[si]; si++; return s, nil },
		func() (Rails, error) { r := railReads[ri]; ri++; return r, nil })
	if stateErr != nil || railsErr != nil {
		t.Fatalf("errs: %v %v", stateErr, railsErr)
	}
	if got := snap.EffectiveCarriedPrefixes()["/r.jsonl"]; got != 42 {
		t.Fatalf("effective prefix = %d, want 42 — the torn smaller watermark must not stand", got)
	}

	// Marker present in BOTH reads keeps quarantining even while the
	// watermark advanced underneath it.
	si, ri = 0, 0
	railReads[0] = Rails{UnrecordedCarries: map[string]UnrecordedCarry{"/r.jsonl": {At: marked, Covered: math.MaxInt64}}}
	railReads[1] = railReads[0]
	snap, _, _ = loadSnapshot(
		func() (State, error) { s := states[si]; si++; return s, nil },
		func() (Rails, error) { r := railReads[ri]; ri++; return r, nil })
	if got := snap.EffectiveCarriedPrefixes()["/r.jsonl"]; got != math.MaxInt64 {
		t.Fatalf("effective prefix = %d, want MaxInt64 — live marker wins", got)
	}
}

// Per-file decode errors surface independently — a caller degrading on one
// file keeps the other's successful read — and a read that fails on the
// second pass is reported too.
func TestLoadSnapshotReportsEachFileErrorIndependently(t *testing.T) {
	stateFail := fmt.Errorf("state corrupt")
	snap, stateErr, railsErr := loadSnapshot(
		func() (State, error) { return State{}, stateFail },
		func() (Rails, error) {
			return Rails{ReportsHeld: &ReportsHeld{Since: time.Now()}}, nil
		})
	if stateErr == nil || railsErr != nil {
		t.Fatalf("errs = %v / %v, want state only", stateErr, railsErr)
	}
	if snap.Rails.ReportsHeld == nil {
		t.Fatal("the healthy rails read was dropped with the state error")
	}

	// A second-pass failure after clean first reads is still an error —
	// the file went unreadable mid-load.
	calls := 0
	_, stateErr, railsErr = loadSnapshot(
		func() (State, error) { return State{}, nil },
		func() (Rails, error) {
			calls++
			if calls == 2 {
				return Rails{}, fmt.Errorf("vanished")
			}
			return Rails{}, nil
		})
	if stateErr != nil || railsErr == nil {
		t.Fatalf("errs = %v / %v, want second-pass rails error", stateErr, railsErr)
	}
}

// On real files with no writer, the merged snapshot equals either single
// load — the double read is invisible when nothing interleaves.
func TestLoadSnapshotConsistentWhenNothingInterleaves(t *testing.T) {
	rails, store := railsFixture(t)
	if err := store.RecordCarriedPrefix("/r.jsonl", 42); err != nil {
		t.Fatalf("RecordCarriedPrefix: %v", err)
	}
	if _, err := rails.NoteUnrecordedCarry("/q.jsonl", "/acct", 77, time.Now()); err != nil {
		t.Fatalf("NoteUnrecordedCarry: %v", err)
	}
	snap, stateErr, railsErr := LoadSnapshot(store, rails)
	if stateErr != nil || railsErr != nil {
		t.Fatalf("errs: %v %v", stateErr, railsErr)
	}
	eff := snap.EffectiveCarriedPrefixes()
	if eff["/r.jsonl"] != 42 || eff["/q.jsonl"] != 77 {
		t.Fatalf("effective prefixes = %v", eff)
	}
}
