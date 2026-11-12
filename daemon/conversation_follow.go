package daemon

import (
	"fmt"
	"strings"

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
// evidence, never the session's active pane (#7120 review). One check costs
// one stat per verified root plus that process's descriptor list in the steady
// state; nothing in the conversation store is walked. The commit takes the
// per-session op-lock the swap, handoff, and set-conversation paths hold.
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
	outcome := inst.FollowAgentConversationForRuntime(token, recorded, current)
	if outcome == session.ConversationFollowUnchanged {
		return nil
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	persistErr := persistInstanceData(repoID, inst.ToInstanceData())
	repoStartLock.Unlock()
	switch {
	case outcome == session.ConversationFollowBaselined:
		m.info().Printf("session %q: the operator-recorded conversation %s stands; its Codex pane now holds %s, and only a move off that is followed (#7122)",
			inst.Title, recorded.ID, heldThreadForLog(current))
	case strings.EqualFold(recorded.ID, current.ID):
		m.info().Printf("session %q: its Codex pane is now on the conversation %s an operator recorded; following it like any capture from here (#7120)",
			inst.Title, current.ID)
	default:
		m.info().Printf("session %q: its Codex pane moved from conversation %s to %s; recorded the thread its runtime now holds open (#7101)",
			inst.Title, recorded.ID, current.ID)
	}
	if persistErr != nil {
		m.warn().Printf("failed to persist conversation id for %q: %v", inst.Title, persistErr)
	}
	return nil
}

func heldThreadForLog(held session.AgentConversationData) string {
	if held.HasID() {
		return "conversation " + held.ID
	}
	return "no conversation yet"
}

// repairHeldObservation is which thread the runtime held open when an
// operator's set-conversation repair was requested, for the stamp the repair
// record carries (#7120 review): the follow check leaves the repair standing
// until the runtime holds something else.
type repairHeldObservation struct {
	token  session.AgentRuntimeToken
	heldID string
	ok     bool
}

// observeRepairHeld takes that answer BEFORE the repair's op-lock (#7122,
// #7120 review F-H): it walks the process table, which on darwin can block
// on a member that is mid-exec, and a slow walk must never hold the lock a
// kill or archive waits on. A query that cannot answer leaves the repair
// unstamped; the first follow observation then becomes its baseline, which
// can only keep the repair longer.
func observeRepairHeld(inst *session.Instance) repairHeldObservation {
	scope := inst.SessionCodexCaptureScope()
	if !scope.CapturesCodex() {
		return repairHeldObservation{}
	}
	token := inst.AgentRuntimeToken()
	if token.Agent() != tmux.ProgramCodex {
		return repairHeldObservation{}
	}
	held, err := session.HeldAgentConversation(token.Agent(), inst.BindConversationCaptureRuntime(scope))
	if err != nil {
		return repairHeldObservation{}
	}
	return repairHeldObservation{token: token, heldID: held.ID, ok: true}
}

// stamp folds the observation into the repair it was taken for, under the
// op-lock — only while the runtime it observed is still the live one, so a
// respawn or swap that landed in between leaves the repair unstamped rather
// than stamped with another runtime's thread.
func (o repairHeldObservation) stamp(inst *session.Instance, conv session.AgentConversationData) session.AgentConversationData {
	if !o.ok || inst.AgentRuntimeToken() != o.token {
		return conv
	}
	return conv.WithRepairStamp(o.heldID)
}
