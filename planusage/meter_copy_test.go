package planusage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A copy of a backlog conversation outside the backlog's paths, such as a
// user's fork, can be scanned before the original. The copied message is then
// counted as the user's spend, and the original's copy is a duplicate; reading
// that duplicate under the backlog path moves the message's cost to backlog
// spend, once (#6618).
func TestScanAllMovesAMessageWhoseCopyWasScannedFirstToBacklog(t *testing.T) {
	home(t)
	config := t.TempDir()
	at := func(id, hhmm string) string {
		return strings.Replace(assistant(id, opus, 1_000_000, 0), "T10:01:00Z", "T"+hhmm+":00Z", 1)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	fork := filepath.Join(config, "projects", "-mine", "fork.jsonl")
	orig := filepath.Join(config, "projects", "-p", "c1.jsonl")
	// The fork holds the orchestrator's m1 and the user's own m3, in the same
	// bucket, and is older, so it is scanned first.
	write(t, fork, at("m1", "10:00"), at("m3", "10:00"))
	write(t, orig, at("m1", "10:00"), at("m2", "11:00"))
	touch := func(p string, ago time.Duration) {
		t.Helper()
		if err := os.Chtimes(p, now.Add(-ago), now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	touch(fork, 30*time.Minute)
	touch(orig, 10*time.Minute)
	sources := map[string][]string{DefaultAccount: {config}}
	backlog := []BacklogPath{{Path: orig, Within: []Span{{From: time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)}}}}
	sum := func(s *MeterState) (all, bl float64) {
		for _, b := range s.Accounts[DefaultAccount].Buckets {
			all += b.All
			bl += b.Backlog
		}
		return all, bl
	}

	s := NewMeterState()
	s.ScanAll(sources, backlog, nil, now)
	if all, bl := sum(s); !near(all, 12) || !near(bl, 8) {
		t.Fatalf("all %v backlog %v: m1 and m2 are the backlog's, m3 is the user's", all, bl)
	}

	// meter.json between passes; the original gets yet another copy of m1.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	s = &MeterState{}
	if err := json.Unmarshal(raw, s); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(orig, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(at("m1", "10:00") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	touch(orig, 5*time.Minute)
	s.ScanAll(sources, backlog, nil, now)
	if all, bl := sum(s); !near(all, 12) || !near(bl, 8) {
		t.Fatalf("all %v backlog %v: m1 moves to backlog spend once", all, bl)
	}
}

// State from before PlainCost, or from an older af, says nothing about how a
// counted message was attributed, so none of its messages moves: a duplicate
// of a message already counted as backlog spend (another content block, or a
// replay after a skipped model gets a price) must not add it again (Codex P2
// on #6624).
func TestScanAllNeverMovesAMessageItHasNoCountedCostFor(t *testing.T) {
	home(t)
	config := t.TempDir()
	at := func(id, hhmm string) string {
		return strings.Replace(assistant(id, opus, 1_000_000, 0), "T10:01:00Z", "T"+hhmm+":00Z", 1)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	orig := filepath.Join(config, "projects", "-p", "c1.jsonl")
	mine := filepath.Join(config, "projects", "-mine", "mine.jsonl")
	write(t, orig, at("m1", "10:00"))
	write(t, mine, at("m3", "10:00"))
	sources := map[string][]string{DefaultAccount: {config}}
	backlog := []BacklogPath{{Path: orig, Within: []Span{{From: time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)}}}}
	s := NewMeterState()
	s.ScanAll(sources, backlog, nil, now)
	// What an upgrade finds: the attribution, and no PlainCost.
	s.Accounts[DefaultAccount].PlainCost = nil

	// Another content block of m1 is appended, and the whole file is read
	// again from its start, as after a skipped model gets a price.
	f, err := os.OpenFile(orig, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(at("m1", "10:00") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	delete(s.Accounts[DefaultAccount].Files, orig)
	s.ScanAll(sources, backlog, nil, now)
	all, bl := 0.0, 0.0
	for _, b := range s.Accounts[DefaultAccount].Buckets {
		all += b.All
		bl += b.Backlog
	}
	if !near(all, 8) || !near(bl, 4) {
		t.Fatalf("all %v backlog %v: m1 is counted as backlog spend once", all, bl)
	}
}

// The cost a message moves with is the one counted for it, not its copy's at
// a price configured since (Codex P2 on #6624).
func TestScanAllMovesTheCostItCounted(t *testing.T) {
	home(t)
	config := t.TempDir()
	at := func(id, hhmm string) string {
		return strings.Replace(assistant(id, opus, 1_000_000, 0), "T10:01:00Z", "T"+hhmm+":00Z", 1)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	fork := filepath.Join(config, "projects", "-mine", "fork.jsonl")
	orig := filepath.Join(config, "projects", "-p", "c1.jsonl")
	write(t, fork, at("m1", "10:00"), at("m3", "10:00"))
	sources := map[string][]string{DefaultAccount: {config}}
	backlog := []BacklogPath{{Path: orig, Within: []Span{{From: time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)}}}}
	s := NewMeterState()
	s.ScanAll(sources, backlog, nil, now)

	// A later pass, from meter.json, after the original appears and opus's
	// input price has doubled.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	s = &MeterState{}
	if err := json.Unmarshal(raw, s); err != nil {
		t.Fatal(err)
	}
	write(t, orig, at("m1", "10:00"))
	s.ScanAll(sources, backlog, map[string]Price{opus: {Input: 8, Output: 40}}, now)
	all, bl := 0.0, 0.0
	for _, b := range s.Accounts[DefaultAccount].Buckets {
		all += b.All
		bl += b.Backlog
	}
	if !near(all, 8) || !near(bl, 4) {
		t.Fatalf("all %v backlog %v: m1 moves with the $4 counted for it", all, bl)
	}
}
