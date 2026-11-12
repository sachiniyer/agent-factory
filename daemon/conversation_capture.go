package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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
//
// Reports whether the arm actually proceeded and its loop was launched. A
// false return is a decline: the deferred-arm caller must not have spent the
// instance's parked baseline (#6946) and puts it back.
func (m *Manager) startConversationCaptureLocked(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken) bool {
	if token.Agent() != tmux.ProgramCodex {
		return false
	}
	if inst.AgentRuntimeToken() != token {
		// The caller's token is already history; whoever replaced the runtime
		// arms its own capture.
		return false
	}
	if m.armedConversationCaptures == nil {
		m.armedConversationCaptures = make(map[*session.Instance]session.AgentRuntimeToken)
	}
	if armed, ok := m.armedConversationCaptures[inst]; ok && armed == token {
		return false
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
		return true
	}
	if m.armedConversationCaptures[inst] == token {
		delete(m.armedConversationCaptures, inst)
	}
	if m.pendingConversationCaptures[inst] <= 1 {
		delete(m.pendingConversationCaptures, inst)
	} else {
		m.pendingConversationCaptures[inst]--
	}
	return false
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

// conversationCaptureLoop keeps an armed capture re-asking while the row
// qualifies (#6769): a Codex rollout minted after the first capture's window —
// the pane's first prompt, a daemon-restart gap — must still land on the
// record, or a restore falls back to the provider's latest-session behavior.
// Recording does not end it (#7101): a recorded row keeps a follow check on the
// same cadence, so a pane that moves to another thread (`/new`, `/resume`)
// re-records it instead of leaving a stale id the next restore must refuse.
// Ordinary "still nothing" polls stay silent; each DISTINCT actual error is
// warned once.
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
		if !m.conversationCaptureArmed(key, inst, token) {
			return
		}
		m.beginConversationCapture(inst)
		held = true
		// A failed before-image never answers the diff, and keeping it for
		// the runtime's life turned a transient store failure — EMFILE, EIO —
		// into the permanent loss of the one channel that can answer while the
		// runtime holds two top-level threads (#7124 review F3). Take it again:
		// one walk per rearm, never per poll. A late baseline only hides a
		// rollout minted before it, which the held query still finds.
		if snap.CodexBaselineErr() != nil && !inst.AgentConversation().HasID() {
			if retaken := inst.SessionCodexCaptureBaseline(); retaken.CapturesCodex() {
				snap = retaken
			}
		}
	}
}

// conversationCaptureArmed is the loop's gate: the row must still be the same
// tracked instance, still own the armed marker for THIS generation, and still
// be a live session. Failing any of those ends the loop — a replaced runtime,
// a row that died or was archived. A recorded conversation no longer does: the
// loop goes on following the runtime (#7101). Nor does a baseline whose store
// scan failed: the follow never reads it (#7120 review), and an unrecorded
// row's attempt skips the diff that would and asks the held query, which
// needs none (#7123), while the loop itself takes the baseline again on its
// next re-ask. Ending the loop on it would hand the row back to the poll,
// whose re-arm walks the store again for a fresh baseline every cycle.
func (m *Manager) conversationCaptureArmed(key string, inst *session.Instance, token session.AgentRuntimeToken) bool {
	if inst == nil || inst.UserKilled() || inst.IsArchived() {
		return false
	}
	if inst.AgentRuntimeToken() != token {
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
// guarantee: a live Codex row with no armed capture — the daemon restarted
// into it, or it predates capture — gets a fresh baseline and the same loop
// every launch gets. A recorded row is armed too, for the follow check
// (#7101). A late baseline is harmless now: capture also asks the runtime
// which rollout it holds open, so a file minted before this arm is still
// found. The expensive half (the baseline's store walk) runs outside m.mu;
// admission re-verifies the row is still tracked under the lock.
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
			// A kill, archive, or restore arms its own capture when the new
			// runtime exists; a baseline built now would bind the wrong store.
			// Swaps and handoffs never enter killsInFlight, so this pass can
			// arm their replacement mid-operation — which the held-open
			// answer makes safe (#7101).
			continue
		}
		// A recorded, killed, or archived row can never spend a parked
		// baseline, so the park is dropped: the check and the drop are one
		// atomic step so a respawn parking its baseline between them is never
		// destroyed (#6946). A recorded row still arms, for the follow check.
		inst.DropPendingConversationCaptureIfMoot()
		if inst.UserKilled() || inst.IsArchived() {
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
		// baseline on the instance (#6769 review): the replacement's rollout
		// can already exist, and a fresh before-image would exclude the file
		// it must find. The park is spent only inside an admitted arm —
		// every decline leaves it for the next tick — so the fallback
		// baseline is built here unconditionally and the park wins whenever
		// admission releases it (#6946).
		// A recorded row only needs the follow check, which walks no store:
		// bind its scope without a before-image, so the poll goroutine never
		// walks the store for it (#7120 review).
		var snap session.ConversationCaptureSnapshot
		if inst.AgentConversation().HasID() {
			snap = inst.SessionCodexCaptureScope()
		} else {
			snap = inst.SessionCodexCaptureBaseline()
		}
		repoID, _ := splitDaemonInstanceKey(key)
		m.mu.Lock()
		m.armDeferredConversationCaptureLocked(repoID, key, inst, snap, token)
		m.mu.Unlock()
	}
}

