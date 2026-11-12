package session

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// The bound on an operator repair's standing (#7122). `af sessions
// set-conversation` records the thread the operator wants the next resume to
// use, typically an older thread than the one the pane is on. The restore
// guard and the handoff carry used to exempt such a record from the
// newer-thread comparison outright, trusting the follow check to end the
// exemption when the pane moved — which it can only do while it can see the
// pane. Whenever it could not (launch evidence missing or retired, a lasting
// ambiguity, a stuck freeze, a daemon restart, a remote row), a thread the
// user started after the repair was silently passed over.
//
// The exemption now covers exactly what the operator overrode: the threads
// that existed when the repair was made. Each newer candidate's mint time is
// compared against the repair's persisted CapturedAt, so the rule needs no
// observation of the pane at all.

// Refusal reasons for a resume af could not check, as opposed to one it
// checked and found wrong. An operator's repair is not retired on these
// (refusalKeepsOperatorRepair).
const (
	codexHomeUnresolvedReason   = "af cannot resolve the Codex home this session writes to"
	codexStoreUnverifiedReason  = "af could not verify the recorded conversation against the Codex home this session uses"
	codexRepairUnverifiedReason = "af could not read the operator-recorded conversation's session metadata to confirm it is this session's own"
)

// codexRolloutMintPrecision is the resolution of both mint-time sources:
// Codex stamps session_meta to the millisecond, and a UUIDv7 carries
// milliseconds.
const codexRolloutMintPrecision = time.Millisecond

// WithRepairStamp returns the operator record c stamped with what the runtime
// held open when the repair was made: heldID, or "" when it held no thread.
func (c AgentConversationData) WithRepairStamp(heldID string) AgentConversationData {
	c.RepairStamped = true
	c.RepairHeldID = strings.ToLower(strings.TrimSpace(heldID))
	return c
}

// withoutRepairStamp is c with no repair stamp: the record as it leaves this
// host, where the runtime the stamp describes does not exist.
func (c AgentConversationData) withoutRepairStamp() AgentConversationData {
	c.RepairStamped = false
	c.RepairHeldID = ""
	return c
}

// operatorRepairBound is the instant an operator repair's exemption stops at:
// a thread minted at or before it is one the operator chose over, so it never
// makes the repair stale, and a thread minted after it does. It is zero — no
// exemption, the record is judged like any capture — for anything af cannot
// date as a real repair: another kind, a record with no capture time (a
// hand-edited operator kind), or one dated after now, which no repair is and
// which would otherwise exempt every thread still to come.
func operatorRepairBound(conv AgentConversationData, now time.Time) time.Time {
	if conv.CaptureKind != ConversationCaptureOperator || conv.CapturedAt.IsZero() || conv.CapturedAt.After(now) {
		return time.Time{}
	}
	return conv.CapturedAt
}

// operatorRepairProvenanceReason re-checks, when a repair is about to be
// resumed or carried, what set-conversation proved when it wrote it
// (ValidateSettableConversation): the rollout is a top-level thread, not a
// sub-agent's, launched in this session's directory. Only the validator
// writes the operator kind, but a record can also arrive carried from another
// host or edited by hand, and the kind alone must not buy an exemption. ""
// means it holds; launchDir is the unresolved launch directory.
func operatorRepairProvenanceReason(rolloutPath, launchDir string) string {
	if strings.TrimSpace(launchDir) == "" {
		return "af cannot determine this session's launch directory, so it cannot confirm the operator-recorded conversation is this session's own"
	}
	cwd, subagent, ok := codexRolloutSessionMeta(rolloutPath)
	switch {
	case !ok:
		return codexRepairUnverifiedReason
	case subagent:
		return "the operator-recorded conversation is a Codex sub-agent thread, not this session's own conversation"
	case pathutil.ResolveForCompare(cwd) != pathutil.ResolveForCompare(launchDir):
		return "the operator-recorded conversation was started in another directory, not in this session's"
	}
	return ""
}

// refusalKeepsOperatorRepair reports whether a refused resume of conv must
// leave the record in place rather than retire it: an operator's repair
// refused only because af could not look — the home would not resolve, the
// store would not enumerate, the repaired rollout's header would not read —
// is no evidence about the id, and the next restore checks it again (#7120
// review F-G).
func refusalKeepsOperatorRepair(conv AgentConversationData, reason string) bool {
	if conv.CaptureKind != ConversationCaptureOperator {
		return false
	}
	switch reason {
	case codexHomeUnresolvedReason, codexStoreUnverifiedReason, codexRepairUnverifiedReason:
		return true
	}
	return false
}

// codexRolloutsNotMintedBy drops every path proven minted at or before bound
// and keeps the rest — including any af cannot date, which fails closed into
// the ordinary comparison. The mint time is rounded up to its precision, so a
// thread minted in the same millisecond as the repair counts as newer.
func codexRolloutsNotMintedBy(paths []string, bound time.Time) []string {
	kept := paths[:0:0]
	for _, path := range paths {
		if minted, ok := codexRolloutMintTime(path); ok && !minted.Add(codexRolloutMintPrecision).After(bound) {
			continue
		}
		kept = append(kept, path)
	}
	return kept
}

// codexRolloutMintTime is the instant Codex minted a rollout's thread.
//
// The source is the session_meta header's timestamp: Codex's recorder writes
// it once, as the first line, from the same clock reading that names the
// file, formatted in UTC and marked Z — so it is an instant, readable the
// same way whatever zone the daemon or the pane runs in, and it is the line
// the classifier reads anyway. The rollout's filename and day directory carry
// that reading in the recorder's LOCAL time with no offset: a daemon whose
// zone differs from the pane's (a service unit with no TZ against a shell
// that sets one) would read it hours off, so it is never used. The file's
// modification time is its last append, not its mint, and a birth time is
// neither portable nor preserved by a copy.
//
// A header with no parseable stamp falls back to the thread id: Codex mints
// thread ids as UUIDv7, whose first 48 bits are the Unix time in
// milliseconds. Neither source means "unknown": the caller treats the
// rollout as possibly minted after any bound.
func codexRolloutMintTime(path string) (time.Time, bool) {
	if line, err := readCodexRolloutFirstLine(path); err == nil {
		var header struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Timestamp string `json:"timestamp"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &header) == nil && header.Type == "session_meta" {
			for _, stamp := range []string{header.Payload.Timestamp, header.Timestamp} {
				// RFC 3339 requires the zone designator, so a stamp without
				// one is refused rather than read as UTC.
				if minted, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
					return minted, true
				}
			}
		}
	}
	return codexUUIDv7Time(codexConversationIDFromPath(path))
}

// codexUUIDv7Time decodes the millisecond Unix timestamp a version-7,
// RFC 4122-variant UUID carries in its first 48 bits.
func codexUUIDv7Time(id string) (time.Time, bool) {
	if !codexConversationIDRe.MatchString(id) {
		return time.Time{}, false
	}
	hex := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if hex[12] != '7' || !strings.ContainsRune("89ab", rune(hex[16])) {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(hex[:12], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}
