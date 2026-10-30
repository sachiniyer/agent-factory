package backlog

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

func transcriptLine(id, model string, in int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-20T10:00:00Z","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", id, model, in)
}

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMeterRunAcrossPassesAndSubagents(t *testing.T) {
	project := filepath.Join(t.TempDir(), "projects", "-wt")
	main := filepath.Join(project, "s.jsonl")
	sub := filepath.Join(project, "s", "subagents", "agent-1.jsonl")
	writeFile(t, main, transcriptLine("m1", "claude-opus-5-5", 1_000_000), transcriptLine("m1", "claude-opus-5-5", 1_000_000))
	writeFile(t, sub, transcriptLine("m2", "claude-sonnet-5-5", 1_000_000))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	run := RunRef{SessionID: "s1", StartedAt: now.Add(-time.Hour)}
	files := planusage.TranscriptFiles([]string{project})

	m := MeterRun(3, run, files, map[string]int64{}, nil, now)
	if !m.Changed || m.Unmeterable != "" || math.Abs(m.Cost()-6) > 1e-9 || len(m.Meter) != 2 || len(m.Entries) != 2 {
		t.Fatalf("%+v", m)
	}
	run.Meter = m.Meter
	again := MeterRun(3, run, files, map[string]int64{}, nil, now)
	if again.Changed || math.Abs(again.Cost()-6) > 1e-9 || len(again.Entries) != 0 {
		t.Fatalf("a second pass counted again: %+v", again)
	}

	b := New()
	b.Items = []Item{{ID: 3, Status: StatusRunning, Runs: []RunRef{{SessionID: "s1"}, {SessionID: "s2"}}}}
	if !b.SetRunMeters(map[string]map[string]Meter{"s1": m.Meter}) || b.SetRunMeters(map[string]map[string]Meter{"s1": m.Meter}) {
		t.Fatal("SetRunMeters change report")
	}
	if b.Items[0].Runs[0].Cost() != m.Cost() || b.Items[0].Runs[1].Meter != nil {
		t.Fatalf("%+v", b.Items[0].Runs)
	}
}

func TestMeterRunUnmeterable(t *testing.T) {
	project := filepath.Join(t.TempDir(), "projects", "-wt")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	young := RunRef{StartedAt: now.Add(-time.Minute)}
	if m := MeterRun(12, young, nil, nil, nil, now); m.Unmeterable != "" {
		t.Fatalf("a young run with no files: %q", m.Unmeterable)
	}
	old := RunRef{StartedAt: now.Add(-3 * time.Minute)}
	if m := MeterRun(12, old, nil, nil, nil, now); !strings.Contains(m.Unmeterable, "cannot find the transcripts of B-12") {
		t.Fatalf("%q", m.Unmeterable)
	}
	f := filepath.Join(project, "s.jsonl")
	writeFile(t, f, transcriptLine("m1", "claude-x-9", 5))
	m := MeterRun(12, old, []string{f}, nil, nil, now)
	if m.Unmeterable != `no price for model claude-x-9; add [backlog.prices."claude-x-9"] to config.toml` || m.Cost() != 0 {
		t.Fatalf("%+v", m)
	}
	priced := MeterRun(12, old, []string{f}, nil, map[string]planusage.Price{"claude-x-9": {Input: 1e6}}, now)
	if priced.Unmeterable != "" || math.Abs(priced.Cost()-5) > 1e-9 {
		t.Fatalf("%+v", priced)
	}
	if got := UnmeterableProgram(12, "aider"); got != `the runner can't meter aider; start B-12 by hand with "af backlog start 12"` {
		t.Fatal(got)
	}
}

func TestMeterRunCountsAResumedTranscriptsCopiesOnce(t *testing.T) {
	project := filepath.Join(t.TempDir(), "projects", "-wt")
	first := filepath.Join(project, "a.jsonl")
	writeFile(t, first, transcriptLine("m1", "claude-opus-5-5", 1_000_000))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	run := RunRef{SessionID: "s1", StartedAt: now.Add(-time.Hour)}
	seen := map[string]int64{}
	m := MeterRun(3, run, []string{first}, seen, nil, now)
	run.Meter = m.Meter

	resumed := filepath.Join(project, "b.jsonl")
	writeFile(t, resumed, transcriptLine("m1", "claude-opus-5-5", 1_000_000), transcriptLine("m2", "claude-opus-5-5", 1_000_000))
	again := MeterRun(3, run, []string{first, resumed}, seen, nil, now)
	if math.Abs(again.Cost()-8) > 1e-9 {
		t.Fatalf("cost %v, want 8: the copy of m1 counted again", again.Cost())
	}
}
