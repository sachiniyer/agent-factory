package planusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	return dir
}

func TestScanBucketsBacklogAndSurvivesRestart(t *testing.T) {
	home(t)
	config := t.TempDir()
	item := filepath.Join(config, "projects", "-wt-item")
	write(t, filepath.Join(item, "s1.jsonl"), assistant("m1", opus, 1_000_000, 0), assistant("m1", opus, 1_000_000, 0))
	write(t, filepath.Join(item, "s1", "subagents", "agent-a.jsonl"), assistant("m2", opus, 1_000_000, 0))
	write(t, filepath.Join(config, "projects", "-home-me-own", "s2.jsonl"), assistant("m3", opus, 0, 1_000_000))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	s := NewMeterState()
	if !s.Scan(DefaultAccount, config, []string{item}, nil, now) {
		t.Fatal("first scan changed nothing")
	}
	key := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC).Unix()
	if b := s.Accounts[DefaultAccount].Buckets[key]; !near(b.All, 28) || !near(b.Backlog, 8) {
		t.Fatalf("%+v", s.Accounts[DefaultAccount].Buckets)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMeter()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Scan(DefaultAccount, config, []string{item}, nil, now) {
		t.Fatal("a restart scanned something twice")
	}
	if b := loaded.Accounts[DefaultAccount].Buckets[key]; !near(b.All, 28) {
		t.Fatalf("%+v", b)
	}

	reading := &Reading{
		FiveHour: &Limit{UsedPercentage: 40, ResetsAt: time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC).Unix()},
		SevenDay: &Limit{UsedPercentage: 10, ResetsAt: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC).Unix()},
	}
	w := Attribute(reading, loaded.Accounts[DefaultAccount], now)
	if !w.FiveHour.Valid() || w.FiveHour.Plan != 40 || !near(w.FiveHour.Backlog, 40*8.0/28) {
		t.Fatalf("five hour %+v", w.FiveHour)
	}
	if w.SevenDay.Valid() || w.SevenDay.Plan != 0 || w.SevenDay.Backlog != 0 {
		t.Fatalf("a window past its reset should count as 0: %+v", w.SevenDay)
	}
}

func TestScanSkipsOldFilesAndPrunes(t *testing.T) {
	home(t)
	config := t.TempDir()
	old := filepath.Join(config, "projects", "-p", "old.jsonl")
	write(t, old, assistant("m1", opus, 1_000_000, 0))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stamp := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := NewMeterState()
	s.account("x").Buckets[now.Add(-9*24*time.Hour).Unix()] = Bucket{All: 1}
	s.Accounts["x"].Files["/gone.jsonl"] = FileMark{Offset: 3}
	s.account(DefaultAccount).Files[old] = FileMark{Offset: 1}
	s.Scan(DefaultAccount, config, nil, nil, now)
	s.Scan("x", t.TempDir(), nil, nil, now)
	a := s.Accounts[DefaultAccount]
	if len(a.Buckets) != 0 || len(a.Files) != 0 {
		t.Fatalf("an old file was read: %+v", a)
	}
	if len(s.Accounts["x"].Buckets) != 0 || len(s.Accounts["x"].Files) != 0 {
		t.Fatalf("not pruned: %+v", s.Accounts["x"])
	}
}

