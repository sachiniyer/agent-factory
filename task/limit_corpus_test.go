package task

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestClaudeResetCorpusReplay re-runs the wrapped-banner corpus the held
// #6952 attempt carried (task/limit_test.go + task/limit_wrap_test.go at its
// head) and pins each input's verdict under the continuation-indent rule
// (#6944). Two deliberate differences from that corpus's expectations:
//
//   - A continuation row is the banner's own only when it carries the
//     renderer's hanging indent — exactly the text column under the row's
//     "  ⎿  " gutter. The corpus's column-0 "continuation" rows are a
//     different row by layout (a transcript echo, a draft, a prompt), so a
//     clause that ran off its row DEGRADES — wall detected, reset unproven —
//     instead of being completed by whichever neighboring row happened to
//     follow it. That is the fix's whole point: several held-PR regressions
//     were exactly these inputs adopting a user row's clock or zone.
//   - Where the banner's own row already proves part of the clause (a bare
//     "reset at 2pm"), the parse keeps only what it can prove — the injected
//     clock's zone with no zone adopted from another row.
//
// The indented corpus shapes — the real renderer's layout — parse fully and
// live in limit_wrap_test.go beside the layout rule.
func TestClaudeResetCorpusReplay(t *testing.T) {
	loc := nyLoc(t)
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, loc)
	det := NewLimitDetector(nil)

	// Inputs the held corpus expected to parse by swallowing the next row.
	// Under the layout rule the banner's own row proves nothing more than it
	// shows: hit stands (the banner IS a wall), the reset does not.
	degraded := []string{
		// The clause's tail is on a column-0 row — a different row, however
		// wrap-shaped it looks.
		"Claude usage limit reached. Your limit will reset\nat 2pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2:30\npm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2:30 P\nM (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Sept.\n6, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov.\n6, 2026 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 1\n2, 2026 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 1\n1am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at No\nv 6, 2026 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at N\no\nvember 6, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 6\n2026 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 6, 20\n27 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2:\n30pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2\n:30pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 6\n9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2p\nm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 6\n, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at 2:3\n0pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 1\n2pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 1\n2:30pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset Th\nursday at 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset T\nh\nursday at 9am (America/New_York).",
		"Claude usage limit reached. Your limit will re\nset at 2pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset a\nt 2pm (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 12t\nh, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset Monday\nat 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 12\nth, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 12th\n, 2027 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov 6, 20\r\n27 9am (America/New_York).",
		"Claude usage limit reached. Your limit will reset at Nov.\x1b[39m\n6, 2027 9am (America/New_York).",
		"\x1b[38;5;211mClaude usage limit reached. Your limit will reset at Nov.\n6, 2026 9am (America/New_York).\x1b[39m",
		// The Codex cross-row counterexamples: a draft row's clock is never
		// the banner's.
		"Claude usage limit reached. Your limit will reset at some point soon.\nuser draft: meet Monday at 2pm",
		"Claude usage limit reached. Your limit will reset at some point soon.\r\nuser draft: meet Monday at 2pm",
		"Claude usage limit reached. Your limit will reset at some point soon.\x1b[39m\nuser draft: meet Monday at 2pm",
		"Claude usage limit reached. Your limit will reset sometime in Nov.\nuser draft: meet at 2pm",
		// Corpus fixtures that paired a 3-column "⎿  " gutter with a
		// 5-space continuation — the hanging indent is the gutter's own
		// text column, so this pairing is not the renderer's layout and the
		// deeper row is not the banner's continuation.
		"⎿  Claude usage limit reached. Your limit will re\n     set at 2pm (America/New_York).",
		"⎿  Claude usage limit reached. Your limit will reset Mon\n     day at 9am (America/New_York).",
		"⎿  Claude usage limit reached. Your limit will reset at Nov.\n     6, 2026 9am (America/New_York).",
		"⎿  Claude usage limit reached. Your limit will reset at 2p\n     m (America/New_York).",
	}
	for i, content := range degraded {
		hit, _, hasReset, evidence := det.Check(content, tmux.ProgramClaude, now)
		if !hit {
			t.Fatalf("degraded[%d]: the banner must still detect: hit=%v\n%s", i, hit, content)
		}
		if hasReset || evidence != "" {
			t.Fatalf("degraded[%d]: a clause cut at the row boundary must not borrow the next row: hasReset=%v evidence=%q\n%s", i, hasReset, evidence, content)
		}
	}

	// Inputs whose banner row proves a partial clause: the parse keeps only
	// what that row shows — the clock, or the clock and a row-local date —
	// resolved in the injected clock's zone. The neighboring row's zone,
	// date, weekday, or period is never adopted.
	est, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	rowBound := []struct {
		name     string
		content  string
		want     time.Time
		evidence string
	}{
		{
			name:     "col-0 paren row is another row",
			content:  "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York).",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm",
		},
		{
			name:     "col-0 paren row after a weekday clause",
			content:  "Claude usage limit reached. Your limit will reset Monday at 9am\n(America/New_York).",
			want:     time.Date(2026, 7, 6, 9, 0, 0, 0, loc).UTC(),
			evidence: "reset Monday at 9am",
		},
		{
			name:     "col-0 paren row after a dated clause",
			content:  "Claude usage limit reached. Your limit will reset at Nov 6, 2026 9am\n(America/New_York).",
			want:     time.Date(2026, 11, 6, 9, 0, 0, 0, loc).UTC(),
			evidence: "reset at Nov 6, 2026 9am",
		},
		{
			name:     "the canonical wrong-zone counterexample",
			content:  "Claude usage limit reached. Your limit will reset at 2pm\n(America/Los_Angeles)",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm",
		},
		{
			name:     "a lone period on the next row joins nothing",
			content:  "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)\n.",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, est).UTC(),
			evidence: "reset at 2pm (America/New_York)",
		},
		{
			name:     "paren then period each on their own col-0 row",
			content:  "Claude usage limit reached. Your limit will reset at 2pm\n(America/New_York)\n.",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm",
		},
		{
			name:     "an unclosed paren proves no zone",
			content:  "Claude usage limit reached. Your limit will reset at 2pm (America/\nNew_York).",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm (America/",
		},
		{
			name:     "guttered row, mismatched continuation indent",
			content:  "⎿  Claude usage limit reached. Your limit will reset at 2pm\n     (America/New_York).",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm",
		},
		{
			name:     "guttered row, mid-paren mismatched indent",
			content:  "⎿  Claude usage limit reached. Your limit will reset at 2pm (America/\n     New_York).",
			want:     time.Date(2026, 7, 4, 14, 0, 0, 0, loc).UTC(),
			evidence: "reset at 2pm (America/",
		},
	}
	for _, tc := range rowBound {
		t.Run(tc.name, func(t *testing.T) {
			hit, resetAt, hasReset, evidence := det.Check(tc.content, tmux.ProgramClaude, now)
			if !hit || !hasReset {
				t.Fatalf("hit=%v hasReset=%v; the row's own clause must still parse", hit, hasReset)
			}
			if !resetAt.Equal(tc.want) {
				t.Fatalf("resetAt = %v, want %v", resetAt.UTC(), tc.want)
			}
			if evidence != tc.evidence {
				t.Fatalf("evidence = %q, want %q", evidence, tc.evidence)
			}
		})
	}
}
