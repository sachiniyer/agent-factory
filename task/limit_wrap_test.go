package task

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The pane-side half of #6944: when the banner's reset clause wraps, only the
// banner's own continuation rows may extend it, and they are identified by
// LAYOUT — Claude's renderer wraps a message line into rows that carry the
// continuation indent beneath the "  ⎿  " gutter. A row without that indent
// is a different row (a transcript echo, a user draft, another message) and
// ends the clause: its clock, weekday, date, or timezone must never supply
// the reset. tmux captures with -J, which already joins tmux's own
// grid-level hard wraps, so the only multi-row banner shapes left in a
// capture are the renderer's own indented word-wraps.
//
// The authoritative reset for claude sessions is the transcript's
// usage_limit_reached record (session.ClaudeActiveLimitReset); this pane
// parse is the trigger and the fallback, and the property it must keep is
// that a wrapped or reworded banner degrades to "wall, unknown reset" rather
// than guessing a reset from a neighboring row.

// TestClaudeResetWrappedBanner pins the layout rule: a continuation row is
// joined only when it carries the banner row's text-column indent — the
// renderer's hanging indent under the "  ⎿  " gutter — and every boundary
// rejoins with a space EXCEPT the one hard-split a word-wrap renderer emits:
// the row feeding the boundary is a single token at the full wrap width — a
// mid-token fragment, since a complete banner word is never wider than the
// region it wraps to.
func TestClaudeResetWrappedBanner(t *testing.T) {
	est, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) // 05:00 America/New_York
	want2pm := time.Date(2026, 1, 2, 14, 0, 0, 0, est).UTC()

	cases := []struct {
		name    string
		content string
		want    time.Time
		wantEv  string // whitespace-normalized evidence when a reset parses
	}{
		{
			name:    "unwrapped control",
			content: "  ⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York).",
			want:    want2pm,
			wantEv:  "reset at 2pm (America/New_York).",
		},
		{
			name:    "tz paren wrapped onto the indented continuation row",
			content: "  ⎿  Claude usage limit reached. Your limit will reset at 2pm\n     (America/New_York).",
			want:    want2pm,
			wantEv:  "reset at 2pm (America/New_York).",
		},
		{
			name:    "clause wrapped between reset and at",
			content: "  ⎿  Claude usage limit reached. Your limit will reset\n     at 2pm (America/New_York).",
			want:    want2pm,
			wantEv:  "reset at 2pm (America/New_York).",
		},
		{
			name:    "weekly weekday wrapped mid-clause",
			content: "  ⎿  Claude usage limit reached. Your limit will reset Mon\n     day at 9am (America/New_York).",
			want:    time.Date(2026, 1, 5, 9, 0, 0, 0, est).UTC(),
			wantEv:  "reset Mon day at 9am (America/New_York).",
		},
		{
			// A self-consistent very narrow render: every row fits the
			// wrap width (13 text columns), and the parenthesized zone —
			// the one token in the clause that exceeds it — splits hard
			// mid-token after filling its row. The split row is full and
			// the rejoined token is wider than the row, so the join drops
			// the break rather than spacing it.
			name: "tz token hard-splits inside the paren on a very narrow pane",
			content: "  ⎿  Claude usage\n" +
				"     limit\n" +
				"     reached. Your\n" +
				"     limit will\n" +
				"     reset at 2pm\n" +
				"     (America/New_\n" +
				"     York).",
			want:   want2pm,
			wantEv: "reset at 2pm (America/New_York).",
		},
		{
			// The banner's own name split across continuation rows: detection
			// itself must span the wraps before the clause is ever reached.
			name: "banner name split across continuation rows still detects",
			content: "  ⎿  Claude usage\n" +
				"     limit reached.\n" +
				"     Your limit will\n" +
				"     reset at 2pm\n" +
				"     (America/New_York).",
			want:   want2pm,
			wantEv: "reset at 2pm (America/New_York).",
		},
		{
			name:    "styled capture: SGR escapes on banner and continuation rows",
			content: "\x1b[38;5;211m  ⎿  Claude usage limit reached. Your limit will reset at 2pm\n\x1b[38;5;211m     (America/New_York).\x1b[39m",
			want:    want2pm,
			wantEv:  "reset at 2pm (America/New_York).",
		},
		{
			name:    "weekly date wrapped after the month abbreviation's period",
			content: "  ⎿  Claude usage limit reached. Your limit will reset at Nov.\n     6, 2026 9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(),
			wantEv:  "reset at Nov. 6, 2026 9am (America/New_York).",
		},
		{
			name:    "weekly date wrapped between the day and the clock",
			content: "  ⎿  Claude usage limit reached. Your limit will reset at Nov 6\n     9am (America/New_York).",
			want:    time.Date(2026, 11, 6, 9, 0, 0, 0, est).UTC(),
			wantEv:  "reset at Nov 6 9am (America/New_York).",
		},
		{
			name:    "new-style banner: You've hit your 5-hour limit",
			content: "  ⎿  You've hit your 5-hour limit · resets 9:40am (America/New_York)",
			want:    time.Date(2026, 1, 2, 9, 40, 0, 0, est).UTC(),
			wantEv:  "resets 9:40am (America/New_York)",
		},
		{
			name:    "new-style banner wrapped at the resets clause",
			content: "  ⎿  You've hit your 5-hour limit · resets\n     9:40am (America/New_York)",
			want:    time.Date(2026, 1, 2, 9, 40, 0, 0, est).UTC(),
			wantEv:  "resets 9:40am (America/New_York)",
		},
		{
			name:    "new-style weekly banner",
			content: "  ⎿  You've hit your weekly limit · resets 10am (America/New_York)",
			want:    time.Date(2026, 1, 2, 10, 0, 0, 0, est).UTC(),
			wantEv:  "resets 10am (America/New_York)",
		},
		{
			// Codex's ten-column counterexample: every boundary here is a
			// word-wrap at a space — a full row ending at a word whose
			// concatenation with the next word exceeds the width is not a
			// hard-split. "reset|Monday" must join WITH its space or the
			// fused "resetMonday" loses the whole clause.
			name: "ten-column word wrap keeps every word boundary",
			content: "  ⎿  Claude\n" +
				"     usage\n" +
				"     limit\n" +
				"     reached.\n" +
				"     Your limit\n" +
				"     will reset\n" +
				"     Monday at\n" +
				"     9am",
			want:   time.Date(2026, 1, 5, 9, 0, 0, 0, now.Location()).UTC(), // Mon 9am host zone
			wantEv: "reset Monday at 9am",
		},
	}
	det := NewLimitDetector(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, resetAt, hasReset, evidence := det.Check(tc.content, tmux.ProgramClaude, now)
			if !hit || !hasReset {
				t.Fatalf("hit=%v hasReset=%v; the banner must still detect and parse", hit, hasReset)
			}
			if !resetAt.Equal(tc.want) {
				t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), tc.want)
			}
			if evidence != tc.wantEv {
				t.Fatalf("evidence = %q, want %q", evidence, tc.wantEv)
			}
		})
	}
}

