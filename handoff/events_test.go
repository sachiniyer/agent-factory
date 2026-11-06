package handoff

// events_test.go pins the shadow log's file discipline and the edge
// tracker's dedup rules: day-rotated JSONL under the ledger's lock shape,
// seven-day pruning, control-stripped titles, and emit-on-change /
// re-announce-on-heartbeat semantics.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEventLogAppendWritesDayFileAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }

	rows := []EventRow{
		{TS: now, Kind: EventWall, SessionKey: "sess-1", Agent: "codex",
			Title: "line one\nline two\x07with-bell", From: "work",
			ReasonCode: "usage-reading", Params: map[string]string{"confirmed": "true"}, Mode: "shadow"},
		{TS: now.Add(time.Second), Kind: EventNoAccount, SessionKey: "sess-1",
			ReasonCode: "no-eligible-candidate", Mode: "shadow"},
	}
	if err := log.Append(rows); err != nil {
		t.Fatalf("Append: %v", err)
	}
	file := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), raw)
	}
	var first EventRow
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not JSON: %v", err)
	}
	if first.Kind != EventWall || first.ReasonCode != "usage-reading" || first.From != "work" {
		t.Fatalf("line 0 = %+v", first)
	}
	if strings.ContainsAny(first.Title, "\n\x07") {
		t.Fatalf("title carried control characters: %q", first.Title)
	}
	if first.Mode != "shadow" {
		t.Fatalf("mode = %q", first.Mode)
	}
}

func TestEventLogAppendGroupsByRowDay(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	before := time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second) // lands 2026-11-04
	log.now = func() time.Time { return after }
	if err := log.Append([]EventRow{
		{TS: before, Kind: EventWall, SessionKey: "s", ReasonCode: "wall"},
		{TS: after, Kind: EventWall, SessionKey: "s", ReasonCode: "wall"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, want := range []string{"auto-handoff-events-20261103.jsonl", "auto-handoff-events-20261104.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Fatalf("expected %s: %v", want, err)
		}
	}
}

func TestEventLogPrunesPastRetention(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	old := filepath.Join(dir, "auto-handoff-events-20261101.jsonl") // 9 days back
	recent := filepath.Join(dir, "auto-handoff-events-20261105.jsonl")
	other := filepath.Join(dir, "tasks.json")
	for _, f := range []string{old, recent, other} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old event file survived pruning")
	}
	for _, keep := range []string{recent, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("non-expired file %s was pruned: %v", keep, err)
		}
	}
}

// #6817 review: a symlinked day file is never written through — the log
// follows the AF-home rule, not whatever the link points at.
func TestEventLogAppendRefusesSymlinkedDayFile(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dayFile := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	if err := os.Symlink(target, dayFile); err != nil {
		t.Fatal(err)
	}
	err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}})
	if err == nil {
		t.Fatal("Append wrote through a symlinked day file")
	}
	raw, _ := os.ReadFile(target)
	if len(raw) != len("{}\n") {
		t.Fatalf("append reached the symlink target: %q", raw)
	}
}

// #6817 review: the sibling lock file prunes on the same stamp as the data
// file — otherwise stale locks accumulate past retention.
func TestEventLogPrunesStaleLockFiles(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	staleLock := filepath.Join(dir, "auto-handoff-events-20261101.jsonl.lock")
	if err := os.WriteFile(staleLock, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("stale lock file survived pruning")
	}
}

// #6817 review: a day file past the byte cap takes no more rows — bounded
// growth over a silently unbounded log.
func TestEventLogDayCapHoldsRows(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261103.jsonl")
	if err := os.WriteFile(dayFile, make([]byte, eventDayMaxBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	// Past the day cap, Append ERRORS rather than silently dropping — the
	// caller's edge stays uncommitted so the row retries next tick (#6817
	// round-2 review).
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err == nil {
		t.Fatalf("Append past the day cap returned nil error")
	}
	if info, err := os.Stat(dayFile); err != nil || info.Size() != eventDayMaxBytes {
		t.Fatalf("capped day file changed: size=%d err=%v", info.Size(), err)
	}
}

func TestEdgeTrackerEmitsOnChangeThenSuppresses(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "usage-reading", From: "work"}}
	move := []Event{{Kind: EventMove, Reason: "wall", From: "work", To: "personal"}}

	if got := tr.Filter("s", wall, now, ReannounceTest); len(got) != 1 {
		t.Fatalf("first wall verdict suppressed")
	}
	if got := tr.Filter("s", wall, now.Add(time.Minute), ReannounceTest); got != nil {
		t.Fatalf("unchanged verdict re-emitted inside the heartbeat")
	}
	if got := tr.Filter("s", move, now.Add(2*time.Minute), ReannounceTest); len(got) != 1 {
		t.Fatalf("changed verdict did not emit an edge")
	}
	// Same signature, past the heartbeat → the standing verdict re-announces.
	if got := tr.Filter("s", move, now.Add(2*time.Minute).Add(ReannounceTest+time.Second), ReannounceTest); len(got) != 1 {
		t.Fatalf("unchanged verdict did not re-announce after the heartbeat")
	}
}