// armDeferredConversationCaptureLocked admits or declines one deferred arm
// under m.mu. The parked baseline is taken only here — atomically with the
// token liveness it is gated on — so every decline leaves it parked for the
// next tick: the row leaving m.instances, an arm already in flight for this
// generation, the caller's token going stale, or a baseline that never bound
// a Codex store. A park released and then unused goes back the same way
// (#6946). The declines that make the park permanently moot — a recorded,
// killed, or archived row — drop it explicitly instead; a recorded row then
// arms on the fallback baseline for its follow check (#7101). Callers must
// hold m.mu.
func (m *Manager) armDeferredConversationCaptureLocked(repoID, key string, inst *session.Instance, fallback session.ConversationCaptureSnapshot, token session.AgentRuntimeToken) {
	if m.instances[key] != inst {
		return
	}
	inst.DropPendingConversationCaptureIfMoot()
	if inst.UserKilled() || inst.IsArchived() {
		return
	}
	if armed, ok := m.armedConversationCaptures[inst]; ok && armed == token {
		return
	}
	park, current := inst.TakePendingConversationCaptureForRuntime(token)
	if !current {
		// The caller's token is already history: a swap or handoff arms the
		// replacement's capture inline, and a plain respawn parks its own
		// baseline for the generation that replaces this one.
		return
	}
	snap := fallback
	if park != nil {
		snap = park.Snapshot
	}
	if !snap.CapturesCodex() {
		inst.RestorePendingConversationCapture(park)
		return
	}
	if !m.startConversationCaptureLocked(repoID, key, inst, snap, token) {
		inst.RestorePendingConversationCapture(park)
	}
}

// unrecordedCaptureError is an unrecorded row's attempt failure as its owner
// needs to read it. When the cause is that af cannot tell which process is the
// agent, the consequence lands much later and says nothing: a restore of a row
// with no recorded conversation starts a fresh one without a refusal reason
// (#7124 review F1). So the one warning the loop logs names that consequence
// and the repair, which needs no launch evidence.
func unrecordedCaptureError(title string, err error) error {
	if !errors.Is(err, session.ErrNoConversationCaptureEvidence) {
		return err
	}
	return fmt.Errorf("%w. Until it can, restoring this session starts a fresh conversation; if you know which one it is in, record it with `%s <id>`",
		err, shellsuggest.PositionalCommand("af", []string{"sessions", "set-conversation"}, title))
}

// captureAgentConversation runs one capture attempt and commits any id it
// finds; on a row that already recorded one it runs the follow check instead
// (#7101). A non-nil return is the attempt's failure for the loop's
// rate-limited warning; ordinary absence (no rollout yet) returns nil —
// nothing wrong.
func (m *Manager) captureAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, timeout time.Duration) error {
	if inst == nil || inst.UserKilled() {
		return nil
	}
	if recorded := inst.AgentConversation(); recorded.HasID() {
		return m.followAgentConversation(repoID, key, inst, snap, token, recorded)
	}
	agent := token.Agent()
	if agent == "" {
		return nil
	}
	// Rollout adoption is attributed to the runtime this attempt commits
	// for: a new same-directory rollout is only a directory match until the
	// agent's own process generation provably holds it open — a same-cwd
	// sibling minted while the session waits unprompted is provably foreign
	// and stays unrecorded (#6847). The binding re-reads the launch evidence
	// on each judgement, so a snapshot armed before the launch recorded it
	// still attributes to that runtime once it has; without that evidence
	// nothing is attributed — never the active pane's thread (#7123).
	//
	// Evidence is settled first, once per attempt: an identity an unanswerable
	// reattach probe kept is re-verified here by the session-wide pane scan,
	// and with no verified agent process neither the diff nor the held query
	// can answer, so the attempt ends now — saying what that costs — rather
	// than polling the store for its whole window (#7123, #7124 review F1/F2).
	if err := inst.CheckConversationCaptureEvidence(); err != nil {
		return unrecordedCaptureError(inst.Title, err)
	}
	snap = inst.BindConversationCaptureRuntime(snap)
	var conv session.AgentConversationData
	var err error
	// The diff needs a before-image to compare with. A scope-only snapshot (a
	// loop armed to follow a row that has since lost its record) has none,
	// and a failed one can never answer: asking it would only re-read the
	// same error for the attempt's whole window and then return before the
	// held query below — which needs no baseline — got its turn (#7123). Both
	// go straight to the held query.
	baselineErr := snap.CodexBaselineErr()
	if !snap.BaselineFree() && baselineErr == nil {
		if conv, err = session.CaptureAgentConversation(agent, snap, timeout); err != nil {
			return err
		}
	}
	if !conv.HasID() {
		// Nothing NEW appeared — which a late baseline guarantees when the
		// runtime's rollout predates it: a daemon restart, a deferred arm that
		// claimed a swap's replacement generation after its mission minted the
		// file, a pane that `/resume`d an older thread (#7101). Ask the runtime
		// itself, once per attempt. This lives in the loop, not in
		// CaptureAgentConversation, so the swap's synchronous pre-delivery
		// capture keeps treating "no rollout yet" as the expected fresh start
		// rather than failing the handoff on a walk it does not need.
		if conv, err = session.HeldAgentConversation(agent, snap); err != nil {
			return unrecordedCaptureError(inst.Title, err)
		}
		if !conv.HasID() {
			if baselineErr != nil {
				// Say so once: until the agent holds a rollout open there is
				// nothing to record, and the store is broken either way.
				return fmt.Errorf("the Codex rollout store could not be scanned for a baseline, so af can record only a rollout the agent holds open: %w", baselineErr)
			}
			return nil
		}
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
	// An id recorded while this attempt polled the store — an operator's
	// set-conversation repair, a sibling capture's commit — stands: this
	// discovery is older evidence than the record and must not replace it
	// (#6769 review). The check sits inside the same op-lock the repair
	// takes, so either order is safe: a committed repair suppresses this
	// write, and a committed capture lets the repair overwrite it next.
	if inst.AgentConversation().HasID() {
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
