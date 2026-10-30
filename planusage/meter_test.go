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
	s.AddRunEntries("s1", []Entry{
		{At: now.Add(-time.Hour), Cost: 2},
		{At: now.Add(-time.Hour + time.Minute), Cost: 1},
		{At: now.Add(-7 * time.Hour), Cost: 50},
	})
	reset := now.Add(2 * time.Hour)
	w := PlanWindow{Plan: 10, ResetsAt: &reset}
	if got := s.RunWindowCost("s1", w); !near(got, 3) {
		t.Fatalf("window cost %v, want 3", got)
	}
	if got := s.RunWindowCost("s1", PlanWindow{}); got != 0 {
		t.Fatalf("no window: %v", got)
	}
	s.PruneRunBuckets(now)
	if len(s.RunBuckets["s1"]) != 1 {
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
