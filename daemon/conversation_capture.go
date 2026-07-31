package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var conversationCaptureTimeout = 2 * time.Second

func (m *Manager) captureAgentConversationAsync(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot) {
	if inst == nil {
		return
	}
	token := inst.AgentRuntimeToken()
	m.mu.Lock()
	m.startConversationCaptureLocked(repoID, key, inst, snap, token)
	m.mu.Unlock()
}

// startConversationCaptureLocked registers discovery before launching it. The
// create path calls this while publishing the new instance under the same
// manager lock, closing the window where a status poll could observe and reap
// the instance before its capture was marked pending. Callers must hold m.mu.
func (m *Manager) startConversationCaptureLocked(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken) {
	if token.Agent() != tmux.ProgramCodex {
		return
	}
	if m.pendingConversationCaptures == nil {
		m.pendingConversationCaptures = make(map[*session.Instance]int)
	}
	m.pendingConversationCaptures[inst]++
	timeout := conversationCaptureTimeout
	go func() {
		defer m.finishConversationCapture(inst)
		m.captureAgentConversation(repoID, key, inst, snap, token, timeout)
	}()
}

func (m *Manager) finishConversationCapture(inst *session.Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingConversationCaptures[inst] <= 1 {
		delete(m.pendingConversationCaptures, inst)
		return
	}
	m.pendingConversationCaptures[inst]--
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

	// Serialize the mutate+persist against this session's archive/kill/restore
	// teardown, exactly as SetPRInfo (#2437) and the tab verbs do: take the
	// per-session op-lock first, re-confirm the tracked session, and refuse an
	// archived one. With no op-lock (the earlier behavior) this write could
	// interleave INSIDE ArchiveSession — between its teardown and its persist — and
	// write a conversation id (and whatever else ToInstanceData snapshots) over the
	// record the archive just committed; archive is inert in BOTH directions
	// (#1809, #2451). The runtime-generation token does not close this: archive
	// never bumps agentRuntimeGeneration, so the token still matches post-archive.
	//
	// The op-lock is taken AFTER the capture subprocess, so a slow capture never
	// blocks a kill or archive, and op-lock BEFORE the per-repo start lock matches
	// the kill/archive ordering. The caller spawns this async and never waits on
	// it, so parking behind an in-flight op cannot deadlock.
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != inst || inst.UserKilled() || inst.IsArchived() {
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
