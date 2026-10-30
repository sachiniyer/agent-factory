package task

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestClaudeResetParenTimezone(t *testing.T) {
	resetTimezoneWarningsForTest(t)
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, loc)
	cases := []struct {
		zones []string
		want  string
		warn  bool
	}{
		{[]string{"UTC"}, "2026-09-07T17:00:00Z", false},
		{[]string{"GMT"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Zulu"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Japan"}, "2026-09-08T08:00:00Z", false},
		{[]string{"Israel"}, "2026-09-08T14:00:00Z", false},
		{[]string{"Singapore"}, "2026-09-08T09:00:00Z", false},
		{[]string{"Turkey"}, "2026-09-08T14:00:00Z", false},
		{[]string{"EST"}, "2026-09-07T22:00:00Z", false},
		{[]string{"America/New_York"}, "2026-09-07T21:00:00Z", false},
		{[]string{"Etc/UTC"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Etc/GMT+5"}, "2026-09-07T22:00:00Z", false},
		{[]string{"Etc/GMT-9"}, "2026-09-08T08:00:00Z", false},
		{[]string{"XYZ"}, "2026-09-07T21:00:00Z", true},
		{[]string{"weekly"}, "2026-09-07T21:00:00Z", true},
		{[]string{"5h limit"}, "2026-09-07T21:00:00Z", true},
		{[]string{"Pro", "America/Los_Angeles"}, "2026-09-08T00:00:00Z", false},
		{[]string{"UTC", "extra"}, "2026-09-07T17:00:00Z", false},
		{[]string{"Pro", "Team"}, "2026-09-07T21:00:00Z", true},
		{nil, "2026-09-07T21:00:00Z", false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.zones, ","), func(t *testing.T) {
			var warnings bytes.Buffer
			previous := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
			banner := "Claude usage limit reached. Your limit will reset at 5pm"
			for _, zone := range tc.zones {
				banner += " (" + zone + ")"
			}
			hit, reset, parsed := NewLimitDetector(nil).Check(banner, tmux.ProgramClaude, now)
			if !hit || !parsed || reset.Format(time.RFC3339) != tc.want {
				t.Errorf("hit=%v parsed=%v reset=%s; want %s", hit, parsed, reset.Format(time.RFC3339), tc.want)
			}
			if tc.warn {
				if strings.Count(warnings.String(), "\n") != 1 || !strings.Contains(warnings.String(), banner) || !strings.Contains(warnings.String(), loc.String()) {
					t.Errorf("want one warning naming banner and fallback zone; got %q", warnings.String())
				}
				if !strings.Contains(warnings.String(), fmt.Sprintf("timezone candidates %q", tc.zones)) {
					t.Errorf("warning must list all rejected candidates: %s", &warnings)
				}
			} else if warnings.Len() != 0 {
				t.Errorf("unexpected warning: %s", &warnings)
			}
		})
	}
}

