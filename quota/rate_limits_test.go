package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures under testdata/ratelimits are HANDWRITTEN from the Codex
// rollout record shape (codex-rs RolloutLine/RolloutItem/EventMsg::TokenCount/
// RateLimitSnapshot). This is a public repo: they contain no real conversation
// text, ids, or account names — every value is fabricated.

func rateLimitFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "ratelimits", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("fixture %s holds %d lines, want exactly 1", name, len(lines))
	}
	return []byte(lines[0])
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func TestParseCodexRateLimitsLine(t *testing.T) {
	tests := []struct {
		fixture    string
		verdict    RateLimitsVerdict
		windows    []UsageWindow
		recordedAt time.Time
	}{
		{
			fixture: "both_windows.jsonl",
			verdict: RateLimitsParsed,
			windows: []UsageWindow{
				{Name: "primary", UsedPercent: 98.5, WindowMinutes: int64ptr(300), ResetsAt: timeptr(time.Unix(1761902400, 0).UTC())},
				{Name: "secondary", UsedPercent: 41.2, WindowMinutes: int64ptr(10080), ResetsAt: timeptr(time.Unix(1762387200, 0).UTC())},
			},
			recordedAt: mustTime(t, "2026-10-31T04:52:13.492Z"),
		},
		{
			fixture: "primary_only.jsonl",
			verdict: RateLimitsParsed,
			windows: []UsageWindow{
				{Name: "primary", UsedPercent: 12.0, WindowMinutes: int64ptr(300), ResetsAt: timeptr(time.Unix(1761906000, 0).UTC())},
			},
			recordedAt: mustTime(t, "2026-10-31T04:55:00.000Z"),
		},
		{
			// A well-formed snapshot carrying no window data is still a
			// recorded snapshot — evidence of nothing, which is NOT evidence
			// of a healthy account.
			fixture:    "no_windows.jsonl",
			verdict:    RateLimitsParsed,
			windows:    nil,
			recordedAt: mustTime(t, "2026-10-31T04:56:10.250Z"),
		},
		{fixture: "null_snapshot.jsonl", verdict: RateLimitsAbsent},
		{fixture: "missing_snapshot_key.jsonl", verdict: RateLimitsAbsent},
		// The #4721 failure shape: text that merely mentions or echoes
		// "rate_limits" — in an agent message, a user message, or a response
		// item — is not a record and must never parse as one.
		{fixture: "other_event_mentions_rate_limits.jsonl", verdict: RateLimitsAbsent},
		{fixture: "user_message_quotes_snapshot.jsonl", verdict: RateLimitsAbsent},
		{fixture: "response_item_echo.jsonl", verdict: RateLimitsAbsent},
		{fixture: "wrong_payload_type_with_limits.jsonl", verdict: RateLimitsAbsent},
		{fixture: "payload_not_object.jsonl", verdict: RateLimitsAbsent},
		{fixture: "token_usage_record.jsonl", verdict: RateLimitsAbsent},
		{fixture: "malformed_percent_string.jsonl", verdict: RateLimitsMalformed},
		{fixture: "malformed_snapshot_array.jsonl", verdict: RateLimitsMalformed},
		{fixture: "malformed_window_no_percent.jsonl", verdict: RateLimitsMalformed},
		{fixture: "malformed_no_timestamp.jsonl", verdict: RateLimitsMalformed},
		{fixture: "malformed_bad_timestamp.jsonl", verdict: RateLimitsMalformed},
		{
			fixture: "null_window_fields.jsonl",
			verdict: RateLimitsParsed,
			windows: []UsageWindow{
				{Name: "primary", UsedPercent: 7.5, WindowMinutes: nil, ResetsAt: nil},
			},
			recordedAt: mustTime(t, "2026-10-31T05:05:00.000Z"),
		},
		{fixture: "torn_tail.jsonl", verdict: RateLimitsAbsent},
		{fixture: "non_object.jsonl", verdict: RateLimitsAbsent},
		{
			fixture: "integer_percent.jsonl",
			verdict: RateLimitsParsed,
			windows: []UsageWindow{
				{Name: "primary", UsedPercent: 42, WindowMinutes: int64ptr(300), ResetsAt: timeptr(time.Unix(1761902400, 0).UTC())},
			},
			recordedAt: mustTime(t, "2026-10-31T05:07:00.000Z"),
		},
		{
			// Fields a future Codex adds — to the snapshot or to a window —
			// are ignored; only the fields af interprets are pinned.
			fixture: "forward_compat_fields.jsonl",
			verdict: RateLimitsParsed,
			windows: []UsageWindow{
				{Name: "primary", UsedPercent: 63.25, WindowMinutes: int64ptr(300), ResetsAt: timeptr(time.Unix(1761902400, 0).UTC())},
				{Name: "secondary", UsedPercent: 9.0, WindowMinutes: int64ptr(10080), ResetsAt: timeptr(time.Unix(1762387200, 0).UTC())},
			},
			recordedAt: mustTime(t, "2026-10-31T05:08:00.000Z"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			verdict, reading := ParseCodexRateLimitsLine(rateLimitFixture(t, tc.fixture))
			if verdict != tc.verdict {
				t.Fatalf("verdict = %d, want %d", verdict, tc.verdict)
			}
			if tc.verdict != RateLimitsParsed {
				if len(reading.Windows) != 0 || !reading.RecordedAt.IsZero() {
					t.Fatalf("non-parsed verdict returned a reading: %+v", reading)
				}
				return
			}
			if !reading.RecordedAt.Equal(tc.recordedAt) {
				t.Fatalf("RecordedAt = %s, want %s", reading.RecordedAt, tc.recordedAt)
			}
			if len(reading.Windows) != len(tc.windows) {
				t.Fatalf("windows = %+v, want %+v", reading.Windows, tc.windows)
			}
			for i, want := range tc.windows {
				got := reading.Windows[i]
				if got.Name != want.Name || got.UsedPercent != want.UsedPercent {
					t.Fatalf("window %d = %+v, want %+v", i, got, want)
				}
				if (got.WindowMinutes == nil) != (want.WindowMinutes == nil) ||
					(got.WindowMinutes != nil && *got.WindowMinutes != *want.WindowMinutes) {
					t.Fatalf("window %d WindowMinutes = %v, want %v", i, got.WindowMinutes, want.WindowMinutes)
				}
				if (got.ResetsAt == nil) != (want.ResetsAt == nil) ||
					(got.ResetsAt != nil && !got.ResetsAt.Equal(*want.ResetsAt)) {
					t.Fatalf("window %d ResetsAt = %v, want %v", i, got.ResetsAt, want.ResetsAt)
				}
			}
		})
	}
}

func TestUsageReadingWorst(t *testing.T) {
	if _, ok := (UsageReading{}).Worst(); ok {
		t.Fatal("an empty reading reported a worst window; \"no window\" is not a zero reading")
	}
	reading := UsageReading{Windows: []UsageWindow{
		{Name: "primary", UsedPercent: 12.0},
		{Name: "secondary", UsedPercent: 88.5},
	}}
	worst, ok := reading.Worst()
	if !ok || worst.Name != "secondary" || worst.UsedPercent != 88.5 {
		t.Fatalf("Worst = %+v, %v; want secondary 88.5", worst, ok)
	}
}

func int64ptr(v int64) *int64        { return &v }
func timeptr(v time.Time) *time.Time { return &v }