// TestClaudeResetClauseEndsAtContinuationIndent pins the property that makes
// the fallback safe: a row that does NOT carry the banner's continuation
// indent is a different row — a transcript echo, a user draft, the prompt —
// and ends the clause. Whatever clock, weekday, date, or timezone that row
// carries must never supply the reset. These are the #6944 Codex
// counterexamples: on the held normalizer each of them let a neighboring row
// feed the parse.
func TestClaudeResetClauseEndsAtContinuationIndent(t *testing.T) {
	est, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	twoPmHere := time.Date(2026, 1, 2, 14, 0, 0, 0, now.Location()).UTC()
	sevenPmHere := time.Date(2026, 1, 2, 19, 0, 0, 0, now.Location()).UTC()
	twoPmNY := time.Date(2026, 1, 2, 14, 0, 0, 0, est).UTC()

	cases := []struct {
		name      string
		content   string
		wantReset *time.Time // nil when the clause cannot prove a reset
		wantEv    string
	}{
		{
			// The canonical counterexample: an unindented "(America/Los_Angeles)"
			// row is a different row, never the wrapped paren — the clock the
			// banner's own row proves resolves in the injected clock's zone,
			// and the other row's zone is not adopted.
			name:      "next-row paren without the indent is another row",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at 2pm\n(America/Los_Angeles).",
			wantReset: &twoPmHere,
			wantEv:    "reset at 2pm",
		},
		{
			name:      "unparseable clause then a draft row with a clock",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at some point soon.\nuser draft: meet Monday at 2pm",
			wantReset: nil,
		},
		{
			name:      "unparseable clause then a draft row with a clock (CRLF)",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at some point soon.\r\nuser draft: meet Monday at 2pm",
			wantReset: nil,
		},
		{
			// A clause that ends mid-token does not guess the continuation:
			// "reset" alone cannot prove a clock — the row below is a
			// different row even though the text looks like a wrap.
			name:      "clause cut inside the reset keyword ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York).",
			wantReset: nil,
		},
		{
			name:      "clause cut inside the clock token ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at 2p\nm (America/New_York).",
			wantReset: nil,
		},
		{
			name:      "clause cut inside a date ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at Nov 1\n2, 2026 9am (America/New_York).",
			wantReset: nil,
		},
		{
			name:      "clause cut before the clock ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at Nov 6\n2026 9am (America/New_York).",
			wantReset: nil,
		},
		{
			name:      "clause cut inside the keyword ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will re\nset at 2pm (America/New_York).",
			wantReset: nil,
		},
		{
			name:      "clause cut inside a weekday ends there",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset Monday\nat 9am (America/New_York).",
			wantReset: nil,
		},
		{
			// An indented row AFTER the clause's own sentence end cannot
			// reach back into it: the period closed the clause before the
			// next row's clock, weekday, or zone is ever read.
			name:      "indented row after the clause's period supplies nothing",
			content:   "  ⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York).\n     meet Monday at 3pm (America/Los_Angeles)",
			wantReset: &twoPmNY,
			wantEv:    "reset at 2pm (America/New_York).",
		},
		{
			// The last banner wins, and a transcript row after it cannot
			// move its reset.
			name: "later transcript row cannot supply tz to the last banner",
			content: "  ⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York).\n" +
				"  ⎿  Claude usage limit reached. Your limit will reset at 7pm\n" +
				"(America/Los_Angeles)",
			wantReset: &sevenPmHere,
			wantEv:    "reset at 7pm",
		},
	}

	det := NewLimitDetector(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, resetAt, hasReset, evidence := det.Check(tc.content, tmux.ProgramClaude, now)
			if !hit {
				t.Fatalf("the banner must still be detected: hit=%v", hit)
			}
			if tc.wantReset == nil {
				if hasReset {
					t.Fatalf("a clause that ends at the indent boundary must not borrow the next row: hasReset=%v resetAt=%v evidence=%q", hasReset, resetAt, evidence)
				}
				if evidence != "" {
					t.Fatalf("an unproven reset must carry no evidence, got %q", evidence)
				}
				return
			}
			if !hasReset {
				t.Fatalf("expected the banner row's own clause to parse")
			}
			if !resetAt.Equal(*tc.wantReset) {
				t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), *tc.wantReset)
			}
			if evidence != tc.wantEv {
				t.Fatalf("evidence = %q, want %q", evidence, tc.wantEv)
			}
		})
	}
}

