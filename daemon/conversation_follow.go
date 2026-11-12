package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
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
//     new thread (#7120 review);
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
	if !inst.FollowAgentConversationForRuntime(token, recorded, current) {
		return nil
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	persistErr := persistInstanceData(repoID, inst.ToInstanceData())
	repoStartLock.Unlock()
	m.info().Printf("session %q: its Codex pane moved from conversation %s to %s; recorded the thread its runtime now holds open (#7101)",
		inst.Title, recorded.ID, current.ID)
	if persistErr != nil {
		m.warn().Printf("failed to persist conversation id for %q: %v", inst.Title, persistErr)
	}
	return nil
}

// stampConversationRepair records what the runtime held when an operator's
// set-conversation repair landed, so the follow check leaves the repair until
// the runtime moves (#7120 review). It runs under the repair's op-lock. A held
// query that cannot answer leaves the repair unstamped; the first follow
// observation then becomes its baseline, which can only keep the repair longer.
func stampConversationRepair(inst *session.Instance, conv session.AgentConversationData) {
	scope := inst.SessionCodexCaptureScope()
	if !scope.CapturesCodex() {
		return
	}
	held, err := session.HeldAgentConversation(conv.Agent, inst.BindConversationCaptureRuntime(scope))
	if err != nil {
		return
	}
	inst.StampConversationRepair(conv.ID, held.ID)
}
