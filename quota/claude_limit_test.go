package quota

import (
	"encoding/json"
	"testing"
	"time"
)

// The record shape under test is Claude Code's own transcript record for a
// refused request (#6944): an assistant JSONL line with
// apiError="usage_limit_reached" and isApiErrorMessage=true, carrying
// apiErrorParams.rate_limit_info.resetsAt (epoch seconds), quotaLimits.resetsAt,
// the unwrapped display text in message.content, the record's timestamp, and
// the conversation's session_id. Every fixture is fabricated — this is a
// public repo and the fixtures carry no real conversation content.
//
// The parser's honesty rule mirrors ParseCodexRateLimitsLine: the match is
// STRUCTURAL on the record's own discriminator fields, never a substring — a
// user or agent quoting "usage_limit_reached" or the banner text must parse as
// no claim, and a claim that does not fit the pinned shape is malformed, never
// silently skipped.

// claudeLimitLine builds one transcript record line. Fields a case needs to
// vary are supplied through overrides; absent overrides produce the real,
// complete record shape observed under v2.1.309–v2.1.319.
func claudeLimitLine(t *testing.T, mutate func(rec map[string]any)) []byte {
	t.Helper()
	rec := map[string]any{
		"isSidechain": false,
		"type":        "assistant",
		"uuid":        "9f67fcf0-379b-42a7-9d74-a27b3e5f7207",
		"timestamp":   "2026-11-07T17:38:02.566Z",
		"message": map[string]any{
			"model": "<synthetic>",
			"role":  "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)"},
			},
		},
		"requestId": "req_011Cfake",
		"apiError":  "usage_limit_reached",
		"apiErrorParams": map[string]any{
			"rate_limit_info": map[string]any{
				"status":        "rejected",
				"resetsAt":      1794073200,
				"rateLimitType": "five_hour",
			},
		},
		"quotaLimits": map[string]any{
			"status":        "rejected",
			"resetsAt":      1794073200,
			"rateLimitType": "five_hour",
		},
		"error":             "rate_limit",
		"isApiErrorMessage": true,
		"apiErrorStatus":    429,
		"session_id":        "28e1b947-ba23-4588-988b-3c688b6f241c",
		"cwd":               "/home/user/project",
		"version":           "2.1.317",
	}
	if mutate != nil {
		mutate(rec)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	return line
}

func mustTimestamp(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse fixture timestamp %q: %v", s, err)
	}
	return ts
}

