package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// followAgentConversation is a recorded row's turn of the capture loop
// (#7101). Codex keeps the active thread's rollout open, so the conversation a
// pane is in is the top-level rollout its runtime holds — and a pane can move:
// a user-started `/new`, a `/resume` of an older thread. Capture used to stop at
// the first id it recorded, so the record went stale while the pane worked on;
// the next lost-restore then (correctly) refused the stale id and started
// fresh, discarding the thread the session's context lived in. This re-points
// the record at the thread the runtime holds now.
//
// What it must never move:
//   - a record while a swap, handoff, or carry is pending or any operation is
//     in flight — they read the recorded id as the conversation they carry or
//     retire (session.ConversationFollowFrozen, checked here and again inside
//     the commit). A freeze that outlasts a few checks is reported once;
//   - an operator's set-conversation repair, until the runtime holds something
//     other than what it held when the repair was made, and then only to that
//     new thread (#7120 review). What it held is persisted with the record, so
//     a restart does not reset it; a repair that carries no observation takes
//     this check's answer as its baseline, persisted the same way (#7122);
//   - a record that changed since this check read it.
//
// The answer comes only from the runtime af launched — verified launch
// evidence, or an identity kept from an unanswerable reattach probe once the
// session-wide pane scan re-verifies it (#7123), never the session's active
// pane (#7120 review). One check costs
// one stat per verified root plus that process's descriptor list in the steady
// state; nothing in the conversation store is walked. The commit takes the
// per-session op-lock the swap, handoff, and set-conversation paths hold, and
// then the repository start lock, held across the commit, its write, and the
// rollback a failed write takes (#7252, #7254 review).
func (m *Manager) followAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, recorded session.AgentConversationData) error {
	if recorded.Agent != token.Agent() {
		return nil
	}
	if reason := inst.ConversationFollowFrozen(); reason != "" {
		if inst.NoteConversationFollowFrozen(reason) {
			return fmt.Errorf("not following recorded conversation %s while %s; it is checked again once that settles", recorded.ID, reason)
		}
		return nil
	}
	inst.ClearConversationFollowFrozen()
	// An identity an unanswerable reattach probe kept is re-verified here,
	// by the session-wide pane scan, before the held query judges from it
	// (#7123). A recorded row keeps its record either way, so its failure is
	// the follow's own and names no restore consequence.
	if err := inst.CheckConversationCaptureEvidence(); err != nil {
		return fmt.Errorf("could not confirm recorded conversation %s is still the pane's: %w", recorded.ID, err)
	}
	current, err := session.HeldAgentConversation(token.Agent(), inst.BindConversationCaptureRuntime(snap))
	if err != nil {
		return fmt.Errorf("could not confirm recorded conversation %s is still the pane's: %w", recorded.ID, err)
	}
	if !inst.ConversationFollowPermitted(token, recorded, current) {
		return nil
	}

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()
	m.mu.Lock()
	tracked := m.instances[key]
	m.mu.Unlock()
	if tracked != inst || inst.UserKilled() || inst.IsArchived() {
		return nil
	}
	// The move is committed, persisted, and on failure rolled back inside one
	// start-lock critical section (#7254 review). Every whole-row writer
	// snapshots the row under this lock, so none can see the moved id unless
	// this write saved it: one that did, after an unlocked commit or before an
	// unlocked rollback, would persist the move and the rollback would then
	// leave memory behind a disk that has it. opLock -> start lock -> the
	// instance's own mutex is the order set_conversation.go takes too.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	outcome := inst.FollowAgentConversationForRuntime(token, recorded, current)
	if outcome == session.ConversationFollowUnchanged {
		repoStartLock.Unlock()
		return nil
	}
	persistErr := persistInstanceData(repoID, inst.ToInstanceData())
	if persistErr != nil {
		// Roll the in-memory record back to what disk still holds, mirroring
		// the operator-repair path in set_conversation.go: the follow move
		// can never be re-proposed (its own re-read judges Unchanged) and the
		// poll's pending-write gate is never armed for this write, so leaving
		// the move committed in memory would diverge memory and disk until an
		// unrelated row write happened to land. Restoring recorded makes the
		// next follow check re-detect the runtime/thread mismatch and retry
		// the persist — the retry contract the warn-only best-effort pattern
		// presumes.
		inst.SetAgentConversation(recorded)
	}
	repoStartLock.Unlock()
	if persistErr != nil {
		m.warn().Printf("session %q: %s, but the record could not be saved: %v; it stays on conversation %s, and the next follow check retries (#7252)",
			inst.Title, followSeenForLog(outcome, recorded, current), persistErr, recorded.ID)
		return nil
	}
	switch {
	case outcome == session.ConversationFollowBaselined:
		m.info().Printf("session %q: the operator-recorded conversation %s stands; its Codex pane now holds %s, and only a move off that is followed (#7122)%s",
			inst.Title, recorded.ID, heldThreadForLog(current), baselineRestoreCaveat(current))
	case strings.EqualFold(recorded.ID, current.ID):
		m.info().Printf("session %q: its Codex pane is now on the conversation %s an operator recorded; following it like any capture from here (#7120)",
			inst.Title, current.ID)
	default:
		m.info().Printf("session %q: its Codex pane moved from conversation %s to %s; recorded the thread its runtime now holds open (#7101)",
			inst.Title, recorded.ID, current.ID)
	}
	return nil
}

