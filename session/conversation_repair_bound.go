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
// user moved to after the repair was silently passed over.
//
// The exemption now covers exactly what the operator overrode, read from the
// record and the store alone (operatorRepairWindow.disregards): threads whose
// rollouts were already in the store when the repair was made, and of those —
// when the repair knows which thread the pane held — only that one and the
// ones nobody has written since.
//
// "The repair" is the request (#7129): it is dated no later than the moment
// the request began, and its stamp describes the runtime live then. A runtime
// that takes that one's place afterwards held nothing at the repair, so what
// it holds or writes is a move; what the old runtime wrote before it was
// replaced is still judged by the stamp (#7132 review F-1).

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

// WithRepairStamp returns the operator record c stamped with what the request
// saw the runtime hold open as it began: heldID, or "" when it held no thread.
// The stamp is an observation, so that thread is the repair's own however
// late its rollout entered the store (#7129 N-2).
func (c AgentConversationData) WithRepairStamp(heldID string) AgentConversationData {
	c.RepairStamped = true
	c.RepairHeldID = strings.ToLower(strings.TrimSpace(heldID))
	c.RepairHeldObserved = true
	return c
}

// WithRepairRequestedAt dates the repair no later than requestedAt, the
// moment the request began (#7129 N-4). "The repair" is the request: a thread
// the pane started while the request waited for the op-lock or walked the
// store is one the operator never saw, so it must not count as one the repair
// overrode — stamped or not. It only ever moves the date earlier, which only
// ever disregards less.
func (c AgentConversationData) WithRepairRequestedAt(requestedAt time.Time) AgentConversationData {
	if !requestedAt.IsZero() && requestedAt.Before(c.CapturedAt) {
		c.CapturedAt = requestedAt
	}
	return c
}

// WithRepairObservation is WithRepairStamp for an observation taken at
// observedAt, and dates the repair no later than it. The stamp and the
// staleness bound must agree on when the repair happened (#7125 review R-2):
// a thread the pane moves to after the observation is a move to the follow,
// so the bound must not count it as one the operator overrode merely because
// it entered the store before the repair was validated and written.
func (c AgentConversationData) WithRepairObservation(heldID string, observedAt time.Time) AgentConversationData {
	return c.WithRepairStamp(heldID).WithRepairRequestedAt(observedAt)
}

// WithRepairRuntimeReplaced records that the runtime the repair was made
// against was replaced at at (#7129 N-1): the runtime that took its place did
// not exist when the request began, so it held nothing then, and whatever it
// holds is a move — for the follow, and, for any thread written after at, for
// a restore the follow never saw. The stamp itself is kept, because it still
// says which of the threads written before at were the old runtime's own
// (#7132 review F-1). An earlier replacement stands: every runtime since
// then is a later one. The repair itself stands until the follow or a
// restore sees a move.
func (c AgentConversationData) WithRepairRuntimeReplaced(at time.Time) AgentConversationData {
	if at.IsZero() {
		return c
	}
	if c.RepairRuntimeReplacedAt.IsZero() || at.Before(c.RepairRuntimeReplacedAt) {
		c.RepairRuntimeReplacedAt = at.Round(0)
	}
	return c
}

// repairHeldAtRepair is what the follow compares the live runtime's answer
// with: the thread the stamp says the runtime held at the repair, and whether
// af knows it. A replaced runtime held nothing then, whatever the stamp says
// about the one before it.
func (c AgentConversationData) repairHeldAtRepair() (id string, known bool) {
	if !c.RepairRuntimeReplacedAt.IsZero() {
		return "", true
	}
	return c.RepairHeldID, c.RepairStamped
}

// withRepairBaseline is the stamp the follow gives a repair the request could
// not observe: what the runtime holds at the follow's first look, standing in
// for what it held at the repair. It is no observation of the repair — the
// pane may have moved in between — so it earns no exemption beyond the
// window's own date (#7129 N-2, N-3).
func (c AgentConversationData) withRepairBaseline(heldID string) AgentConversationData {
	c.RepairStamped = true
	c.RepairHeldID = strings.ToLower(strings.TrimSpace(heldID))
	c.RepairHeldObserved = false
	return c
}

// withoutRepairStamp is c with no repair stamp: the record as it leaves this
// host, where the runtime the stamp describes does not exist.
func (c AgentConversationData) withoutRepairStamp() AgentConversationData {
	c.RepairStamped = false
	c.RepairHeldID = ""
	c.RepairHeldObserved = false
	c.RepairRuntimeReplacedAt = time.Time{}
	return c
}

// markRepairRuntimeReplacedLocked is N-1 at the moment af learns the live
// runtime is not the one a standing repair was made against: a launch
// boundary, which records a runtime started after any repair the row carries;
// a reattach that found another process behind the tmux name (#7132 review
// F-2); or a re-scan that found the kept identity gone or rooting no pane
// (#7147 G-3). Without it the stamp would go on naming the old runtime's
// thread, which the follow and the bound would both exempt in the new pane,
// or an unstamped repair would let the follow baseline from the new pane.
//
// at is as early as af can defend. A launch passes the instant tmux took
// just before it ran new-session, after its existence gate confirmed no
// session behind the name (TmuxSession.SpawnedAt, launchReplacementInstant),
// so writes the new pane makes while af is still recording the launch — the
// ready poll and the tmux calls after it, seconds under load — are already
// moves (#7147 G-2). The old pane was gone by then, so none of its writes can
// land after that instant. A replacement af only detects is dated at
// detection; the process may have started earlier. The surrounding launch or
// settle owns UpdatedAt and the checkpoint that persists it. Callers hold
// i.mu.
func (i *Instance) markRepairRuntimeReplacedLocked(at time.Time) {
	if len(i.Tabs) == 0 {
		return
	}
	if conv := i.Tabs[0].Conversation; conv.CaptureKind == ConversationCaptureOperator {
		i.Tabs[0].Conversation = conv.WithRepairRuntimeReplaced(at)
	}
}

