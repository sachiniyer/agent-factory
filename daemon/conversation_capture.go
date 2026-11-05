package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// conversationCaptureTimeout bounds how long ONE capture attempt waits for a
// new provider conversation to APPEAR. It is not the whole capture budget: a
// rollout that appeared but whose session_meta is still being written gets the
// session layer's settle extension on top (#3266), so a capture goroutine can
// outlive this window — consumers of pendingConversationCaptures already
// tolerate that. And it is not the whole discovery budget either: an armed
// loop re-asks on conversationCaptureRearmInterval while the row is live and
// unrecorded (#6769).
var conversationCaptureTimeout = 2 * time.Second

// conversationCaptureRearmInterval paces the re-ask an armed capture loop
// makes while its row is live and unrecorded. It is deliberately not the poll
// cadence: a store walk per second per unrecorded session would be the daemon
// spending its tick re-proving "nothing there" on rows that simply have not
// prompted yet, while 10s stays well under any reasonable "the first prompt
// minted a rollout" latency. Var, not const, so tests can shorten it.
var conversationCaptureRearmInterval = 10 * time.Second

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
//
// The arm is generation-keyed (#6769): a loop already armed for THIS token is
// a duplicate, but an arm for an older runtime generation is displaced — the
// displaced goroutine notices the stolen marker on its next qualifier check
// and exits. Without that, a stale loop winding down could refuse the new
// generation's arm and leave the rollout it mints between baselines forever
// unrecorded.
func (m *Manager) startConversationCaptureLocked(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken) {
	if token.Agent() != tmux.ProgramCodex {
		return
	}
	if inst.AgentRuntimeToken() != token {
		// The caller's token is already history; whoever replaced the runtime
		// arms its own capture.
		return
	}
	if m.armedConversationCaptures == nil {
		m.armedConversationCaptures = make(map[*session.Instance]session.AgentRuntimeToken)
	}
	if armed, ok := m.armedConversationCaptures[inst]; ok && armed == token {
		return
	}
	m.armedConversationCaptures[inst] = token
	if m.pendingConversationCaptures == nil {
		m.pendingConversationCaptures = make(map[*session.Instance]int)
	}
	m.pendingConversationCaptures[inst]++
	// Both clocks are snapshot under m.mu: the loop goroutine must never read
	// the package vars back, or a test restoring them races it.
	timeout := conversationCaptureTimeout
	rearm := conversationCaptureRearmInterval
	if m.launchBackgroundMutation(func(stop <-chan struct{}) {
		m.conversationCaptureLoop(repoID, key, inst, snap, token, timeout, rearm, stop)
	}) {
		return
	}
	if m.armedConversationCaptures[inst] == token {
		delete(m.armedConversationCaptures, inst)
	}
	if m.pendingConversationCaptures[inst] <= 1 {
		delete(m.pendingConversationCaptures, inst)
	} else {
		m.pendingConversationCaptures[inst]--
	}
}

// beginConversationCapture re-arms the reap fence for the capture attempt the
// loop is about to run. The arm-time count covers registration through the
// first attempt; this re-covers each later one.
func (m *Manager) beginConversationCapture(inst *session.Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingConversationCaptures == nil {
		m.pendingConversationCaptures = make(map[*session.Instance]int)
	}
	m.pendingConversationCaptures[inst]++
}

// disarmConversationCapture releases the armed marker — but only for the token
// that owns it, so a displaced loop never clears the NEWER generation's arm.
func (m *Manager) disarmConversationCapture(inst *session.Instance, token session.AgentRuntimeToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if armed, ok := m.armedConversationCaptures[inst]; ok && armed == token {
		delete(m.armedConversationCaptures, inst)
	}
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

// conversationCaptureLoop keeps an armed capture re-asking until a
// conversation is recorded or the row stops qualifying (#6769): a Codex
// rollout minted after the first capture's window — the pane's first prompt,
// a daemon-restart gap — must still land on the record, or a restore falls
// back to the provider's latest-session behavior. Ordinary "still nothing"
// polls stay silent; each DISTINCT actual error is warned once.
//
// pendingConversationCaptures keeps its original meaning — an ATTEMPT in
// flight, the signal a reap waits on — not "a loop exists". A row that simply
// never prompts is not in-flight work, so the fence drops between attempts;
// held tracks what THIS loop currently owns so a displaced loop can never
// decrement the newer generation's fence.
func (m *Manager) conversationCaptureLoop(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, timeout, rearm time.Duration, stop <-chan struct{}) {
	// The arm-time increment is this loop's first holding, and the FIRST
	// attempt is unconditional — the arm registered pending for exactly that
	// poll, so a row that went Lost between arm and goroutine start still runs
	// it rather than draining the fence a waiting reap consults. The qualifier
	// gates RE-attempts only.
	held := true
	defer func() {
		if held {
			m.finishConversationCapture(inst)
		}
		m.disarmConversationCapture(inst, token)
	}()
	var lastErr string
	for {
		if err := m.captureAgentConversation(repoID, key, inst, snap, token, timeout); err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			m.warn().Printf("conversation capture for %q keeps failing: %v", inst.Title, err)
		}
		m.finishConversationCapture(inst)
		held = false
		timer := time.NewTimer(rearm)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !m.conversationCaptureArmed(key, inst, token, snap) {
			return
		}
		m.beginConversationCapture(inst)
		held = true
	}
}

