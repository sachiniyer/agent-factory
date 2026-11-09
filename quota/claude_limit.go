package quota

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// This file is the Claude Code sibling of rate_limits.go (#6944): the
// structural reader for the usage-limit record Claude's transcript JSONL
// carries. When a request is refused for a plan limit, Claude appends an
// assistant record with apiError="usage_limit_reached",
// isApiErrorMessage=true, and apiErrorParams.rate_limit_info.resetsAt — the
// wall's reset instant in epoch seconds, with quotaLimits.resetsAt as a
// sibling carrier. The display text lives in message.content but is never
// parsed here: the epoch resetsAt is the reset's only trustworthy form,
// because the rendered clause can wrap in a narrow pane and a weekly reset's
// text does not even name its day.
//
// The match is STRUCTURAL, on the record's own discriminator fields — never
// a substring: a user turn or tool result quoting "usage_limit_reached" or
// the banner text (the #4721 failure shape) must never parse as evidence,
// and a line that claims the refusal record but does not fit the pinned
// shape is malformed — "unknown", never a silently-guessed reading.

// ClaudeLimitVerdict is what one transcript line claimed about a usage-limit
// refusal. The three-way split mirrors RateLimitsVerdict: a line that makes
// no claim is skipped, a well-formed claim is evidence, and a claim that
// does not fit the pinned shape is MALFORMED.
type ClaudeLimitVerdict int

const (
	// ClaudeLimitAbsent: the line is not a refusal record — a different
	// record type, a refusal for another reason, content quoting the claim's
	// vocabulary, or an unparseable fragment (a transcript's tail can be
	// torn mid-write; that is not a claim).
	ClaudeLimitAbsent ClaudeLimitVerdict = iota
	// ClaudeLimitParsed: the line carried a well-formed refusal record.
	ClaudeLimitParsed
	// ClaudeLimitMalformed: the line claims a usage-limit refusal —
	// apiError="usage_limit_reached" with isApiErrorMessage=true — but a
	// field af interprets does not fit the pinned shape.
	ClaudeLimitMalformed
)

// ClaudeLimitRecord is one refusal record exactly as Claude wrote it.
type ClaudeLimitRecord struct {
	// ResetsAt is when the wall resets, or nil when the record published
	// none — a refusal without a reset statement is still a refusal.
	ResetsAt *time.Time
	// RecordedAt is the record's own timestamp — the ordering key for "the
	// freshest refusal statement", which file position cannot supply.
	RecordedAt time.Time
	// Text is the record's display text (the unwrapped banner) — retained
	// for diagnostics, never parsed for the reset.
	Text string
	// SessionID is the conversation the record belongs to.
	SessionID string
	// RateLimitType is the wall kind the record names —
	// apiErrorParams.rate_limit_info.rateLimitType ("five_hour",
	// "seven_day", …) — "" when the record publishes none.
	RateLimitType string
}

// ParseClaudeLimitRecordLine classifies one Claude transcript JSONL line
// and, when it is a well-formed usage-limit refusal, returns the record. The
// zero record is returned for every non-Parsed verdict.
func ParseClaudeLimitRecordLine(line []byte) (ClaudeLimitVerdict, ClaudeLimitRecord) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var rec map[string]any
	if err := dec.Decode(&rec); err != nil {
		return ClaudeLimitAbsent, ClaudeLimitRecord{}
	}
	apiError, _ := rec["apiError"].(string)
	isRefusal, _ := rec["isApiErrorMessage"].(bool)
	if apiError != "usage_limit_reached" || !isRefusal {
		return ClaudeLimitAbsent, ClaudeLimitRecord{}
	}

	// From here the line CLAIMS a refusal: a field af reads parsing to the
	// wrong shape is malformed, never a silent skip. The timestamp is the
	// ordering key — a claim that cannot be ordered is no claim at all.
	recordedAt, ok := claudeRecordTime(rec["timestamp"])
	if !ok {
		return ClaudeLimitMalformed, ClaudeLimitRecord{}
	}

	out := ClaudeLimitRecord{RecordedAt: recordedAt}
	if id, _ := rec["session_id"].(string); id != "" {
		out.SessionID = id
	}
	out.Text = claudeRecordText(rec["message"])
	if params, isObj := rec["apiErrorParams"].(map[string]any); isObj {
		if info, isObj := params["rate_limit_info"].(map[string]any); isObj {
			// Optional metadata af does not interpret — an absent or
			// non-string value reads as unnamed, never a malformed claim.
			out.RateLimitType, _ = info["rateLimitType"].(string)
		}
	}

	resetsAt, ok := claudeResetsAt(rec["apiErrorParams"], "rate_limit_info")
	if !ok {
		return ClaudeLimitMalformed, ClaudeLimitRecord{}
	}
	if resetsAt == nil {
		// The primary carrier published none — the quotaLimits sibling is
		// the fallback, and it is malformed by the same rule when it claims
		// a reset in a shape af cannot read.
		resetsAt, ok = claudeResetsAt(rec["quotaLimits"], "")
		if !ok {
			return ClaudeLimitMalformed, ClaudeLimitRecord{}
		}
	}
	out.ResetsAt = resetsAt
	return ClaudeLimitParsed, out
}

// claudeRecordTime decodes the record's RFC3339 timestamp. ok is false when
// the field is absent, not a string, or unparseable — the claim is malformed
// in all three cases.
func claudeRecordTime(v any) (time.Time, bool) {
	text, _ := v.(string)
	if text == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// claudeResetsAt extracts a resetsAt epoch-seconds instant from the map at
// rec[field] (drilling one level when field is non-empty). The three results
// are: a decoded instant, nil when the record carries no reset statement
// (container or field absent — a refusal may legitimately publish none), and
// ok=false when a container or the field itself is present in a shape af
// cannot read — a malformed claim, never a guessed value.
func claudeResetsAt(container any, field string) (*time.Time, bool) {
	if container == nil {
		return nil, true
	}
	obj, isObj := container.(map[string]any)
	if !isObj {
		return nil, false
	}
	if field != "" {
		nested, present := obj[field]
		if !present || nested == nil {
			return nil, true
		}
		obj, isObj = nested.(map[string]any)
		if !isObj {
			return nil, false
		}
	}
	raw, present := obj["resetsAt"]
	if !present || raw == nil {
		return nil, true
	}
	seconds, isNumber := raw.(json.Number)
	if !isNumber {
		return nil, false
	}
	unix, err := seconds.Int64()
	if err != nil {
		return nil, false
	}
	instant := time.Unix(unix, 0).UTC()
	return &instant, true
}

// claudeRecordText concatenates the text blocks of the record's
// message.content. The display text is retained as evidence of what the pane
// showed, but the parser never reads a reset out of it — every non-text or
// non-array shape yields "".
func claudeRecordText(message any) string {
	obj, isObj := message.(map[string]any)
	if !isObj {
		return ""
	}
	blocks, isArray := obj["content"].([]any)
	if !isArray {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		entry, isObj := block.(map[string]any)
		if !isObj {
			continue
		}
		if kind, _ := entry["type"].(string); kind != "text" {
			continue
		}
		if text, _ := entry["text"].(string); text != "" {
			b.WriteString(text)
		}
	}
	return b.String()
}