// operatorRepairWindow is what an operator repair overrode. Its zero value
// overrides nothing: the record is judged like any capture.
type operatorRepairWindow struct {
	at         time.Time
	stamped    bool
	heldID     string
	observed   bool
	replacedAt time.Time
}

// operatorRepairWindowOf reads the window from the record alone. It is the
// zero window for anything af cannot date as a real repair: another kind, a
// record with no capture time (a hand-edited operator kind), or one dated
// after now, which no repair is and which would otherwise override every
// thread still to come.
//
// A replacement instant dated after now is one af cannot place either — a
// launch recorded while the clock ran fast, before NTP stepped it back — so
// it is read as the repair's own date: every write since the repair is a
// move, which fails closed exactly as the CapturedAt guard does (#7147 G-1).
func operatorRepairWindowOf(conv AgentConversationData, now time.Time) operatorRepairWindow {
	if conv.CaptureKind != ConversationCaptureOperator || conv.CapturedAt.IsZero() || conv.CapturedAt.After(now) {
		return operatorRepairWindow{}
	}
	replacedAt := conv.RepairRuntimeReplacedAt
	if replacedAt.After(now) {
		replacedAt = conv.CapturedAt
	}
	return operatorRepairWindow{
		at:         conv.CapturedAt,
		stamped:    conv.RepairStamped,
		heldID:     strings.ToLower(conv.RepairHeldID),
		observed:   conv.RepairStamped && conv.RepairHeldObserved,
		replacedAt: replacedAt,
	}
}

// launchReplacementInstant is the instant a launch records a standing
// repair's runtime as replaced at: spawnedAt, taken just before the process
// was spawned, or now when it has none — or when spawnedAt is after now,
// which only a clock stepped back during the launch produces, and which
// would otherwise hand the new pane's first writes to the old stamp. Both
// are compared as wall-clock readings: the instant is persisted and compared
// with file mtimes, and two time.Now() readings compare by their monotonic
// clock, which a step of the wall clock does not move.
func launchReplacementInstant(spawnedAt, now time.Time) time.Time {
	spawnedAt, now = spawnedAt.Round(0), now.Round(0)
	if spawnedAt.IsZero() || spawnedAt.After(now) {
		return now
	}
	return spawnedAt
}

// disregards reports whether a rollout newer than the repaired one (path,
// last written at modified) is a thread the operator chose over, so it does
// not make the repair stale.
//
//   - A thread written after the runtime the repair was made against was
//     replaced is a move, whatever the rest of the window says: only a runtime
//     that held nothing at the repair can have written it (#7129 N-1). Writes
//     before that instant are the old runtime's, judged by the rules below
//     (#7132 review F-1).
//   - The thread the request saw the runtime hold is the repair's own, however
//     late its rollout entered the store, and even when its first line cannot
//     be dated at all: the request's held query classified that rollout's
//     header, so its store date says nothing more (#7129 N-2). A fresh pane's
//     first prompt can stamp its first line while the request's walk runs. A
//     baseline's thread is not exempt this way — the follow took it after the
//     repair.
//   - Otherwise its rollout must have been in the store when the repair was
//     made (codexRolloutStoredAt) — a thread whose rollout appeared later,
//     even one Codex constructed earlier, is one the user moved to afterwards
//     (#7125 review R-1).
//   - And when the repair carries a stamp, a thread other than the stamped one
//     must not have been written since the repair: an older thread the pane
//     resumed after the repair is a move too (R-3). A repair with no stamp
//     cannot tell the pane's own thread from the others, so every thread
//     already in the store counts as overridden.
//
// The written-since test needs only the name and the mtime the caller already
// has, so it runs before the header read it makes unnecessary (#7129 N-5): in
// a shared home every other worktree's active thread fails it.
func (w operatorRepairWindow) disregards(path string, modified time.Time) bool {
	if w.at.IsZero() {
		return false
	}
	held := w.heldID != "" && codexConversationIDFromPath(path) == w.heldID
	switch {
	case !w.replacedAt.IsZero() && modified.After(w.replacedAt):
		return false
	case held && w.observed:
		return true
	case w.stamped && !held && modified.After(w.at):
		return false
	}
	stored, ok := codexRolloutStoredAt(path)
	return ok && !stored.Add(codexRolloutStampPrecision).After(w.at)
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
	if codexRolloutCompressed(rolloutPath) {
		// Kept, not retired, but refused at every restore until the rollout
		// is plain again: af never resumes it, so Codex never writes it back
		// out. Say how to get it back (#7134 review F-5).
		return declineResumeUnverified("the operator-recorded conversation's rollout is stored compressed, and af cannot read a compressed rollout to confirm it is this session's own; " +
			"to resume it, decompress it in place (zstd -d --rm " + strconv.Quote(rolloutPath) + ") and run af sessions set-conversation again")
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