func TestParseClaudeLimitRecordLine(t *testing.T) {
	wantReset := time.Unix(1794073200, 0).UTC()
	wantRecordedAt := mustTimestamp(t, "2026-11-07T17:38:02.566Z")
	wantText := "You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)"
	wantSession := "28e1b947-ba23-4588-988b-3c688b6f241c"

	cases := []struct {
		name     string
		line     []byte
		verdict  ClaudeLimitVerdict
		resetsAt *time.Time
		text     string
		recorded time.Time
		session  string
	}{
		{
			name:     "full refusal record",
			line:     claudeLimitLine(t, nil),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     wantText,
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "resetsAt falls back to quotaLimits when rate_limit_info lacks it",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["apiErrorParams"] = map[string]any{
					"rate_limit_info": map[string]any{"status": "rejected"},
				}
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     wantText,
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "refusal publishing no reset is parsed with a nil ResetsAt",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["apiErrorParams"] = map[string]any{
					"rate_limit_info": map[string]any{"status": "rejected"},
				}
				delete(rec, "quotaLimits")
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: nil,
			text:     wantText,
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "record without apiErrorParams still parses (quotaLimits supplies the reset)",
			line: claudeLimitLine(t, func(rec map[string]any) {
				delete(rec, "apiErrorParams")
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     wantText,
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "weekly refusal parses identically",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["message"].(map[string]any)["content"] = []any{
					map[string]any{"type": "text", "text": "You've hit your weekly limit · resets 10am (America/Los_Angeles)"},
				}
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     "You've hit your weekly limit · resets 10am (America/Los_Angeles)",
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "non-text content blocks are ignored, text joins",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["message"].(map[string]any)["content"] = []any{
					map[string]any{"type": "thinking", "thinking": "..."},
					map[string]any{"type": "text", "text": "You've hit your 5-hour limit"},
					map[string]any{"type": "text", "text": " · resets 9:40am (America/Los_Angeles)"},
				}
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     "You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)",
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		{
			name: "missing message still parses the refusal (text empty)",
			line: claudeLimitLine(t, func(rec map[string]any) {
				delete(rec, "message")
			}),
			verdict:  ClaudeLimitParsed,
			resetsAt: &wantReset,
			text:     "",
			recorded: wantRecordedAt,
			session:  wantSession,
		},
		// Claims whose own fields do not fit the pinned shape are malformed —
		// never silently skipped, never silently guessed.
		{
			name: "resetsAt wrong type is malformed",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["apiErrorParams"].(map[string]any)["rate_limit_info"].(map[string]any)["resetsAt"] = "1794073200"
			}),
			verdict: ClaudeLimitMalformed,
		},
		{
			name: "resetsAt fractional is malformed",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["apiErrorParams"].(map[string]any)["rate_limit_info"].(map[string]any)["resetsAt"] = 1794073200.5
			}),
			verdict: ClaudeLimitMalformed,
		},
		{
			name: "timestamp wrong type is malformed",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["timestamp"] = 1794073200
			}),
			verdict: ClaudeLimitMalformed,
		},
		{
			name: "unparseable timestamp is malformed",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["timestamp"] = "yesterday"
			}),
			verdict: ClaudeLimitMalformed,
		},
		{
			name: "missing timestamp is malformed — a claim with no instant cannot be ordered",
			line: claudeLimitLine(t, func(rec map[string]any) {
				delete(rec, "timestamp")
			}),
			verdict: ClaudeLimitMalformed,
		},
		// Everything below makes NO claim and is absent — above all the
		// #4721-class shapes where the banner's own words appear inside
		// something else.
		{
			name: "a different apiError is no usage-limit claim",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["apiError"] = "overloaded"
			}),
			verdict: ClaudeLimitAbsent,
		},
		{
			name: "apiError absent is no claim",
			line: claudeLimitLine(t, func(rec map[string]any) {
				delete(rec, "apiError")
			}),
			verdict: ClaudeLimitAbsent,
		},
		{
			name: "isApiErrorMessage false is no claim",
			line: claudeLimitLine(t, func(rec map[string]any) {
				rec["isApiErrorMessage"] = false
			}),
			verdict: ClaudeLimitAbsent,
		},
		{
			name: "isApiErrorMessage missing is no claim",
			line: claudeLimitLine(t, func(rec map[string]any) {
				delete(rec, "isApiErrorMessage")
			}),
			verdict: ClaudeLimitAbsent,
		},
		{
			name:    "a user message quoting the banner text is no claim",
			line:    []byte(`{"type":"user","timestamp":"2026-11-07T17:38:02.566Z","message":{"role":"user","content":"You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)"}}`),
			verdict: ClaudeLimitAbsent,
		},
		{
			name:    "a tool result containing a usage_limit_reached record text is no claim",
			line:    []byte(`{"type":"user","timestamp":"2026-11-07T17:38:02.566Z","message":{"role":"user","content":[{"type":"tool_result","content":"{\"apiError\":\"usage_limit_reached\",\"resetsAt\":1794073200}"}]}}`),
			verdict: ClaudeLimitAbsent,
		},
		{
			name:    "an ordinary assistant record is no claim",
			line:    []byte(`{"type":"assistant","timestamp":"2026-11-07T17:38:02.566Z","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I will run the command now."}]}}`),
			verdict: ClaudeLimitAbsent,
		},
		{name: "empty line is no claim", line: nil, verdict: ClaudeLimitAbsent},
		{name: "whitespace line is no claim", line: []byte("   "), verdict: ClaudeLimitAbsent},
		{name: "non-object JSON is no claim", line: []byte(`"usage_limit_reached"`), verdict: ClaudeLimitAbsent},
		{name: "torn tail of a record is no claim", line: []byte(`{"type":"assistant","apiError":"usage_limit_reached","apiErrorPa`), verdict: ClaudeLimitAbsent},
		{name: "not JSON is no claim", line: []byte(`You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)`), verdict: ClaudeLimitAbsent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, rec := ParseClaudeLimitRecordLine(tc.line)
			if verdict != tc.verdict {
				t.Fatalf("verdict = %d, want %d", verdict, tc.verdict)
			}
			if tc.verdict != ClaudeLimitParsed {
				if rec.ResetsAt != nil || !rec.RecordedAt.IsZero() || rec.Text != "" || rec.SessionID != "" {
					t.Fatalf("non-parsed verdict returned a record: %+v", rec)
				}
				return
			}
			if (rec.ResetsAt == nil) != (tc.resetsAt == nil) {
				t.Fatalf("ResetsAt = %v, want %v", rec.ResetsAt, tc.resetsAt)
			}
			if rec.ResetsAt != nil && !rec.ResetsAt.Equal(*tc.resetsAt) {
				t.Fatalf("ResetsAt = %v, want %v", rec.ResetsAt, *tc.resetsAt)
			}
			if rec.Text != tc.text {
				t.Fatalf("Text = %q, want %q", rec.Text, tc.text)
			}
			if !rec.RecordedAt.Equal(tc.recorded) {
				t.Fatalf("RecordedAt = %v, want %v", rec.RecordedAt, tc.recorded)
			}
			if rec.SessionID != tc.session {
				t.Fatalf("SessionID = %q, want %q", rec.SessionID, tc.session)
			}
		})
	}
}