func TestLoadMeterRefusesNewerSchema(t *testing.T) {
	h := home(t)
	if err := os.MkdirAll(filepath.Join(h, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, DirName, MeterFileName), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeter(); err == nil {
		t.Fatal("newer schema accepted")
	}
}

func TestAttributeWithoutReading(t *testing.T) {
	w := Attribute(nil, nil, time.Now())
	if w.FiveHour.Valid() || w.SevenDay.Valid() {
		t.Fatal(w)
	}
}

func TestScanCountsResumedCopiesOnceAndReportsUnpriced(t *testing.T) {
	home(t)
	config := t.TempDir()
	project := filepath.Join(config, "projects", "-p")
	write(t, filepath.Join(project, "a.jsonl"), assistant("m1", opus, 1_000_000, 0))
	write(t, filepath.Join(project, "b.jsonl"), assistant("m1", opus, 1_000_000, 0), assistant("u", "claude-x-9", 1, 1), assistant("m2", opus, 1_000_000, 0))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := NewMeterState()
	s.Scan(DefaultAccount, config, nil, nil, now)
	a := s.Accounts[DefaultAccount]
	all, _ := a.costIn(now.Add(-time.Hour*5), now)
	if !near(all, 8) || len(a.Unpriced) != 1 || a.Unpriced[0] != "claude-x-9" {
		t.Fatalf("all %v, unpriced %v", all, a.Unpriced)
	}
	if u := AccountUsageOf(nil, a, nil, now); len(u.Unpriced) != 1 || u.ReadingAt != nil {
		t.Fatalf("%+v", u)
	}
	if u := AccountUsageOf(nil, a, map[string]Price{"claude-x-9": {Input: 1}}, now); len(u.Unpriced) != 0 {
		t.Fatalf("a priced model is still reported: %+v", u)
	}
}

func TestScanSkipsUnchangedFilesWithoutOpeningThem(t *testing.T) {
	home(t)
	config := t.TempDir()
	path := filepath.Join(config, "projects", "-p", "a.jsonl")
	write(t, path, assistant("m1", opus, 1_000_000, 0))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stamp := now.Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := NewMeterState()
	s.Scan(DefaultAccount, config, nil, nil, now)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if s.Scan(DefaultAccount, config, nil, nil, now) {
		t.Fatal("an unchanged file was read")
	}
	if _, ok := s.Accounts[DefaultAccount].Files[path]; !ok {
		t.Fatal("the mark of an unchanged file was dropped")
	}
}

func TestRunWindowCost(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := NewMeterState()
	s.AddRunEntries("s1", "acct", []Entry{
		{At: now.Add(-time.Hour), Cost: 2},
		{At: now.Add(-time.Hour + time.Minute), Cost: 1},
		{At: now.Add(-7 * time.Hour), Cost: 50},
	})
	reset := now.Add(2 * time.Hour)
	w := PlanWindow{Plan: 10, ResetsAt: &reset}
	if got := s.RunWindowCost("s1", "acct", w); !near(got, 3) {
		t.Fatalf("window cost %v, want 3", got)
	}
	if got := s.RunWindowCost("s1", "acct", PlanWindow{}); got != 0 {
		t.Fatalf("no window: %v", got)
	}
	s.PruneRunBuckets(now)
	if len(s.RunBuckets["s1|acct"]) != 1 {
		t.Fatalf("old buckets kept: %v", s.RunBuckets)
	}
	s.KeepRunSeen(map[string]bool{})
	if len(s.RunBuckets) != 0 {
		t.Fatalf("an ended run's buckets kept: %v", s.RunBuckets)
	}
}

func TestAttributeReportsAllCost(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a := &AccountMeter{Buckets: map[int64]Bucket{now.Add(-time.Hour).Unix(): {All: 8, Backlog: 2}}}
	r := &Reading{FiveHour: &Limit{UsedPercentage: 40, ResetsAt: now.Add(time.Hour).Unix()}}
	if w := Attribute(r, a, now).FiveHour; w.All != 8 || !near(w.Backlog, 10) {
		t.Fatalf("%+v", w)
	}
}

// A model skipped for want of a price is priced later, once the user adds it
// (Codex P2 on #6299): the file is read again from the start, and what was
// already counted does not count twice.
func TestScanPricesSkippedMessagesOnceTheModelHasAPrice(t *testing.T) {
	home(t)
	config := t.TempDir()
	file := filepath.Join(config, "projects", "-p", "a.jsonl")
	write(t, file, assistant("m1", opus, 1_000_000, 0), assistant("u1", "claude-x-9", 1_000_000, 0))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := NewMeterState()
	s.Scan(DefaultAccount, config, nil, nil, now)
	a := s.Accounts[DefaultAccount]
	if all, _ := a.costIn(now.Add(-5*time.Hour), now); !near(all, 4) {
		t.Fatalf("before the price: %v", all)
	}
	if a.Files[file].Skipped != "claude-x-9" {
		t.Fatalf("mark %+v", a.Files[file])
	}
	prices := map[string]Price{"claude-x-9": {Input: 2}}
	if !s.Scan(DefaultAccount, config, nil, prices, now) {
		t.Fatal("a newly priced model did not trigger a re-read")
	}
	if all, _ := a.costIn(now.Add(-5*time.Hour), now); !near(all, 6) {
		t.Fatalf("after the price: %v, want the skipped message (2) added once to the first (4)", all)
	}
	if s.Scan(DefaultAccount, config, nil, prices, now) {
		t.Fatal("a third scan re-read the file")
	}
}

// A conversation copied into another account's config dir by a handoff consumed
// the SOURCE account's plan; the copy must not count for the destination (Codex
// P2 on #6299), whichever account is listed first.
func TestScanDoesNotCountACarriedConversationForTheDestination(t *testing.T) {
	home(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "projects", "-p", "c.jsonl")
	dstFile := filepath.Join(dst, "projects", "-p", "c.jsonl")
	write(t, srcFile, assistant("m1", opus, 1_000_000, 0), assistant("m2", opus, 1_000_000, 0))
	// The copy, then one message made under the destination identity.
	write(t, dstFile, assistant("m1", opus, 1_000_000, 0), assistant("m2", opus, 1_000_000, 0), assistant("m3", opus, 1_000_000, 0))
	old := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	if err := os.Chtimes(srcFile, old, old); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(dstFile, now, now); err != nil {
		t.Fatal(err)
	}
	s := NewMeterState()
	s.ScanAll(map[string][]string{"aaa-dest": {dst}, "zzz-source": {src}}, nil, nil, now)
	cost := func(name string) float64 {
		all, _ := s.Accounts[name].costIn(now.Add(-5*time.Hour), now.Add(time.Hour))
		return all
	}
	if !near(cost("zzz-source"), 8) || !near(cost("aaa-dest"), 4) {
		t.Fatalf("source %v (want 8), destination %v (want only its own message, 4)", cost("zzz-source"), cost("aaa-dest"))
	}
}

// A run's dedupe keys and buckets can be put back when its cost could not be
// committed (Codex P2 on #6299), and its transcript directories are remembered
// across the worktree being archived.
func TestRunSnapshotRestoreAndRememberedDirs(t *testing.T) {
	s := NewMeterState()
	seen := s.RunSeenFor("s1")
	seen["a"] = 1
	s.AddRunEntries("s1", "acct", []Entry{{At: time.Date(2026, 10, 20, 10, 1, 0, 0, time.UTC), Cost: 3}})
	snap := s.SnapshotRun("s1")
	s.RunSeenFor("s1")["b"] = 2
	s.AddRunEntries("s1", "acct", []Entry{{At: time.Date(2026, 10, 20, 10, 2, 0, 0, time.UTC), Cost: 5}})
	s.RestoreRun("s1", snap)
	if len(s.RunSeen["s1"]) != 1 || !near(s.RunBuckets["s1|acct"][time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC).Unix()], 3) {
		t.Fatalf("not restored: %+v %+v", s.RunSeen["s1"], s.RunBuckets["s1|acct"])
	}
	s.RestoreRun("none", RunSnapshot{Seen: map[string]int64{}, Buckets: map[string]map[int64]float64{}})
	if _, ok := s.RunBuckets["none|x"]; ok {
		t.Fatal("an empty snapshot left an empty bucket map behind")
	}
	if dirs := s.RememberRunDirs("s1", []string{"/a", "/b"}); len(dirs) != 2 {
		t.Fatal(dirs)
	}
	if dirs := s.RememberRunDirs("s1", []string{"/b", "/c"}); len(dirs) != 3 {
		t.Fatalf("the earlier directories must stay after a later pass sees only the new one: %v", dirs)
	}
	s.KeepRunSeen(map[string]bool{})
	if len(s.RunDirs) != 0 {
		t.Fatal("dirs outlived the session")
	}
}

