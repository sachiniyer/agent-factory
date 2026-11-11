package daemon

import (
	"fmt"
	"strings"

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
// It never moves the record while a swap, handoff, or carry is pending — they
// read the recorded id as the conversation they carry or retire — nor while
// any operation is in flight (ConversationFollowFrozen, checked before the
// walk and again inside the commit). The commit takes the per-session op-lock
// the swap, handoff, and set-conversation paths hold, and only replaces the
// exact record it judged, so an operator's repair or a concurrent capture that
// landed meanwhile stands.
//
// One check costs one process-table read plus the pane members' descriptor
// lists; nothing in the conversation store is walked, and a held header this
// daemon has already classified is not read again (session.HeldAgentConversation).
func (m *Manager) followAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, recorded session.AgentConversationData) error {
	if recorded.Agent != token.Agent() || inst.ConversationFollowFrozen() != "" {
		return nil
	}
	current, err := session.HeldAgentConversation(token.Agent(), inst.BindConversationCaptureRuntime(snap))
	if err != nil {
		return fmt.Errorf("could not confirm recorded conversation %s is still the pane's: %w", recorded.ID, err)
	}
	if !current.HasID() || strings.EqualFold(current.ID, recorded.ID) {
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