func TestEdgeTrackerSilenceIsAnEdgeNotAnEvent(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "wall"}}

	tr.Filter("s", wall, now, time.Hour)
	// noop (no events) is a signature change → recorded, emits nothing.
	if got := tr.Filter("s", nil, now.Add(time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("silence emitted rows")
	}
	// The NEXT wall is a fresh edge even inside the heartbeat window.
	if got := tr.Filter("s", wall, now.Add(2*time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("wall after a noop transition did not re-emit")
	}
}

func TestEdgeTrackerPruneDropsGoneSessions(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wall := []Event{{Kind: EventWall, Reason: "wall"}}
	tr.Filter("gone", wall, now, time.Hour)
	tr.Prune(map[string]bool{"live": true})
	if got := tr.Filter("gone", wall, now.Add(time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("a session that left and returned did not re-announce")
	}
}

// TestEventSignatureExcludesVolatileParams pins the fix for the
// percent-jitter re-emission bug: the signature is verdict IDENTITY (kind,
// reason, from, to, stable params) and must NOT change when volatile
// evidence (percent, the observed window name, the move count) drifts
// between ticks for a semantically unchanged verdict (design R14).
func TestEventSignatureExcludesVolatileParams(t *testing.T) {
	t.Run("wall percent drift does not change signature", func(t *testing.T) {
		base := func(pct string) []Event {
			return []Event{{
				Kind: EventWall, Reason: "usage-reading", From: "work",
				Params: map[string]string{
					"confirmed": "true", "window": "primary", "percent": pct,
				},
			}}
		}
		sig1 := eventSignature(base("98"))
		sig2 := eventSignature(base("98.4"))
		if sig1 != sig2 {
			t.Fatalf("percent drift changed the signature:\n  %q\n  %q", sig1, sig2)
		}
		// The stable param (confirmed) still participates — a flip is a
		// real verdict change.
		if eventSignature([]Event{{
			Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{"confirmed": "false", "window": "primary", "percent": "98"},
		}}) == sig1 {
			t.Fatalf("confirmed flip did not change the signature")
		}
	})

	t.Run("wall window name drift does not change signature", func(t *testing.T) {
		e1 := []Event{{Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{"confirmed": "true", "window": "primary", "percent": "98"}}}
		e2 := []Event{{Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{"confirmed": "true", "window": "secondary", "percent": "99"}}}
		if eventSignature(e1) != eventSignature(e2) {
			t.Fatalf("window name drift changed the signature")
		}
	})

	t.Run("move percent drift does not change signature", func(t *testing.T) {
		base := func(pct string) []Event {
			return []Event{{
				Kind: EventMove, Reason: "wall", From: "work", To: "personal",
				Params: map[string]string{
					"trigger": "wall", "skipped": "backup=limited", "window": "primary", "percent": pct,
				},
			}}
		}
		if eventSignature(base("98")) != eventSignature(base("98.4")) {
			t.Fatalf("move percent drift changed the signature")
		}
		// The stable params (trigger, skipped) still participate.
		if eventSignature([]Event{{
			Kind: EventMove, Reason: "wall", From: "work", To: "personal",
			Params: map[string]string{"trigger": "idle", "skipped": "backup=limited", "percent": "98"},
		}}) == eventSignature(base("98")) {
			t.Fatalf("trigger change did not change the signature")
		}
	})

	t.Run("switch-loop move count drift does not change signature", func(t *testing.T) {
		base := func(moves string) []Event {
			return []Event{{
				Kind: EventSwitchLoop, Reason: "move-cap",
				Params: map[string]string{"moves": moves, "window": "30m"},
			}}
		}
		if eventSignature(base("3")) != eventSignature(base("4")) {
			t.Fatalf("move count drift changed the signature")
		}
	})
}

// TestEventSignatureIncludesStableParams ensures the stable params that
// define verdict identity still participate in the signature so two
// genuinely different verdicts are not collapsed into one.
func TestEventSignatureIncludesStableParams(t *testing.T) {
	t.Run("no-account skipped list change is a verdict change", func(t *testing.T) {
		e1 := []Event{{Kind: EventNoAccount, Reason: "no-eligible-candidate", From: "work",
			Params: map[string]string{"skipped": "backup=limited", "trigger": "wall"}}}
		e2 := []Event{{Kind: EventNoAccount, Reason: "no-eligible-candidate", From: "work",
			Params: map[string]string{"skipped": "backup=limited;spare=walled", "trigger": "wall"}}}
		if eventSignature(e1) == eventSignature(e2) {
			t.Fatalf("skipped list change did not change the signature")
		}
	})

	t.Run("halt reason change is a verdict change", func(t *testing.T) {
		e1 := []Event{{Kind: EventHalted, Reason: "mover-halted",
			Params: map[string]string{"halt_reason": "move-failed", "halted_at": "2026-11-03T00:00:00Z"}}}
		e2 := []Event{{Kind: EventHalted, Reason: "mover-halted",
			Params: map[string]string{"halt_reason": "carry-refused", "halted_at": "2026-11-03T00:00:00Z"}}}
		if eventSignature(e1) == eventSignature(e2) {
			t.Fatalf("halt_reason change did not change the signature")
		}
	})

	t.Run("halted_at change is a new episode", func(t *testing.T) {
		e1 := []Event{{Kind: EventHalted, Reason: "mover-halted",
			Params: map[string]string{"halt_reason": "move-failed", "halted_at": "2026-11-03T00:00:00Z"}}}
		e2 := []Event{{Kind: EventHalted, Reason: "mover-halted",
			Params: map[string]string{"halt_reason": "move-failed", "halted_at": "2026-11-03T01:00:00Z"}}}
		if eventSignature(e1) == eventSignature(e2) {
			t.Fatalf("halted_at change did not change the signature")
		}
	})

	t.Run("refusal backoff until change is a verdict change", func(t *testing.T) {
		e1 := []Event{{Kind: EventIgnored, Reason: "refusal-backoff",
			Params: map[string]string{"until": "2026-11-03T01:00:00Z"}}}
		e2 := []Event{{Kind: EventIgnored, Reason: "refusal-backoff",
			Params: map[string]string{"until": "2026-11-03T02:00:00Z"}}}
		if eventSignature(e1) == eventSignature(e2) {
			t.Fatalf("until change did not change the signature")
		}
	})
}

// TestEdgeTrackerSuppressesVolatileParamJitter is the end-to-end edge-tracker
// proof: a standing wall verdict whose only movement is the usage percent
// is suppressed inside the heartbeat, while a real verdict change (confirmed
// flip) still emits — and the emitted row carries the current percent.
func TestEdgeTrackerSuppressesVolatileParamJitter(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

	wallAt := func(pct string) []Event {
		return []Event{{
			Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{"confirmed": "true", "window": "primary", "percent": pct},
		}}
	}

	// Tick 1: initial verdict emits.
	got := tr.Filter("s", wallAt("98"), now, ReannounceTest)
	if len(got) != 1 || got[0].Params["percent"] != "98" {
		t.Fatalf("tick 1: expected one row with percent=98, got %v", got)
	}
	// Tick 2: only the percent drifted (98 → 98.4, exactly what codex
	// produces by completing another response). The verdict is
	// semantically unchanged → suppressed inside the heartbeat.
	if got := tr.Filter("s", wallAt("98.4"), now.Add(45*time.Second), ReannounceTest); got != nil {
		t.Fatalf("percent jitter re-emitted the unchanged verdict: %v", got)
	}
	// Tick 3: percent unchanged — control proving the suppression is not
	// a fluke of the value.
	if got := tr.Filter("s", wallAt("98.4"), now.Add(90*time.Second), ReannounceTest); got != nil {
		t.Fatalf("unchanged percent re-emitted: %v", got)
	}
}

// TestEdgeTrackerHeartbeatReannouncesWithCurrentPercent verifies the
// heartbeat re-announce fires past the interval and the emitted row still
// carries the current (drifted) percent — the operator sees fresh numbers
// on the heartbeat, not the stale ones from the first emission.
func TestEdgeTrackerHeartbeatReannouncesWithCurrentPercent(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

	wallAt := func(pct string) []Event {
		return []Event{{
			Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{"confirmed": "true", "window": "primary", "percent": pct},
		}}
	}

	// Initial emission.
	tr.Filter("s", wallAt("98"), now, ReannounceTest)
	// Percent drifts but the verdict is unchanged — suppressed inside.
	tr.Filter("s", wallAt("98.4"), now.Add(time.Minute), ReannounceTest)
	// Past the heartbeat — re-announces with the CURRENT percent.
	got := tr.Filter("s", wallAt("98.8"), now.Add(ReannounceTest+time.Second), ReannounceTest)
	if len(got) != 1 {
		t.Fatalf("heartbeat did not re-announce: got %v", got)
	}
	if got[0].Params["percent"] != "98.8" {
		t.Fatalf("heartbeat row carried stale percent %q, want 98.8", got[0].Params["percent"])
	}
}

// TestEdgeTrackerRealVerdictChangeEmitsWithVolatileParams verifies that a
// genuine verdict change (the confirmed boolean flips) emits even when the
// volatile percent also changed — the stable param drives the edge, not
// the evidence.
func TestEdgeTrackerRealVerdictChangeEmitsWithVolatileParams(t *testing.T) {
	tr := NewEdgeTracker()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

	confirmed := func(conf bool, pct string) []Event {
		return []Event{{
			Kind: EventWall, Reason: "usage-reading", From: "work",
			Params: map[string]string{
				"confirmed": strconv.FormatBool(conf), "window": "primary", "percent": pct,
			},
		}}
	}

	// Tick 1: unconfirmed wall emits.
	if got := tr.Filter("s", confirmed(false, "98"), now, ReannounceTest); len(got) != 1 {
		t.Fatalf("initial verdict suppressed: %v", got)
	}
	// Tick 2: percent drifted but confirmed still false — suppressed.
	if got := tr.Filter("s", confirmed(false, "98.4"), now.Add(time.Minute), ReannounceTest); got != nil {
		t.Fatalf("percent jitter re-emitted: %v", got)
	}
	// Tick 3: confirmed flips to true — a real verdict change, emits even
	// though the percent also changed.
	got := tr.Filter("s", confirmed(true, "98.8"), now.Add(2*time.Minute), ReannounceTest)
	if len(got) != 1 {
		t.Fatalf("confirmed flip did not emit: %v", got)
	}
	if got[0].Params["confirmed"] != "true" || got[0].Params["percent"] != "98.8" {
		t.Fatalf("emitted row missing verdict/evidence: %v", got[0].Params)
	}
}

// ReannounceTest mirrors the wiring's heartbeat constant for this leaf's
// internal tests (the exported one lives in the engine package).
const ReannounceTest = 30 * time.Minute

// #6817 review: a crash-torn tail must not corrupt the next row — an
// unterminated final line is dropped before the append, never concatenated.
func TestEventLogAppendRepairsUnterminatedTail(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261112.jsonl")
	good := `{"ts":"2026-11-12T07:59:00Z","kind":"wall","reason":"hit"}`
	if err := os.WriteFile(dayFile, []byte(good+"\n"+`{"ts":"2026-11-12T07:59`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall", Title: "next"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, err := os.ReadFile(dayFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("day file has %d lines, want 2: %q", len(lines), raw)
	}
	if lines[0] != good {
		t.Fatalf("good row mutated: %q", lines[0])
	}
	var decoded EventRow
	if err := json.Unmarshal([]byte(lines[1]), &decoded); err != nil {
		t.Fatalf("appended row is not valid JSON (tail corrupted it): %v", err)
	}
	if decoded.Title != "next" {
		t.Fatalf("appended row lost its fields: %+v", decoded)
	}
}

// A file that is nothing but a partial line truncates to empty rather than
// refusing every subsequent append.
func TestEventLogAppendTruncatesWholePartialFile(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return now }
	dayFile := filepath.Join(dir, "auto-handoff-events-20261112.jsonl")
	if err := os.WriteFile(dayFile, []byte(`{"ts":"partial, no newline`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]EventRow{{TS: now, Kind: EventWall, ReasonCode: "wall"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, _ := os.ReadFile(dayFile)
	if !strings.HasPrefix(string(raw), "{") || !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("file does not hold exactly one clean row: %q", raw)
	}
}
