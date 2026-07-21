package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

var conversationCaptureTimeout = 2 * time.Second

func (m *Manager) captureAgentConversationAsync(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot) {
	if inst == nil {
		return
	}
	token := inst.AgentRuntimeToken()
	go m.captureAgentConversation(repoID, key, inst, snap, token, conversationCaptureTimeout)
}

func (m *Manager) captureAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, timeout time.Duration) {
	if inst == nil || inst.UserKilled() || inst.AgentConversation().HasID() {
		return
	}
	agent := token.Agent()
	if agent == "" {
		return
	}
	conv, err := session.CaptureAgentConversation(agent, snap, timeout)
	if err != nil {
		log.WarningLog.Printf("conversation capture for %q failed: %v", inst.Title, err)
		return
	}
	if !conv.HasID() {
		return
	}

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != inst || inst.UserKilled() {
		return
	}
	if !inst.SetAgentConversationForRuntime(token, conv) {
		return
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	err = persistInstanceData(repoID, inst.ToInstanceData())
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist conversation id for %q: %v", inst.Title, err)
	}
}
