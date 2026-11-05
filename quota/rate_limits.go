package quota

import (
	"bytes"
	"encoding/json"
	"time"
)

// This file is evidence class 2 of the account-handoff design (#6776 slice 1a):
// per-account usage readings parsed out of the agent's OWN records. It is
// Codex-only — the claim that other agents persist an equivalent limit
// percentage in their transcripts is unverified, and unverified means af reads
// nothing there (the same rule the package comment sets for entitlement: an
// absent integration degrades to the truthful answer, never a guess).
//
// The record shape is Codex's rollout serialization (codex-rs): every JSONL
// line is {"timestamp", "type", "payload"}; type "event_msg" wraps an
// internally-tagged EventMsg; EventMsg::TokenCount serializes as
// {"type":"token_count","info":…,"rate_limits":{…}}. A snapshot carries two
// optional windows, "primary" and "secondary", each a RateLimitWindow of
// {used_percent, window_minutes, resets_at(unix seconds)}. Newer rollouts also
// carry an "ordinal" field and a growing set of snapshot fields (limit_id,
// credits, plan_type, …) — unknown fields are ignored so a Codex release adding
// one does not turn every reading unknown, but a field af INTERPRETS parsing to
// the wrong type makes the claim malformed.
//
// The match is STRUCTURAL, on the record's own discriminator fields — never a
// substring: agent output quoting the text "rate_limits" (a user turn, a
// message, a transcript echo — the #4721 failure shape) must never parse as
// evidence.

// UsageWindow is one rate-limit window exactly as the agent recorded it.
type UsageWindow struct {
	// Name is the snapshot's own slot name — "primary" or "secondary" — so a
	// report can say WHICH window is at the wall rather than averaging them
	// into a number the provider never reported.
	Name string
	// UsedPercent is the recorded percentage of the window consumed.
	UsedPercent float64
	// WindowMinutes is the rolling window's length in minutes, or nil when
	// the record carried no duration.
	WindowMinutes *int64
	// ResetsAt is when the window resets, or nil when the record carried none.
	ResetsAt *time.Time
}

// UsageReading is one rate_limits snapshot: the windows the agent recorded
// and when it recorded them. Windows may be empty — a snapshot carrying no
// window data is still a recorded snapshot, which is evidence of absence of
// data, not evidence of a healthy account.
type UsageReading struct {
	Windows    []UsageWindow
	RecordedAt time.Time
}

// Worst returns the window with the highest recorded UsedPercent. The second
// result is false when the snapshot carried no windows: "no window" is not a
// zero reading, and callers must not treat it as headroom.
func (r UsageReading) Worst() (UsageWindow, bool) {
	var worst UsageWindow
	found := false
	for _, window := range r.Windows {
		if !found || window.UsedPercent > worst.UsedPercent {
			worst, found = window, true
		}
	}
	return worst, found
}

// RateLimitsVerdict is what one rollout line claimed about rate limits. The
// three-way split is the reader's honesty rule: a line that makes no claim is
// skipped, a well-formed claim is evidence, and a line that claims a snapshot
// but does not fit the pinned shape is MALFORMED — it contributes "unknown",
// never a silently-guessed reading.
type RateLimitsVerdict int

const (
	// RateLimitsAbsent: the line is not a rate_limits record — a different
	// record type, a different event, a token_count with no snapshot, or an
	// unparseable fragment (a rollout's tail can be torn mid-write; that is
	// not a claim).
	RateLimitsAbsent RateLimitsVerdict = iota
	// RateLimitsParsed: the line carried a well-formed rate_limits snapshot.
	RateLimitsParsed
	// RateLimitsMalformed: the line claims a rate_limits snapshot — event_msg,
	// token_count, a non-null rate_limits field — but the snapshot does not
	// match the pinned record shape.
	RateLimitsMalformed
)

// codexRateLimitWindow is the pinned wire shape of one window. Every field is
// a pointer so "absent" and "present-but-wrong" stay distinguishable from real
// values: upstream types used_percent as required and the other two as
// optional, and a record that disagrees is malformed, not zero.
type codexRateLimitWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *int64   `json:"window_minutes"`
	ResetsAt      *int64   `json:"resets_at"`
}

// ParseCodexRateLimitsLine classifies one Codex rollout JSONL line and, when
// it is a well-formed rate_limits record, returns the reading. The zero
// reading is returned for every non-Parsed verdict.
func ParseCodexRateLimitsLine(line []byte) (RateLimitsVerdict, UsageReading) {
	var envelope struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return RateLimitsAbsent, UsageReading{}
	}
	if envelope.Type != "event_msg" || len(envelope.Payload) == 0 {
		return RateLimitsAbsent, UsageReading{}
	}
	var payload struct {
		Type       string          `json:"type"`
		RateLimits json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		// A payload that is not even an object cannot be a token_count event,
		// so this line makes no claim.
		return RateLimitsAbsent, UsageReading{}
	}
	if payload.Type != "token_count" ||
		len(payload.RateLimits) == 0 ||
		bytes.Equal(bytes.TrimSpace(payload.RateLimits), []byte("null")) {
		return RateLimitsAbsent, UsageReading{}
	}

	// From here the line CLAIMS a rate_limits snapshot: failures are
	// malformed claims, never silent skips.
	recordedAt, err := time.Parse(time.RFC3339Nano, envelope.Timestamp)
	if err != nil {
		return RateLimitsMalformed, UsageReading{}
	}
	var snapshot struct {
		Primary   *codexRateLimitWindow `json:"primary"`
		Secondary *codexRateLimitWindow `json:"secondary"`
	}
	if err := json.Unmarshal(payload.RateLimits, &snapshot); err != nil {
		return RateLimitsMalformed, UsageReading{}
	}
	reading := UsageReading{RecordedAt: recordedAt}
	for _, slot := range []struct {
		name   string
		window *codexRateLimitWindow
	}{
		{"primary", snapshot.Primary},
		{"secondary", snapshot.Secondary},
	} {
		if slot.window == nil {
			continue
		}
		if slot.window.UsedPercent == nil {
			return RateLimitsMalformed, UsageReading{}
		}
		window := UsageWindow{
			Name:          slot.name,
			UsedPercent:   *slot.window.UsedPercent,
			WindowMinutes: slot.window.WindowMinutes,
		}
		if slot.window.ResetsAt != nil {
			resetsAt := time.Unix(*slot.window.ResetsAt, 0).UTC()
			window.ResetsAt = &resetsAt
		}
		reading.Windows = append(reading.Windows, window)
	}
	return RateLimitsParsed, reading
}
