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

	m := MeterRun(3, Where{}, run, files, map[string]int64{}, nil, now)
	if !m.Changed || m.Unmeterable != "" || math.Abs(m.Cost()-6) > 1e-9 || len(m.Meter) != 2 || len(m.Entries) != 2 {
		t.Fatalf("%+v", m)
	}
	run.Meter = m.Meter
	again := MeterRun(3, Where{}, run, files, map[string]int64{}, nil, now)
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
	if m := MeterRun(12, Where{}, young, nil, nil, nil, now); m.Unmeterable != "" {
		t.Fatalf("a young run with no files: %q", m.Unmeterable)
	}
	old := RunRef{StartedAt: now.Add(-3 * time.Minute)}
	if m := MeterRun(12, Where{}, old, nil, nil, nil, now); m.Unmeterable != `af cannot find the transcripts of the session of B-12 · mark B-12 done or drop it · see "af backlog show 12"` {
		t.Fatalf("%q", m.Unmeterable)
	}
	// The runner shows the reason in every project, so it names the item's own.
	there := Where{Name: "detail", Root: "/src/detail"}
	if m := MeterRun(12, there, old, nil, nil, nil, now); m.Unmeterable != `af cannot find the transcripts of the session of B-12 in project detail · mark B-12 done or drop it · see "af backlog show 12 --repo /src/detail"` {
		t.Fatalf("%q", m.Unmeterable)
	}
	f := filepath.Join(project, "s.jsonl")
	writeFile(t, f, transcriptLine("m1", "claude-x-9", 5))
	m := MeterRun(12, Where{}, old, []string{f}, nil, nil, now)
	if m.Unmeterable != `no price for model claude-x-9; add [backlog.prices."claude-x-9"] to config.toml` || m.Cost() != 0 {
		t.Fatalf("%+v", m)
	}
	priced := MeterRun(12, Where{}, old, []string{f}, nil, map[string]planusage.Price{"claude-x-9": {Input: 1e6}}, now)
	if priced.Unmeterable != "" || math.Abs(priced.Cost()-5) > 1e-9 {
		t.Fatalf("%+v", priced)
	}
	if got := UnmeterableProgram(12, there, "aider"); got != `the runner can't meter aider, the program of B-12 in project detail · mark B-12 done or drop it · see "af backlog show 12 --repo /src/detail"` {
		t.Fatal(got)
	}
}

func TestWhere(t *testing.T) {
	if got := (Where{}).Item(3); got != "B-3" {
		t.Fatal(got)
	}
	if got := (Where{}).Fix(3); got != `mark B-3 done or drop it · see "af backlog show 3"` {
		t.Fatal(got)
	}
	if got := (Where{Name: "detail"}).Item(3); got != "B-3 in project detail" {
		t.Fatal(got)
	}
}

func TestWhereFixQuotesTheRepoPath(t *testing.T) {
	cases := map[string]string{
		"/src/sahara":     `/src/sahara`,
		"/src/my project": `'/src/my project'`,
		"/src/it's$(x)":   `'/src/it'\''s$(x)'`,
		"/src/a;rm -rf /": `'/src/a;rm -rf /'`,
	}
	for root, want := range cases {
		got := (Where{Root: root}).Fix(7)
		if wantFix := `mark B-7 done or drop it · see "af backlog show 7 --repo ` + want + `"`; got != wantFix {
			t.Errorf("%s: got %s, want %s", root, got, wantFix)
		}
	}
}

