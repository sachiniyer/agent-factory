package planusage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assistant is one transcript line: a content block of message id with usage
// of in input and out output tokens.
func assistant(id, model string, in, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-20T10:01:00Z","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n", id, model, in, out)
}

const user = `{"type":"user","timestamp":"2026-10-20T10:00:00Z","message":{"role":"user","content":"hi"}}` + "\n"

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// opus 5.5: $4 input, $20 output per MTok.
const opus = "claude-opus-5-5"

func TestReadTranscriptDedupesByMessageID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, user,
		assistant("m1", opus, 1_000_000, 0), assistant("m1", opus, 1_000_000, 0), assistant("m1", opus, 1_000_000, 0),
		assistant("m2", opus, 0, 1_000_000))
	res, err := ReadTranscript(path, FileMark{}, ReadOptions{})
	if err != nil || !near(res.Cost(), 24) || len(res.Entries) != 2 || res.Mark.Last != messageKey("m2", "") {
		t.Fatalf("%+v %v", res, err)
	}
	info, _ := os.Stat(path)
	if res.Mark.Offset != info.Size() {
		t.Fatalf("offset %d of %d", res.Mark.Offset, info.Size())
	}
}

func TestReadTranscriptHalfWrittenLineAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	full := assistant("m2", opus, 0, 1_000_000)
	write(t, path, assistant("m1", opus, 1_000_000, 0), full[:20])
	res, err := ReadTranscript(path, FileMark{}, ReadOptions{})
	if err != nil || !near(res.Cost(), 4) {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Mark.Offset != int64(len(assistant("m1", opus, 1_000_000, 0))) {
		t.Fatalf("offset %d counts the half line", res.Mark.Offset)
	}
	appendTo(t, path, full[20:]+assistant("m2", opus, 0, 1_000_000))
	again, err := ReadTranscript(path, res.Mark, ReadOptions{})
	if err != nil || !near(again.Cost(), 20) || len(again.Entries) != 1 {
		t.Fatalf("%+v %v", again, err)
	}
	same, err := ReadTranscript(path, again.Mark, ReadOptions{})
	if err != nil || len(same.Entries) != 0 || same.Mark != again.Mark {
		t.Fatalf("a restart from the stored mark counted again: %+v %v", same, err)
	}
}

func TestReadTranscriptCountsAStraddlingMessageOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, assistant("m1", opus, 1_000_000, 0))
	res, _ := ReadTranscript(path, FileMark{}, ReadOptions{})
	appendTo(t, path, assistant("m1", opus, 1_000_000, 0))
	again, _ := ReadTranscript(path, res.Mark, ReadOptions{})
	if len(again.Entries) != 0 || again.Mark.Offset <= res.Mark.Offset {
		t.Fatalf("%+v", again)
	}
}

func TestReadTranscriptStopsAtUnpricedAndBadLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	first := assistant("m1", opus, 1_000_000, 0)
	write(t, path, first, assistant("m2", "claude-x-9", 1, 1), assistant("m3", opus, 1_000_000, 0))
	res, _ := ReadTranscript(path, FileMark{}, ReadOptions{})
	if res.Unpriced != "claude-x-9" || res.Mark.Offset != int64(len(first)) || !near(res.Cost(), 4) {
		t.Fatalf("%+v", res)
	}
	priced, _ := ReadTranscript(path, res.Mark, ReadOptions{Overrides: map[string]Price{"claude-x-9": {Input: 1e6, Output: 0}}})
	if priced.Unpriced != "" || !near(priced.Cost(), 1+4) {
		t.Fatalf("%+v", priced)
	}

	bad := filepath.Join(dir, "b.jsonl")
	write(t, bad, first, `{"type":"assistant","message":{"usage":`+"\n", first)
	stop, _ := ReadTranscript(bad, FileMark{}, ReadOptions{})
	if !stop.Bad || stop.Mark.Offset != int64(len(first)) {
		t.Fatalf("%+v", stop)
	}
	skip, _ := ReadTranscript(bad, FileMark{}, ReadOptions{SkipBad: true})
	if skip.Bad || len(skip.Entries) != 1 {
		t.Fatalf("%+v", skip)
	}
}

func TestReadTranscriptCachePricesAndRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	line := `{"type":"assistant","timestamp":"2026-10-20T10:01:00Z","message":{"id":"c","model":"claude-opus-5-5","usage":{"input_tokens":0,"cache_creation_input_tokens":3000000,"cache_read_input_tokens":1000000,"output_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":1000000,"ephemeral_1h_input_tokens":2000000}}}}` + "\n"
	write(t, path, line)
	res, _ := ReadTranscript(path, FileMark{}, ReadOptions{})
	if want := 5.0 + 2*8 + 0.2; !near(res.Cost(), want) {
		t.Fatalf("cost %v, want %v", res.Cost(), want)
	}
	write(t, path, assistant("n", opus, 1_000_000, 0))
	again, _ := ReadTranscript(path, FileMark{Offset: 1 << 20, Last: messageKey("c", "")}, ReadOptions{})
	if !near(again.Cost(), 4) {
		t.Fatalf("a rewritten file was not read from the start: %+v", again)
	}
}

func TestTranscriptFilesIncludesSubagents(t *testing.T) {
	project := filepath.Join(t.TempDir(), "projects", "-wt")
	write(t, filepath.Join(project, "s1.jsonl"), user)
	write(t, filepath.Join(project, "s1", "subagents", "agent-a.jsonl"), user)
	write(t, filepath.Join(project, "s1", "tool-results", "x.jsonl"), user)
	got := TranscriptFiles([]string{project})
	if len(got) != 2 || !strings.HasSuffix(got[0], "agent-a.jsonl") && !strings.HasSuffix(got[1], "agent-a.jsonl") {
		t.Fatal(got)
	}
}

func TestReadTranscriptSkipsUnpricedWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, assistant("m1", "claude-x-9", 1, 1), assistant("m2", "claude-x-9", 1, 1), assistant("m3", opus, 1_000_000, 0))
	res, _ := ReadTranscript(path, FileMark{}, ReadOptions{SkipUnpriced: true})
	info, _ := os.Stat(path)
	if res.Unpriced != "" || len(res.Skipped) != 1 || res.Skipped[0] != "claude-x-9" || !near(res.Cost(), 4) || res.Mark.Offset != info.Size() {
		t.Fatalf("%+v", res)
	}
}

func TestReadTranscriptSeenCountsCopiedMessagesOnce(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.jsonl")
	write(t, first, assistant("m1", opus, 1_000_000, 0))
	seen := map[string]int64{}
	if res, _ := ReadTranscript(first, FileMark{}, ReadOptions{Seen: seen}); !near(res.Cost(), 4) {
		t.Fatalf("%+v", res)
	}
	resumed := filepath.Join(dir, "b.jsonl")
	write(t, resumed, assistant("m1", opus, 1_000_000, 0), assistant("m2", opus, 0, 1_000_000))
	if res, _ := ReadTranscript(resumed, FileMark{}, ReadOptions{Seen: seen}); !near(res.Cost(), 20) {
		t.Fatalf("a resumed transcript counted its copied message again: %+v", res)
	}
	if len(seen) != 2 {
		t.Fatalf("%v", seen)
	}

	write(t, first, assistant("m1", opus, 1_000_000, 0))
	if res, _ := ReadTranscript(first, FileMark{Offset: 1 << 20}, ReadOptions{Seen: seen}); res.Cost() != 0 || res.Mark.Offset == 0 {
		t.Fatalf("a rewritten file counted again: %+v", res)
	}
}

func TestPruneSeen(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	seen := map[string]int64{"old": now.Add(-9 * 24 * time.Hour).Unix(), "new": now.Unix()}
	if !PruneSeen(seen, now) || len(seen) != 1 || PruneSeen(seen, now) {
		t.Fatal(seen)
	}
}

// The meter reads only regular transcript files: a symlink in a project
// directory is neither listed nor opened, so it cannot be used to make af read
// some other file.
func TestMeterNeverFollowsASymlinkedTranscript(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "secret.jsonl")
	line := `{"type":"assistant","timestamp":"2026-10-30T00:00:00Z","message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"
	if err := os.WriteFile(other, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptFiles([]string{dir}); len(got) != 0 {
		t.Fatalf("a symlinked transcript was listed: %v", got)
	}
	if _, err := ReadTranscript(link, FileMark{}, ReadOptions{}); err == nil {
		t.Fatal("a symlinked transcript was opened")
	}
	real := filepath.Join(dir, "real.jsonl")
	if err := os.WriteFile(real, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptFiles([]string{dir}); len(got) != 1 || got[0] != real {
		t.Fatalf("files = %v", got)
	}
}
