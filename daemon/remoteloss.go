package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Remote-loss debounce (#1794, hardening the #1782 fix).
//
// A remote session is observed over REST to its in-sandbox agent-server, so
// every probe carries the transport's failure modes: a dropped ssh forward, a
// docker-proxy hiccup, a NAT rebind, a blackholed route. NONE of those are
// distinguishable from a dead sandbox at the call site — Snapshot() and Alive()
// both just fail.
//
// That indistinguishability is dangerous because of what sits downstream. The
// poll loop runs RefreshStatuses then RestoreLostSessions in the SAME tick, and
// docker/ssh/hook backends all advertise Capabilities().Recover — but their
// Recover is not a reconnect. It is recoverSandbox: provision a BRAND-NEW
// sandbox and clone the branch back from origin. So a remote row that flips Lost
// gets a fresh sandbox milliseconds later, the original sandbox keeps running
// unreferenced, and every commit it never pushed is gone. A single blip must
// therefore never be sufficient to mark a remote session Lost (#1794).
//
// The debounce demands the failure be DURABLE on two independent axes before the
// poll will settle Lost:
//
//   - COUNT (remoteLostFailureThreshold): N consecutive failed probes, so one
//     bad round-trip is absorbed.
//   - TIME (remoteLostGracePeriod): the failures must also span a real
//     wall-clock window. Count alone is not enough — a fast-failing blip
//     (ECONNREFUSED returns instantly while a forward re-establishes) can burn
//     N ticks in a couple of seconds, which is exactly the blip this is meant
//     to survive.
//
// The counter tracks consecutive observations that found NO EVIDENCE OF LIFE —
// not merely consecutive transport errors. Evidence of life (fresh output, a
// waiting prompt, an Alive() that answers true) clears it; a Snapshot that
// merely completes does not, because an agent-server can keep serving clean idle
// snapshots long after the agent inside it died. See the clear sites in
// refreshInstanceStatus.
// Both axes must be satisfied, so whichever is slower under the operator's
// daemon_poll_interval governs — which is the point of having both. At the 1s
// default the grace period dominates (a durably dead remote surfaces as Lost
// after ~60s rather than ~3s); at a coarse interval the count does. Either way
// the ceiling on how long a genuinely dead remote reads healthy is bounded and
// small, which is the #1782 guarantee — 60s of stale green is a fair price for
// never re-provisioning over a live sandbox.
var (
	// remoteLostFailureThreshold is the consecutive failed-probe count required
	// before a remote session may be marked Lost. var, not const, so tests can
	// drive the threshold directly.
	remoteLostFailureThreshold = 3
	// remoteLostGracePeriod is the minimum wall-clock span the failures must
	// persist before Lost is allowed, independent of how many ticks fit in it.
	remoteLostGracePeriod = 60 * time.Second
	// remoteLostConfirmTimeout bounds the confirmation Alive() probe. It is
	// deliberately SHORT and unrelated to remoteAgentCallTimeout (30s): the
	// failed Snapshot that got us here already spent the full call timeout, and
	// RefreshStatuses walks instances SERIALLY, so an unbounded second probe
	// would let one blackholed sandbox add ~2x30s to every poll — stalling
	// status, root-ensure, lost-restore and limit-resume for EVERY other
	// session (#1794). The confirmation only has to break a tie the debounce
	// has already all but settled, so it gets a tight budget.
	remoteLostConfirmTimeout = 5 * time.Second
)

// remoteLossState is the per-session debounce state. Guarded by Manager.mu.
type remoteLossState struct {
	consecutiveFailures int
	firstFailureAt      time.Time
}

// isRemoteWorkspace reports whether an instance's workspace lives off-box, so
// its probes cross a network and can fail transiently. Branching on the
// capability rather than the backend's name is the #1592 Phase 1 contract: a new
// off-box backend inherits the debounce by declaring what it is, not by being
// added to a list here.
func isRemoteWorkspace(instance *session.Instance) bool {
	return instance.Capabilities().Workspace == session.WorkspaceRemote
}

// noteRemoteProbeFailure records one failed remote probe and reports whether the
// loss now looks DURABLE — N consecutive failures spanning at least the grace
// period. Until both hold the caller must leave the session's liveness alone and
// let the next tick decide.
func (m *Manager) noteRemoteProbeFailure(key, title string) bool {
	now := nowFunc()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.remoteLossStates[key]
	if st == nil {
		st = &remoteLossState{firstFailureAt: now}
		m.remoteLossStates[key] = st
	}
	st.consecutiveFailures++
	if st.consecutiveFailures < remoteLostFailureThreshold {
		return false
	}
	if now.Sub(st.firstFailureAt) < remoteLostGracePeriod {
		return false
	}
	log.WarningLog.Printf("remote session %q has failed %d consecutive agent-server probes over %s; confirming before marking it Lost", title, st.consecutiveFailures, now.Sub(st.firstFailureAt).Round(time.Second))
	return true
}

// clearRemoteLoss drops a session's debounce state once the poll has positive
// evidence the agent is alive. The next episode then starts from zero rather
// than inheriting a stale count that could tip a later single blip straight over
// the threshold. Callers must hold evidence of LIFE, not just of reachability —
// see the switch in refreshInstanceStatus.
func (m *Manager) clearRemoteLoss(key string) {
	m.mu.Lock()
	delete(m.remoteLossStates, key)
	m.mu.Unlock()
}

// settleRemoteProbeFailure decides what a failed observation probe means for one
// instance and writes the outcome. It is the Snapshot-error arm of
// refreshInstanceStatus, kept here beside the debounce it drives.
//
// The order matters. The DEBOUNCE runs first and the confirmation probe only
// afterwards, so the common transient case — the one this exists for — costs
// exactly zero extra REST calls, and the confirmation is paid once per episode
// rather than once per tick. That is also what keeps a wedged sandbox from
// double-stalling the serial poll: it can no longer spend a full 30s Snapshot
// AND a full 30s Alive on every single tick (#1794).
func (m *Manager) settleRemoteProbeFailure(repoID, key string, instance *session.Instance, before session.Liveness, beforeReset time.Time) {
	if !isRemoteWorkspace(instance) {
		// A local agent-server's Snapshot never errors; if that ever changes,
		// an unexplained local error is not evidence of death. Leave it be.
		return
	}
	if before == session.LiveLost {
		// Already settled where a failing probe would put it. There is nothing to
		// transition, so don't pay the confirmation round-trip (or re-log the
		// threshold) on every tick of an outage. The way back is a SUCCESSFUL
		// Snapshot, which clears the episode in refreshInstanceStatus and lets the
		// normal branches settle the real liveness.
		return
	}
	if !m.noteRemoteProbeFailure(key, instance.Title) {
		return // not durable yet — the next tick decides
	}
	// Durable failure. Confirm with the INDEPENDENT Alive() probe before the
	// transition: Snapshot can fail on its own (a capture error from a server
	// that is up and answering), and Lost is the trigger for a destructive
	// re-provision, so it is worth one bounded round-trip to be sure.
	if aliveWithin(instance.AgentServer(), remoteLostConfirmTimeout) {
		log.InfoLog.Printf("remote session %q failed repeated snapshots but its agent-server answers as alive; leaving its status alone", instance.Title)
		m.clearRemoteLoss(key)
		return
	}
	log.WarningLog.Printf("remote session %q: agent-server unreachable and confirmed not alive — marking it Lost", instance.Title)
	_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
	m.persistPollChange(repoID, instance, before, beforeReset)
}

// aliveWithin runs a possibly-slow Alive() probe under a hard local deadline,
// returning false if it cannot answer in time.
//
// It bounds the CALLER's wait, not the underlying REST call — AgentServer.Alive
// takes no context, and threading one through the whole interface to serve this
// one probe is not worth the blast radius. The orphaned goroutine finishes on
// its own within remoteAgentCallTimeout and writes to a BUFFERED channel, so it
// never blocks and nothing leaks past that; at most one such goroutine exists
// per wedged remote per tick.
//
// Timing out reports not-alive, which is correct in position: every caller has
// already established durable failure, so an agent-server that cannot answer a
// short liveness ping on top of that is unreachable by any useful definition.
func aliveWithin(as session.AgentServer, timeout time.Duration) bool {
	result := make(chan bool, 1)
	go func() { result <- as.Alive() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case alive := <-result:
		return alive
	case <-timer.C:
		return false
	}
}
