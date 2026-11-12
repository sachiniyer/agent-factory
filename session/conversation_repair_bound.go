package session

import (
	"encoding/json"
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
// user moved to after the repair was silently passed over.
//
// The exemption now covers exactly what the operator overrode, read from the
// record and the store alone (operatorRepairWindow.disregards): threads whose
// rollouts were already in the store when the repair was made, and of those —
// when the repair knows which thread the pane held — only that one and the
// ones nobody has written since.

// resumeDecline is why prepareExactResumeConversation did not resume the
// recorded conversation; reason is "" when there is nothing to say. unverified
// types a refusal that only says af could not look — the Codex home would not
// resolve, the store would not enumerate, a header it needed would not read —
// apart from one that looked and found the id wrong (missing, ambiguous,
// stale, someone else's). The first is no evidence about the id, so an
// operator's repair is not retired on it (#7120 review F-G, #7125 review R-4).
type resumeDecline struct {
	reason     string
	unverified bool
}

func declineResume(reason string) resumeDecline { return resumeDecline{reason: reason} }

func declineResumeUnverified(reason string) resumeDecline {
	return resumeDecline{reason: reason, unverified: true}
}

// retiresRecord reports whether the declined record is retired once the fresh
// spawn commits: any refusal of a capture's record, and a refusal of an
// operator's repair that rests on evidence about the id.
func (d resumeDecline) retiresRecord(conv AgentConversationData) bool {
	if d.reason == "" {
		return false
	}
	return conv.CaptureKind != ConversationCaptureOperator || !d.unverified
}

// codexRolloutStampPrecision is the resolution of Codex's session_meta stamps:
// milliseconds.
const codexRolloutStampPrecision = time.Millisecond

// WithRepairStamp returns the operator record c stamped with what the runtime
// held open when the repair was made: heldID, or "" when it held no thread.
func (c AgentConversationData) WithRepairStamp(heldID string) AgentConversationData {
	c.RepairStamped = true
	c.RepairHeldID = strings.ToLower(strings.TrimSpace(heldID))
	return c
}

// WithRepairObservation is WithRepairStamp for an observation taken at
// observedAt, and dates the repair no later than it. The stamp and the
// staleness bound must agree on when the repair happened (#7125 review R-2):
// a thread the pane moves to after the observation is a move to the follow,
// so the bound must not count it as one the operator overrode merely because
// it entered the store before the repair was validated and written.
func (c AgentConversationData) WithRepairObservation(heldID string, observedAt time.Time) AgentConversationData {
	c = c.WithRepairStamp(heldID)
	if !observedAt.IsZero() && observedAt.Before(c.CapturedAt) {
		c.CapturedAt = observedAt
	}
	return c
}

// withoutRepairStamp is c with no repair stamp: the record as it leaves this
// host, where the runtime the stamp describes does not exist.
func (c AgentConversationData) withoutRepairStamp() AgentConversationData {
	c.RepairStamped = false
	c.RepairHeldID = ""
	return c
}

// operatorRepairWindow is what an operator repair overrode. Its zero value
// overrides nothing: the record is judged like any capture.
type operatorRepairWindow struct {
	at      time.Time
	stamped bool
	heldID  string
}

// operatorRepairWindowOf reads the window from the record alone. It is the
// zero window for anything af cannot date as a real repair: another kind, a
// record with no capture time (a hand-edited operator kind), or one dated
// after now, which no repair is and which would otherwise override every
// thread still to come.
func operatorRepairWindowOf(conv AgentConversationData, now time.Time) operatorRepairWindow {
	if conv.CaptureKind != ConversationCaptureOperator || conv.CapturedAt.IsZero() || conv.CapturedAt.After(now) {
		return operatorRepairWindow{}
	}
	return operatorRepairWindow{at: conv.CapturedAt, stamped: conv.RepairStamped, heldID: strings.ToLower(conv.RepairHeldID)}
}

// disregards reports whether a rollout newer than the repaired one (path,
// last written at modified) is a thread the operator chose over, so it does
// not make the repair stale. It must have been in the store when the repair
// was made (codexRolloutStoredAt) — a thread whose rollout appeared later,
// even one Codex constructed earlier, is one the user moved to afterwards
// (#7125 review R-1). And when the repair knows which thread the pane held,
// it must be that thread or one nobody has written to since the repair: an
// older thread the pane resumed after the repair is a move too (R-3). A
// repair with no observation cannot tell the pane's own thread from the
// others, so every thread already in the store counts as overridden.
func (w operatorRepairWindow) disregards(path string, modified time.Time) bool {
	if w.at.IsZero() {
		return false
	}
	stored, ok := codexRolloutStoredAt(path)
	if !ok || stored.Add(codexRolloutStampPrecision).After(w.at) {
		return false
	}
	if !w.stamped {
		return true
	}
	if w.heldID != "" && codexConversationIDFromPath(path) == w.heldID {
		return true
	}
	return !modified.After(w.at)
}

// operatorRepairProvenance re-checks, when a repair is about to be resumed or
// carried, what set-conversation proved when it wrote it
// (ValidateSettableConversation): the rollout is a top-level thread, not a
// sub-agent's, launched in this session's directory. Only the validator
// writes the operator kind, but a record can also arrive carried from another
// host or edited by hand, and the kind alone must not buy an exemption. The
// zero decline means it holds; launchDir is the unresolved launch directory.
func operatorRepairProvenance(rolloutPath, launchDir string) resumeDecline {
	if strings.TrimSpace(launchDir) == "" {
		return declineResumeUnverified("af cannot determine this session's launch directory, so it cannot confirm the operator-recorded conversation is this session's own")
	}
	cwd, subagent, ok := codexRolloutSessionMeta(rolloutPath)
	switch {
	case !ok:
		return declineResumeUnverified("af could not read the operator-recorded conversation's session metadata to confirm it is this session's own")
	case subagent:
		return declineResume("the operator-recorded conversation is a Codex sub-agent thread, not this session's own conversation")
	case pathutil.ResolveForCompare(cwd) != pathutil.ResolveForCompare(launchDir):
		return declineResume("the operator-recorded conversation was started in another directory, not in this session's")
	}
	return resumeDecline{}
}

// codexRolloutStoredAt is the instant a rollout entered the store: the
// session_meta LINE's own timestamp. Codex's recorder dates a new thread when
// it constructs it — that reading names the file and day directory and is the
// session_meta payload's timestamp, and the thread id is a UUIDv7 of about the
// same instant — but it creates the file and writes that first line, stamped
// with the clock at the write, only at the thread's first persist: its first
// prompt (codex-rs rollout/src/recorder.rs, deferred creation). A fresh pane
// therefore holds a constructed thread with no rollout until it is prompted.
// Only the line stamp says when the rollout appeared; a construction date can
// only under-date it, so it never stands alone. The later of the two is used,
// so a payload stamped after its line cannot be under-dated either.
//
// Both stamps are UTC marked Z, so the comparison is between instants however
// the daemon's and the pane's zones differ; RFC 3339 requires the zone
// designator, so a stamp without one is refused rather than read as UTC. The
// filename and day directory carry the construction reading in the
// recorder's LOCAL time with no offset and are never read. A rollout with no
// datable line is reported undatable, which the caller reads as "may have
// appeared after the repair".
func codexRolloutStoredAt(path string) (time.Time, bool) {
	line, err := readCodexRolloutFirstLine(path)
	if err != nil {
		return time.Time{}, false
	}
	var header struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &header) != nil || header.Type != "session_meta" {
		return time.Time{}, false
	}
	stored, err := time.Parse(time.RFC3339Nano, header.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	if constructed, err := time.Parse(time.RFC3339Nano, header.Payload.Timestamp); err == nil && constructed.After(stored) {
		stored = constructed
	}
	return stored, true
}