// TestClaudeResetWrappedBannerEvidenceStability pins the #6765 re-read
// identity across a pane resize: the unwrapped and wrapped renderings of the
// same banner produce the same whitespace-normalized evidence phrase, so the
// same-wall dedupe keeps the first parse rather than minting a new
// observation when the pane is resized between reads.
func TestClaudeResetWrappedBannerEvidenceStability(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	unwrapped := "  ⎿  Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	wrapped := "  ⎿  Claude usage limit reached. Your limit will reset at 2pm\n     (America/New_York)."
	_, _, _, evUnwrapped := det.Check(unwrapped, tmux.ProgramClaude, now)
	_, _, _, evWrapped := det.Check(wrapped, tmux.ProgramClaude, now)
	if evUnwrapped == "" || evUnwrapped != evWrapped {
		t.Fatalf("a word-wrap must not change the evidence: unwrapped=%q wrapped=%q", evUnwrapped, evWrapped)
	}
	if strings.Contains(evWrapped, "\n") {
		t.Fatalf("evidence must be whitespace-normalized: %q", evWrapped)
	}

	// Re-reads of the unchanged wrapped banner at different clocks reproduce
	// the same phrase (identity) even though the time-only resolution may
	// drift — the same contract #6765 pins for unwrapped banners.
	_, _, _, ev1 := det.Check(wrapped, tmux.ProgramClaude, now)
	_, _, _, ev2 := det.Check(wrapped, tmux.ProgramClaude, now.Add(time.Hour))
	if ev1 != ev2 {
		t.Fatalf("an unchanged wrapped banner must reproduce identical evidence: %q != %q", ev1, ev2)
	}
}