// followSeenForLog is what the follow saw, for the line it logs when it could
// not save what it saw: the record it would have written was rolled back.
func followSeenForLog(outcome session.ConversationFollowOutcome, recorded, held session.AgentConversationData) string {
	switch {
	case outcome == session.ConversationFollowBaselined:
		return fmt.Sprintf("its Codex pane holds %s under the operator-recorded conversation %s, a baseline af keeps with the record (#7122)", heldThreadForLog(held), recorded.ID)
	case strings.EqualFold(recorded.ID, held.ID):
		return fmt.Sprintf("its Codex pane is now on the conversation %s an operator recorded (#7120)", held.ID)
	}
	return fmt.Sprintf("its Codex pane moved from conversation %s to %s", recorded.ID, held.ID)
}

// baselineRestoreCaveat is what the follow's baseline does not buy at a
// restore (#7132 review F-3): af read the pane only after the repair, so a
// restore counts that thread as the repair's own only if its rollout was
// already in the store when the id was set, and otherwise as a move that
// starts the restore fresh.
func baselineRestoreCaveat(held session.AgentConversationData) string {
	if !held.HasID() {
		return ""
	}
	return "; af read the pane only after the id was set, so a restore keeps the id over that conversation only if it existed when the id was set, and otherwise starts fresh"
}

func heldThreadForLog(held session.AgentConversationData) string {
	if held.HasID() {
		return "conversation " + held.ID
	}
	return "no conversation yet"
}

// repairHeldObservation is what an operator's set-conversation request saw
// of the runtime as it began, for the stamp the repair record carries (#7120
// review): the follow check leaves the repair standing until the runtime
// holds something else.
type repairHeldObservation struct {
	// token is the runtime live when the request began; the stamp describes
	// it and no other (#7129).
	token session.AgentRuntimeToken
	// heldID is what it held open, when ok says the held query answered.
	heldID string
	ok     bool
	// at is when the request began — before the walk and the op-lock — and
	// dates the repair whether or not anything was observed (#7129 N-4).
	at time.Time
}

// observeRepairHeld takes that answer BEFORE the repair's op-lock (#7122,
// #7120 review F-H): it walks the process table, which on darwin can block
// on a member that is mid-exec, and a slow walk must never hold the lock a
// kill or archive waits on. A query that cannot answer leaves the repair
// unstamped; the follow's first look at the same runtime then becomes its
// baseline.
func observeRepairHeld(inst *session.Instance, requestedAt time.Time) repairHeldObservation {
	o := repairHeldObservation{token: inst.AgentRuntimeToken(), at: requestedAt}
	scope := inst.SessionCodexCaptureScope()
	if !scope.CapturesCodex() || o.token.Agent() != tmux.ProgramCodex {
		return o
	}
	held, err := session.HeldAgentConversation(o.token.Agent(), inst.BindConversationCaptureRuntime(scope))
	if err != nil {
		return o
	}
	o.heldID, o.ok = held.ID, true
	return o
}

// stamp folds the observation into the repair it was taken for, under the
// op-lock. Every repair is dated no later than the request (#7129 N-4; #7125
// review R-2): the staleness bound then treats a thread the pane moved to
// while the op-lock wait and the store walk ran as the move the follow sees
// it as. The stamp names what the runtime held only while that runtime is
// still the live one. A respawn or swap that landed in between launched a
// runtime after the request began, which held nothing then (#7129 N-1), so
// the repair records that its runtime was replaced as of the request, rather
// than letting the follow baseline the replacement. The observation may be
// of either runtime, so it is not kept: the repair is judged as unobserved
// for writes before the request and as moved by any write since. A token
// that moved without a new launch (a reverted handoff) reads the same way,
// which can only end the repair sooner.
func (o repairHeldObservation) stamp(inst *session.Instance, conv session.AgentConversationData) session.AgentConversationData {
	conv = conv.WithRepairRequestedAt(o.at)
	switch {
	case inst.AgentRuntimeToken() != o.token:
		return conv.WithRepairRuntimeReplaced(o.at)
	case o.ok:
		return conv.WithRepairStamp(o.heldID)
	}
	return conv
}