// A session handed from one account to another keeps each account's spend apart,
// so a window sums only what was spent under its own account (Codex P2 on #6299).
func TestRunWindowCostPartitionsByAccount(t *testing.T) {
	s := NewMeterState()
	at := time.Date(2026, 10, 20, 10, 1, 0, 0, time.UTC)
	s.AddRunEntries("s1", "source", []Entry{{At: at, Cost: 5}})
	s.AddRunEntries("s1", "dest", []Entry{{At: at.Add(time.Minute), Cost: 2}})
	reset := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := PlanWindow{ResetsAt: &reset}
	if got := s.RunWindowCost("s1", "dest", w); !near(got, 2) {
		t.Fatalf("destination window = %v, want only its own 2", got)
	}
	if got := s.RunWindowCost("s1", "source", w); !near(got, 5) {
		t.Fatalf("source window = %v", got)
	}
	snap := s.SnapshotRun("s1")
	s.AddRunEntries("s1", "dest", []Entry{{At: at, Cost: 9}})
	s.RestoreRun("s1", snap)
	if got := s.RunWindowCost("s1", "dest", w); !near(got, 2) {
		t.Fatalf("restored destination window = %v", got)
	}
	s.KeepRunSeen(map[string]bool{})
	if len(s.RunBuckets) != 0 {
		t.Fatal("buckets outlived the session")
	}
}
