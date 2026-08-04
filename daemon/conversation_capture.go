package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
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
	// A pending re-create notice was decided before this id existed (#2629). A
	// root whose command selects its own conversation records nothing at launch,
	// so the heal could only say "context unknown" — and if the id just captured
	// IS the carried one, af has now proven the continuity it could not see. Ask
	// for the verdict again so the refreshed answer rides the write below rather
	// than waiting for an unrelated one. A no-op for every ordinary session and
	// for any notice already acknowledged.
	//
	// The previous value is captured BEFORE the refresh so a failed persist can
	// put it back; reading it afterwards would only ever return the new one.
	previousNotice := inst.RootRecreateContext()
	noticeRefreshed := inst.RefreshRecreateContext()

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	// Snapshot INSIDE the lock, and announce inside it too. Both matter: a payload
	// read before entering this ordering domain can be overtaken by a concurrent
	// tab mutation that persists and publishes its newer roster first, and then
	// this older whole-InstanceData payload lands last and makes every open client
	// re-project the tab change away until an unrelated snapshot repairs it. Tab
	// and status events announce under this same lock for exactly that reason.
	//
	// It also orders this goroutine behind the CREATE that spawned it, which is
	// what keeps a fast capture from publishing before session.created. CreateSession
	// holds this same per-repo lock from before it registers the capture until after
	// it publishes EventSessionCreated, so this Lock() cannot be taken until that
	// event is out. Clients upsert created and updated identically, so the reverse
	// order would let the create's older payload land last and put a just-cleared
	// notice back on the row. That guarantee lives in CreateSession's lock scope —
	// narrowing it there re-opens this, which is why it is named here.

	data := inst.ToInstanceData()
	err = persistInstanceData(repoID, data)
	if err == nil && noticeRefreshed {
		m.publishEvent(agentproto.EventSessionUpdated, data)
	}
	repoStartLock.Unlock()
	if err != nil {
		if noticeRefreshed {
			// Memory and disk must not diverge over a write that did not happen. The
			// refresh can go either way — clearing an `unknown` that capture just
			// disproved, or escalating one — so leaving it applied would either drop a
			// warning this daemon still owes the user or show one disk contradicts,
			// and a restart would flip it back either way (the #2814 ack-path fix,
			// applied to its sibling write).
			inst.ReconcileRootRecreateContext(previousNotice)
		}
		log.WarningLog.Printf("failed to persist conversation id for %q: %v", inst.Title, err)
	}
}