func TestRunBlocksRunner(t *testing.T) {
	end := time.Date(2026, 10, 23, 15, 17, 0, 0, time.UTC)
	cases := []struct {
		name     string
		status   Status
		archived bool
		ended    time.Time
		churn    time.Time
		want     bool
	}{
		{"running and live", StatusRunning, false, time.Time{}, time.Time{}, true},
		{"archived", StatusRunning, true, time.Time{}, time.Time{}, false},
		{"ended, waiting on the user", StatusRunning, false, end, end.Add(-time.Second), false},
		{"ended, then worked in", StatusRunning, false, end, end.Add(time.Second), true},
		{"ended then re-armed by a new run", StatusRunning, false, time.Time{}, end, true},
		{"review", StatusReview, false, time.Time{}, time.Time{}, false},
		{"archived after being worked in", StatusRunning, true, end, end.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := RunBlocksRunner(c.status, c.archived, c.ended, c.churn); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMeterRunCountsAResumedTranscriptsCopiesOnce(t *testing.T) {
	project := filepath.Join(t.TempDir(), "projects", "-wt")
	first := filepath.Join(project, "a.jsonl")
	writeFile(t, first, transcriptLine("m1", "claude-opus-5-5", 1_000_000))
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	run := RunRef{SessionID: "s1", StartedAt: now.Add(-time.Hour)}
	seen := map[string]int64{}
	m := MeterRun(3, Where{}, run, []string{first}, seen, nil, now)
	run.Meter = m.Meter

	resumed := filepath.Join(project, "b.jsonl")
	writeFile(t, resumed, transcriptLine("m1", "claude-opus-5-5", 1_000_000), transcriptLine("m2", "claude-opus-5-5", 1_000_000))
	again := MeterRun(3, Where{}, run, []string{first, resumed}, seen, nil, now)
	if math.Abs(again.Cost()-8) > 1e-9 {
		t.Fatalf("cost %v, want 8: the copy of m1 counted again", again.Cost())
	}
}

func TestMeterOrchestratorCountsFromBind(t *testing.T) {
	f := filepath.Join(t.TempDir(), "projects", "-repo", "c1.jsonl")
	early := strings.Replace(transcriptLine("m0", "claude-opus-5-5", 1_000_000), "10:00:00", "09:00:00", 1)
	writeFile(t, f, early, transcriptLine("m1", "claude-opus-5-5", 1_000_000))
	o := Orchestrator{SessionID: "o", Conversations: []OrchConversation{{Main: f, From: time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)}}}
	m := MeterOrchestrator(1, Where{}, o, map[string]int64{}, nil)
	if !m.Changed || len(m.Entries) != 1 || math.Abs(m.Cost()-4) > 1e-9 {
		t.Fatalf("%+v", m)
	}
	if m := MeterOrchestrator(1, Where{}, Orchestrator{SessionID: "o"}, nil, nil); m.Changed {
		t.Fatalf("no conversation yet: %+v", m)
	}
}

// Each conversation counts only its own span, its subagents included: a
// subagent first seen after the group was bound to another session counts from
// its conversation's bind, not the new one (Codex P2 on #6616), and what a
// replaced session wrote after the replacement is not the group's.
func TestMeterOrchestratorCountsEachConversationInItsSpan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects", "-repo")
	at := func(id, hhmm string) string {
		return strings.Replace(transcriptLine(id, "claude-opus-5-5", 1_000_000), "10:00:00", hhmm+":00", 1)
	}
	old, cur := filepath.Join(dir, "c1.jsonl"), filepath.Join(dir, "c2.jsonl")
	day := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, time.UTC) }
	writeFile(t, old, at("a1", "09:30"), at("a2", "11:00"))
	writeFile(t, filepath.Join(dir, "c1", "subagents", "agent-x.jsonl"), at("a3", "10:00"))
	writeFile(t, cur, at("b1", "11:00"))
	o := Orchestrator{SessionID: "o2", BoundAt: day(10, 30), Conversations: []OrchConversation{
		{Main: old, From: day(9, 0), Until: day(10, 30)}, {Main: cur, From: day(10, 30)}}}
	m := MeterOrchestrator(1, Where{}, o, map[string]int64{}, nil)
	// a1 and the subagent's a3 are c1's while it orchestrated, b1 is c2's; a2,
	// written by c1's session after the replacement, is not the group's.
	if len(m.Entries) != 3 || math.Abs(m.Cost()-12) > 1e-9 {
		t.Fatalf("%+v", m)
	}
}