// TestClaudeResetTimezoneWeekdayCollision pins the fix for the timezone-vs-weekday
// collision in parseClaudeReset: the weekday regex used to scan the ENTIRE reset
// tail, including the parenthesized IANA zone, so a tz whose name begins with a
// weekday abbreviation (America/Montreal, Europe/Monaco, America/Thule, …) — the
// parentheses' "/" supplies the word boundary — hijacked the weekly "reset
// <weekday>" branch and parked a same-day 5h limit until next Monday/Thursday.
//
// now is a Tuesday so a Monday/Thursday weekly result lands a full week/2 days
// later, making the divergence from the time-only result (today) unmistakable.
// The clock (5pm) is strictly after now in every tested zone, so the time-only
// path resolves to TODAY (Tuesday) in each — the proof the weekly branch was NOT
// taken. The weekly/calendar positive controls below (with the SAME colliding
// zones) confirm the named-day and calendar-date variants still parse when a
// real weekday/month is present in the clause, i.e. the paren strip is surgical.
func TestClaudeResetTimezoneWeekdayCollision(t *testing.T) {
	resetTimezoneWarningsForTest(t)
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Tuesday 2026-09-08 10:00 America/New_York (14:00 UTC): the Monday/Thursday
	// weekly branch would land on 2026-09-14 / 2026-09-10, the time-only branch on
	// 2026-09-08 (today, since 5pm has not yet passed in any tested zone).
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, loc)
	if now.Weekday() != time.Tuesday {
		t.Fatalf("fixture clock must be Tuesday; got %s", now.Weekday())
	}

	// Collision zones: the paren tz name begins with a weekday abbreviation, so
	// the buggy whole-tail scan matched it as the weekly day. Each now resolves
	// via the time-only path (nextTimeOfDay), so resetAt is TODAY (Tuesday) at
	// 5pm in the zone's own offset — never next Monday/Thursday. wantUTC is the
	// exact instant Go's nextTimeOfDay produces for that zone, computed once and
	// pinned.
	collision := []struct {
		zone    string
		wantUTC string
	}{
		{"America/Montreal", "2026-09-08T21:00:00Z"},   // Mon-treon -> EDT (link to Toronto)
		{"America/Moncton", "2026-09-08T20:00:00Z"},    // Mon-cton -> ADT
		{"America/Monterrey", "2026-09-08T23:00:00Z"},  // Mon-terrey -> CST
		{"America/Montevideo", "2026-09-08T20:00:00Z"}, // Mon-tevideo -> -03
		{"Europe/Monaco", "2026-09-08T15:00:00Z"},      // Mon-aco -> CEST
		{"Africa/Monrovia", "2026-09-08T17:00:00Z"},    // Mon-rovia -> GMT
		{"America/Thule", "2026-09-08T20:00:00Z"},      // Th-u-le -> ADT (Thursday prefix)
	}
	for _, tc := range collision {
		t.Run("time_only/"+tc.zone, func(t *testing.T) {
			var warnings bytes.Buffer
			previous := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
			banner := "Claude usage limit reached. Your limit will reset at 5pm (" + tc.zone + ")"
			hit, reset, parsed := NewLimitDetector(nil).Check(banner, tmux.ProgramClaude, now)
			if !hit || !parsed {
				t.Fatalf("hit=%v parsed=%v reset=%s", hit, parsed, reset)
			}
			if got := reset.Format(time.RFC3339); got != tc.wantUTC {
				t.Fatalf("zone %s: resetAt=%s, want %s (time-only path; weekly branch misparsed the tz name)", tc.zone, got, tc.wantUTC)
			}
			// The time-only path lands TODAY (Tuesday) at 5pm in the zone — the
			// weekly bug would have returned Monday 2026-09-14 or Thursday
			// 2026-09-10. Assert the weekday to make the regression intent explicit.
			if w := reset.In(time.UTC).Weekday(); w != time.Tuesday {
				t.Fatalf("zone %s: resetAt weekday=%s, want Tuesday (time-only today); the weekly branch leaked through", tc.zone, w)
			}
			if !reset.After(now) {
				t.Fatalf("zone %s: resetAt %s not after now %s", tc.zone, reset, now)
			}
			if warnings.Len() != 0 {
				t.Fatalf("valid colliding zone must not warn: %s", &warnings)
			}
		})
	}

	// Positive controls: a REAL weekday or calendar date in the clause, with a
	// colliding zone in parens, must STILL take the weekly / calendar branch.
	// These pass on the buggy tree too (the named day matches before the tz in
	// the whole-tail scan) — they pin that the paren strip did not silent the
	// named-day or calendar-date variants.
	positive := []struct {
		name    string
		tail    string
		wantUTC string
		weekday time.Weekday
	}{
		{
			name:    "weekly Monday with colliding Mon-tz",
			tail:    "Monday at 9am (America/Montreal)",
			wantUTC: "2026-09-14T13:00:00Z", // next Mon 9am EDT
			weekday: time.Monday,
		},
		{
			name:    "weekly Thursday with colliding Thu-tz",
			tail:    "Thursday at 9am (America/Thule)",
			wantUTC: "2026-09-10T12:00:00Z", // next Thu 9am ADT
			weekday: time.Thursday,
		},
		{
			name:    "calendar date with year with colliding Mon-tz",
			tail:    "Nov 6, 2026 9am (America/Montreal)",
			wantUTC: "2026-11-06T14:00:00Z", // Nov 6 9am EST
			weekday: time.Friday,
		},
	}
	for _, tc := range positive {
		t.Run("positive/"+tc.name, func(t *testing.T) {
			resetTimezoneWarningsForTest(t)
			var warnings bytes.Buffer
			previous := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
			banner := "Claude usage limit reached. Your limit will reset " + tc.tail
			hit, reset, parsed := NewLimitDetector(nil).Check(banner, tmux.ProgramClaude, now)
			if !hit || !parsed {
				t.Fatalf("hit=%v parsed=%v reset=%s", hit, parsed, reset)
			}
			if got := reset.Format(time.RFC3339); got != tc.wantUTC {
				t.Fatalf("resetAt=%s, want %s", got, tc.wantUTC)
			}
			if w := reset.In(time.UTC).Weekday(); w != tc.weekday {
				t.Fatalf("resetAt weekday=%s, want %s", w, tc.weekday)
			}
			if !reset.After(now) {
				t.Fatalf("resetAt %s not after now %s", reset, now)
			}
			if warnings.Len() != 0 {
				t.Fatalf("valid colliding zone must not warn: %s", &warnings)
			}
		})
	}
}
