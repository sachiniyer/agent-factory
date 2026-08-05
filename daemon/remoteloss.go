package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Remote-loss debounce (#1794, hardening the #1782 fix).
//
// A remote session is observed over REST to its in-sandbox agent-server, so
// every probe carries the transport's failure modes: a dropped ssh forward, a
// docker-proxy hiccup, a NAT rebind, a blackholed route. A probe that dies to
// any of those tells you NOTHING about the agent — but it used to look exactly
// like a dead sandbox, because Snapshot() and Alive() both merely "failed".
//
// That conflation is dangerous because of what sits downstream. The poll loop
// runs RefreshStatuses then RestoreLostSessions in the SAME tick, and
// docker/ssh/hook backends all advertise Capabilities().Recover — but their
// Recover is not a reconnect. It is recoverSandbox: provision a BRAND-NEW
// sandbox and clone the branch back from origin. So a remote row that flips Lost
// gets a fresh sandbox milliseconds later, the original sandbox keeps running
// unreferenced, and every commit it never pushed is gone. A single blip must
// therefore never be sufficient to mark a remote session Lost (#1794).
//
// The fix is in two layers, and the ORDER of them matters:
//
//  1. DISTINGUISH. AgentServer.Alive returns an error, so "the sandbox answered:
//     the agent is gone" and "the sandbox never answered" stop being the same
//     `false`. See livenessProbe: an ANSWERED probe is authoritative and acted on at
//     once (#935/#1108 immediacy, unchanged); probeUnknown is an absence of
//     evidence and settles nothing on its own. Most of the safety lives here —
//     no amount of debouncing rescues a caller that cannot tell the two apart,
//     which is why the limit-resume path needed the distinction rather than a
//     debounce of its own.
//  2. DEBOUNCE before a second probe. A run of failed Snapshots earns one bounded
//     Alive confirmation, but never manufactures death from silence. Only an
//     an ANSWERED death may settle Lost; probeUnknown preserves last-known state
//     because unreachable is not dead (#2589).
//
// The debounce demands the failure be DURABLE on two independent axes:
//
//   - COUNT (remoteLostFailureThreshold): N consecutive unanswered probes, so
//     one bad round-trip is absorbed.
//   - TIME (remoteLostGracePeriod): the failures must also span a real
//     wall-clock window. Count alone is not enough — a fast-failing blip
//     (ECONNREFUSED returns instantly while a forward re-establishes) can burn
//     N ticks in a couple of seconds, which is exactly the blip this is meant
//     to survive.
//
// Both axes must be satisfied before the confirmation call. If that call is also
// unanswerable, the episode resets and later failures may try again; no number of
// connectivity failures can authorize replacing a possibly-live sandbox.
var (
	// remoteLostFailureThreshold is the consecutive failed-Snapshot count required
	// before the daemon pays for a separate Alive confirmation. var, not const, so
	// tests can drive the threshold directly.
	remoteLostFailureThreshold = 3
	// remoteLostGracePeriod is the minimum wall-clock span the failures must
	// persist before the confirmation, independent of how many ticks fit in it.
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

// stableSessionKey is the daemon's key for any state that describes ONE
// session's runtime rather than one repo/title slot: the remote-loss debounce
// below, aliveObservations, the Lost-restore retry episode, handoff retry/settle
// bookkeeping, the paused task-run backstop.
//
// It keys on the STABLE INSTANCE ID (#1195), not the repo/title daemon key, so
// failure state can never outlive the session that earned it. Titles are reused:
// kill or archive a session and create another with the same title, and a
// title-keyed entry would hand the NEW session the OLD one's accumulated
// failures. For the debounce that means one blip satisfying the threshold
// instantly and re-provisioning a sandbox that had never failed a probe in its
// life; for the Lost-restore episode it means a brand-new session waiting
// minutes for its first restore because a session the user already discarded
// failed four times (#2868). That is the key-by-title-instead-of-identity class
// from #1723/#1678/#1738, and the ID is exactly the field #1195 minted to tell
// "same session" from "title reused".
//
// The ID is what makes a title reuse a DIFFERENT key while a re-provision of the
// same session keeps the SAME one — which is the distinction every caller here
// wants, and why the sites that replace a runtime under one identity must retire
// their own state explicitly (see noteRuntimeReplaced).
//
// THE RULE, because this keeps being got wrong one map at a time (#1723, #1678,
// #1738, #1794, #2868, #2876). Before adding any per-session map to Manager, ask
// what the entry DESCRIBES:
//
//   - A RUNTIME — failures it accumulated, observations of it, work owed to it,
//     a process it owns: key it here. A title is a slot that outlives its
//     occupant, so a title-keyed entry is inherited by the next session to take
//     that name, which is a bug every single time.
//   - A SLOT — "an exclusive operation is running against this name", "this name
//     is reserved", the lock that serializes ops on it: key it by
//     daemonInstanceKey. Being shared with a successor is the entire point, and
//     these are point-in-time within one operation anyway.
//
// If a map is genuinely slot-shaped but its VALUE describes a runtime, the third
// option is to store the stable id alongside and compare before acting —
// deferredTaskLifecycle and vscodeSupervisor.servers both do this deliberately,
// and killRetries instead drops its entry at the very events that free the title.
// What is never safe is a title key, a runtime-shaped value, and no guard.
//
// Legacy records persisted before #1195 carry no ID; they fall back to the
// daemon key. Nothing materializes an ID-less Instance — FromInstanceData
// backfills one in memory for exactly this reason, and the daemon load path
// backfills it durably — so the fallback is a belt-and-braces path, not a live
// one.
func stableSessionKey(repoID string, instance *session.Instance) string {
	if instance.ID != "" {
		return instance.ID
	}
	return daemonInstanceKey(repoID, instance.Title)
}

// isRemoteWorkspace reports whether an instance's workspace lives off-box, so
// its probes cross a network and can fail transiently. Branching on the
// capability rather than the backend's name is the #1592 Phase 1 contract: a new
// off-box backend inherits the debounce by declaring what it is, not by being
// added to a list here.
func isRemoteWorkspace(instance *session.Instance) bool {
	return instance.Capabilities().Workspace == session.WorkspaceRemote
}

// noteRemoteProbeFailure records one UNANSWERABLE remote probe and reports
// whether the loss now looks DURABLE — N consecutive unanswered probes spanning
// at least the grace period. Until both hold the caller must leave the session's
// liveness alone and let the next tick decide.
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

// clearRemoteLoss drops a session's debounce state. Callers must have ANSWERED
// evidence — a completed probe, or a lifecycle event that replaced the runtime
// the old failures were about. The next episode then starts from zero rather
// than inheriting a stale count that could tip a later single blip straight over
// the threshold.
func (m *Manager) clearRemoteLoss(key string) {
	m.mu.Lock()
	delete(m.remoteLossStates, key)
	m.mu.Unlock()
}

// remoteSandboxLiveness keeps all three outcomes of the final pre-recovery probe.
//
// A Lost mark is a claim about the past, and it can be minutes stale: restore
// backs off to 5 minutes, and a user can hit restore by hand at any moment.
// Transports heal. Re-provisioning is irreversible and destroys everything the
// sandbox never pushed, so the question that matters is not "was it lost?" but
// "is it gone NOW?" — asked against live state, immediately before acting.
//
// probeAlive proves the sandbox is there and heals the stale Lost row. An answered death
// is session-specific evidence that replacement is safe: either the server
// answered false or af's clientless sentinel says this runtime is explicitly not
// provisioned. probeUnknown authorizes nothing: unreachable is not dead, and
// collapsing it into a death is exactly how a connectivity failure can discard
// unpushed work (#2589).
//
// Bounded, so a wedged remote cannot stall the poll goroutine here either.
func (m *Manager) remoteSandboxLiveness(instance *session.Instance) livenessProbe {
	if !isRemoteWorkspace(instance) {
		return probeAbsent // a local session has no remote sandbox to preserve
	}
	return aliveWithin(instance.AgentServer(), remoteLostConfirmTimeout)
}

// noteRuntimeReplaced retires observations owned by the prior runtime after a
// lifecycle event that REPLACED it: a Recover, a Respawn, an archive-restore's
// re-provision. Today those are the remote-loss debounce and the live model
// diagnostic.
//
// Call it the INSTANT the swap succeeds — before any persist, log, or other
// work. The poll goroutine does not hold the op-lock and does not skip a session
// whose restore has already cleared OpRestoring, so it can probe the fresh
// runtime while the caller is still doing its bookkeeping. A blip inside that
// window would be judged against the OLD sandbox's threshold-satisfying count
// and mark the new runtime Lost. The window is milliseconds, but closing it
// costs nothing but statement order.
//
// EVERY such site must call this, and the requirement is not obvious, so it is
// named for the trigger rather than the effect. The accumulated count describes
// a sandbox that no longer exists. Left behind it stays threshold-satisfying and
// its firstFailureAt stays long past the grace period, so the very first
// transport blip against the NEW sandbox re-satisfies the debounce instantly and
// re-provisions AGAIN — orphaning the sandbox just built, and stranding another
// live container on every cycle. The debounce is defeated precisely where it
// matters most.
//
// Note the sweep cannot cover this: a re-provision keeps the SAME instance ID
// (that is the point — same session, new sandbox), so the entry stays "live".
// Only the site that replaced the runtime knows it happened.
//
// Missing the call fails silently in production and is caught only by the
// "post-<trigger> blip does not re-provision again" tests. Known sites:
// lostrestore.go (automatic Recover), restore.go (manual Recover RPC),
// limit.go (limit-resume Respawn), archive.go (archive-restore re-provision).
func (m *Manager) noteRuntimeReplaced(repoID string, instance *session.Instance) {
	// Model diagnostics are observations of one concrete agent process, just like
	// the remote-loss episode is an observation of one concrete sandbox. Every
	// recovery/re-provision/handoff already crosses this chokepoint, so retire both
	// predecessor-owned facts together rather than making each caller remember a
	// second reset site.
	instance.ClearAgentModelChange()
	m.clearRemoteLoss(stableSessionKey(repoID, instance))
}

// sweepRemoteLossStates drops debounce entries that no longer describe anything
// probeable — killed, archived, or replaced by a same-title session with a fresh
// ID. Keying by instance ID already stops a new session from READING an old
// one's failures; this is what stops the map from growing, since a session
// killed or archived mid-episode never gets the answered probe that would clear
// it. Called from the poll that populates the map. Guarded by m.mu.
//
// "Probeable", not merely "present in m.instances": an ARCHIVED session stays in
// that map under the same stable ID, so presence alone would keep its entry
// forever. Archiving a remote session tears its sandbox down (Archive pushes the
// branch, kills the in-sandbox workspace, and resets the remote wiring), which
// makes the accumulated count definitionally stale — it describes a sandbox that
// was deliberately destroyed. The poll never probes an archived row either, so
// nothing would ever clear it. Hence: unstarted or Archived is not live.
//
// This cannot substitute for noteRuntimeReplaced. A re-provision keeps the same
// ID AND stays started, so the entry legitimately looks live — only the site
// that swapped the runtime can know it did.
func (m *Manager) sweepRemoteLossStates() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.remoteLossStates) == 0 {
		return
	}
	live := make(map[string]struct{}, len(m.instances))
	for key, inst := range m.instances {
		if !inst.Started() || inst.GetLiveness() == session.LiveArchived {
			continue // nothing to probe: its entry can never be cleared by an answer
		}
		repoID, _ := splitDaemonInstanceKey(key)
		live[stableSessionKey(repoID, inst)] = struct{}{}
	}
	for key := range m.remoteLossStates {
		if _, ok := live[key]; !ok {
			delete(m.remoteLossStates, key)
		}
	}
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
func (m *Manager) settleRemoteProbeFailure(repoID, key string, instance *session.Instance, before session.Liveness, beforeReset time.Time, epoch uint64) {
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
	// Durable unreachability. Confirm with the INDEPENDENT Alive() probe before
	// transitioning: Snapshot can fail on its own (a capture error from a server
	// that is up and answering), and Lost is the trigger for a destructive
	// re-provision, so it is worth one bounded round-trip to be sure.
	switch probe := aliveWithin(instance.AgentServer(), remoteLostConfirmTimeout); probe {
	case probeAlive:
		log.InfoLog.Printf("remote session %q failed repeated snapshots but its agent-server answers as alive; leaving its status alone", instance.Title)
		m.clearRemoteLoss(key)
		return
	case probeAbsent, probeAnsweredDead:
		// The server answered: reachable, agent gone. The snapshots were failing
		// for some other reason. An answer of any kind ends the transport episode.
		log.WarningLog.Printf("remote session %q: agent-server reports its agent is gone — marking it Lost", instance.Title)
		m.clearRemoteLoss(key)
	case probeUnknown:
		log.WarningLog.Printf("remote session %q: agent-server unreachable and still unanswerable after %s — leaving its last-known status unchanged; unreachable is not evidence that this sandbox is gone", instance.Title, remoteLostGracePeriod)
		m.clearRemoteLoss(key)
		return
	}
	_ = instance.Transition(session.ObserveLiveness(session.LiveLost).AtEpoch(epoch))
	m.persistPollChange(repoID, instance, before, beforeReset, false)
}

// livenessProbe is the outcome of a liveness probe. It is a tri-state, not a
// bool, because the third case is the one that matters: a probe that never
// answered tells you nothing about the agent, and treating it as death is what
// re-provisions over a live sandbox (#1794).
type livenessProbe int

const (
	// probeAlive: the agent answered and is running.
	probeAlive livenessProbe = iota
	// probeAbsent: af has an explicit not-provisioned sentinel for this session —
	// the clientless AgentServer, which is raised when af KNOWS there is no runtime
	// to contact. Nothing is preserved by refusing, so this licenses a reap.
	probeAbsent
	// probeAnsweredDead: the agent-server ANSWERED and reports its agent gone. That
	// answer proves the sandbox is REACHABLE — a live container whose agent process
	// crashed, OOMed, or exited — so whatever it holds is still there to be saved.
	// It is death evidence for the AGENT and liveness evidence for the SANDBOX, and
	// keeping those apart is the whole point of the split: collapsing it into
	// "absent" is what let recovery reap a reachable sandbox with unpushed commits
	// in it (#2923).
	probeAnsweredDead
	// probeUnknown: the probe could not be answered — a transport failure or a
	// timeout. NOT evidence of death. Only repetition over time (the debounce)
	// may turn a run of these into a conclusion.
	probeUnknown
)

// probeLiveness runs one liveness probe for an instance, bounding the wait for
// REMOTE instances only. A local probe is in-process (no network to hang on) and
// its Alive never errors, so it answers directly; wrapping it would spend a
// goroutine and a timer per idle session per tick for nothing.
func probeLiveness(instance *session.Instance, as session.AgentServer) livenessProbe {
	if isRemoteWorkspace(instance) {
		return aliveWithin(as, remoteLostConfirmTimeout)
	}
	alive, err := as.Alive()
	if err != nil {
		return probeUnknown
	}
	if alive {
		return probeAlive
	}
	// A local probe is in-process, so an answer of "not alive" is not evidence
	// about any sandbox — there is none to reach.
	return probeAbsent
}

// aliveWithin runs a possibly-slow remote Alive() probe under a hard local
// deadline, reporting probeUnknown if it cannot answer in time.
//
// It bounds the CALLER's wait, not the underlying REST call — AgentServer.Alive
// takes no context, and threading one through the whole interface to serve this
// one probe is not worth the blast radius. The orphaned goroutine finishes on
// its own within remoteAgentCallTimeout and writes to a BUFFERED channel, so it
// never blocks and nothing leaks past that; at most one such goroutine exists
// per wedged remote per tick.
//
// A timeout reports probeUnknown rather than a death: a server that cannot
// answer in time has told us nothing, and the whole point of the tri-state is
// that we stop inventing a verdict from silence.
func aliveWithin(as session.AgentServer, timeout time.Duration) livenessProbe {
	type result struct {
		alive bool
		err   error
	}
	done := make(chan result, 1) // buffered: the goroutine never blocks, even if we gave up
	go func() {
		alive, err := as.Alive()
		done <- result{alive: alive, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		switch {
		case errors.Is(r.err, session.ErrRemoteSandboxNotProvisioned):
			// af's own sentinel: it KNOWS there is no runtime to contact, so there is
			// nothing to preserve. Note this is the ONLY absence evidence — a
			// genuinely destroyed container fails with a TRANSPORT error, which lands
			// on probeUnknown below. Keying a refusal off this sentinel instead of off
			// reachability would strand every truly-gone sandbox forever.
			return probeAbsent
		case r.err != nil:
			return probeUnknown
		case r.alive:
			return probeAlive
		default:
			// It answered. The agent is gone but the sandbox is THERE.
			return probeAnsweredDead
		}
	case <-timer.C:
		return probeUnknown
	}
}

// noteAliveObservation records that a poll got an ANSWER out of this session's
// runtime — the daemon's only positive proof of life.
//
// The Lost-restore loop clears a session's failure history only when this counter
// has advanced past the value captured at its last respawn. That is what makes
// "confirmed alive" an observation rather than a clock (#1917 round 6): a fixed
// settle window is wrong at both ends — a daemon_poll_interval longer than the
// window means a runtime that died instantly is only SEEN Lost after it expires
// (so its history is wrongly cleared and the backoff never arms), and the 60s
// remoteLostGracePeriod means an unanswerable remote stays non-Lost long past any
// short window. Counting answers is immune to both, and scales with whatever the
// poll interval and grace actually are.
func (m *Manager) noteAliveObservation(key string) {
	m.mu.Lock()
	m.aliveObservations[key]++
	m.mu.Unlock()
}