// conversationCaptureArmed is the loop's gate: the row must still be the same
// tracked instance, still own the armed marker for THIS generation, and still
// be a live unrecorded session whose baseline can be trusted. Failing any of
// those ends the loop — a recorded conversation, a replaced runtime, a row
// that died or was archived, a baseline whose store scan never worked.
func (m *Manager) conversationCaptureArmed(key string, inst *session.Instance, token session.AgentRuntimeToken, snap session.ConversationCaptureSnapshot) bool {
	if inst == nil || inst.UserKilled() || inst.IsArchived() || inst.AgentConversation().HasID() {
		return false
	}
	if inst.AgentRuntimeToken() != token || snap.CodexBaselineFailed() {
		return false
	}
	switch inst.GetLiveness() {
	// LivenessUnset is a row the status poll has not observed yet — exactly the
	// state a just-armed create is in, and the window its first prompt lands in.
	// LiveLost is deliberately excluded: a dead pane mints no rollouts, and the
	// respawn that revives the row bumps its generation, displacing this arm.
	case session.LivenessUnset, session.LiveReady, session.LiveRunning, session.LiveLimitReached:
	default:
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[key] != inst {
		return false
	}
	armed, ok := m.armedConversationCaptures[inst]
	return ok && armed == token
}

// ArmConversationCaptures is the deferred half of #6769's recording
// guarantee: a live Codex row with no recorded conversation and no armed
// capture — the daemon restarted into it, or it predates capture — gets a
// fresh baseline and the same loop every launch gets. The expensive half (the
// baseline's store walk) runs outside m.mu; admission re-verifies the row is
// still tracked under the lock.
func (m *Manager) ArmConversationCaptures() {
	m.mu.Lock()
	instances := make(map[string]*session.Instance, len(m.instances))
	for key, inst := range m.instances {
		instances[key] = inst
	}
	kills := make(map[string]struct{}, len(m.killsInFlight))
	for key := range m.killsInFlight {
		kills[key] = struct{}{}
	}
	armed := make(map[*session.Instance]session.AgentRuntimeToken, len(m.armedConversationCaptures))
	for inst, token := range m.armedConversationCaptures {
		armed[inst] = token
	}
	m.mu.Unlock()

	for key, inst := range instances {
		if inst == nil {
			continue
		}
		if _, killing := kills[key]; killing {
			// A replacement operation (swap, handoff, kill, restore) arms its
			// own capture when the new runtime exists; a baseline built now
			// would bind the wrong store.
			continue
		}
		if inst.AgentConversation().HasID() || inst.UserKilled() || inst.IsArchived() {
			continue
		}
		switch inst.GetLiveness() {
		// LivenessUnset is a restored row the first status poll has not
		// classified yet — exactly who this pass exists for.
		case session.LivenessUnset, session.LiveReady, session.LiveRunning, session.LiveLimitReached:
		default:
			continue
		}
		token := inst.AgentRuntimeToken()
		if token.Agent() != tmux.ProgramCodex {
			continue
		}
		if armedToken, ok := armed[inst]; ok && armedToken == token {
			continue
		}
		// A respawn that retired a refused conversation parks its pre-spawn
		// baseline here (#6769 review): the replacement's rollout can already
		// exist, and a fresh before-image would exclude the file it must find.
		snap, parked := inst.TakePendingConversationCapture()
		if !parked {
			snap = inst.SessionCodexCaptureBaseline()
		}
		if !snap.CapturesCodex() {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		m.mu.Lock()
		if m.instances[key] == inst {
			m.startConversationCaptureLocked(repoID, key, inst, snap, token)
		}
		m.mu.Unlock()
	}
}

// captureAgentConversation runs one capture attempt and commits any id it
// finds. A non-nil return is the attempt's failure for the loop's rate-limited
// warning; ordinary absence (no rollout yet) returns nil — nothing wrong.
func (m *Manager) captureAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, timeout time.Duration) error {
	if inst == nil || inst.UserKilled() || inst.AgentConversation().HasID() {
		return nil
	}
	agent := token.Agent()
	if agent == "" {
		return nil
	}
	conv, err := session.CaptureAgentConversation(agent, snap, timeout)
	if err != nil {
		return err
	}
	if !conv.HasID() {
		return nil
	}

	// Serialize the mutate+persist against this session's archive/kill/restore
	// teardown, exactly as the tab verbs do: take the
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
		return nil
	}
	if !inst.SetAgentConversationForRuntime(token, conv) {
		return nil
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
	persistErr := persistInstanceData(repoID, data)
	if persistErr == nil && noticeRefreshed {
		m.publishEvent(agentproto.EventSessionUpdated, data)
	}
	repoStartLock.Unlock()
	if persistErr != nil {
		if noticeRefreshed {
			// Memory and disk must not diverge over a write that did not happen. The
			// refresh can go either way — clearing an `unknown` that capture just
			// disproved, or escalating one — so leaving it applied would either drop a
			// warning this daemon still owes the user or show one disk contradicts,
			// and a restart would flip it back either way (the #2814 ack-path fix,
			// applied to its sibling write).
			inst.ReconcileRootRecreateContext(previousNotice)
		}
		m.warn().Printf("failed to persist conversation id for %q: %v", inst.Title, persistErr)
	}
	return nil
}
