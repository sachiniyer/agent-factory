package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// taskRunTurnWatchEntry keys a run's chrome watcher by the evidence it may
// legitimately observe: the resolved agent (a mid-run handoff swaps the pane's
// signatures) and the task-prompt boundary (a redelivery discards primed rows
// that predate it, so an already-running turn cannot release the fresh window
// as though it were the new prompt's — #5221 review).
type taskRunTurnWatchEntry struct {
	agent    string
	boundary time.Time
	watch    *task.TurnWatch
}

// noteTaskRunTurnEvidence feeds this tick's pane capture through the run's
// chrome watcher and, when the agent's own in-turn chrome is demonstrated,
// stamps the run's turn boundary (#5219). Boot output, the prompt's own echo,
// and ACP init all arrive as ordinary churn — only the chrome proves the agent
// took the turn, so only it (or the fallback's sustained quiet) may release a
// held idle edge. Returns true when the boundary was newly recorded, so the
// caller can force the row durable even while the session stays Running.
//
// The watcher state lives under pausedMu — the paused-path observation feeds
// the same map — while the evidence write follows the observation epoch fence,
// same as RecordPaneChurnCheckpointAtEpoch above.
func (m *Manager) noteTaskRunTurnEvidence(key string, instance *session.Instance, obs session.Observation, capturedAt time.Time, epoch uint64) bool {
	// Only a confirmed frame feeds the watch: an unanswered capture
	// (obs.Captured false — a local capture-pane failure the backend swallows
	// into an all-false frame) carries no pane evidence at all. A confirmed
	// EMPTY frame does feed it — a scoped agent needs an indicator-absent
	// capture before a present one can prove the turn, and for the timed
	// agents an empty frame merely clears the row baseline (#5221 review).
	if !instance.TaskRunActive() || !obs.Captured {
		return false
	}
	content := obs.Content
	// ResolvedAgent, not Program: the pane runs what the last handoff or
	// program override installed, and only that agent's chrome proves a turn.
	// Read under i.mu BEFORE pausedMu — the sweep takes the two separately and
	// never nests them.
	agent := instance.ResolvedAgent()
	boundary := instance.TaskRunPromptAttemptAt()
	stillProven := !instance.TaskRunStillBaseAt().IsZero()
	// The frame must predate nothing: if a redelivery stamped a boundary
	// AFTER this capture started, the pane in `content` never saw the new
	// prompt — feeding it now would arm a fresh watcher off a pre-boundary
	// frame (an indicator-absent capture primes the scoped agents' absent→
	// present contract), after which unrelated pre-boundary chrome can pass
	// as the redelivery's turn. The epoch fence below guards the write; this
	// timestamp fence guards the watcher's mutation itself (#5221 review).
	if boundary.After(capturedAt) {
		return false
	}
	// The sampled epoch must still be current: a lifecycle transition after
	// the capture started (a send, an archive begin — even one canceled
	// since) means this frame depicts a state the instance already exited.
	// The epoch-fenced instance writes would reject it, but the watcher is
	// process-local and unfenced — feeding it could arm a scoped absence or
	// seed a timed baseline the NEXT current capture pairs against, putting
	// pre-boundary chrome on the new window (#5221 review). Read under i.mu
	// BEFORE pausedMu, same as the accessors above.
	if _, currentEpoch := instance.InFlightOpAndEpoch(); currentEpoch != epoch {
		return false
	}
	m.pausedMu.Lock()
	entry, ok := m.taskRunTurnWatches[key]
	if !ok || entry.agent != agent || !entry.boundary.Equal(boundary) {
		// A mid-run handoff swaps the signatures the pane can produce, and a
		// redelivery retires primed state from the previous window — either way
		// the watcher must be rebuilt rather than read the pane through rules
		// or a prev frame that no longer belong to this boundary (#5221 review).
		entry = &taskRunTurnWatchEntry{agent: agent, boundary: boundary, watch: task.NewTurnWatch(agent, boundary)}
		m.taskRunTurnWatches[key] = entry
	}
	// A boundary carrying stillness proof — seeded at the send behind
	// WaitForReady, or latched by a still run since — already observed
	// the pane not in a turn: priming keeps a turn that began before
	// the first daemon capture from being read as stale chrome (scoped)
	// or from failing the elapsed fit on every tick (timed) (#5221
	// review). A contended boundary proves nothing, so its watcher
	// still owes the absence grace and the fit — until the base latches
	// under it, which can happen AFTER the watch exists; the proof
	// applies to the live entry too, and priming is idempotent.
	if stillProven {
		entry.watch.PrimeProvenIdle()
	}
	turning := entry.watch.Observe(content, capturedAt, nowFunc())
	m.pausedMu.Unlock()
	if !turning {
		return false
	}
	return instance.RecordTaskRunTurnAtEpoch(nowFunc(), epoch)
}

// dropTaskRunTurnWatch invalidates a run's chrome watcher on a blind window
// (#5221 review): a failed capture or a snapshot transport error leaves the
// pane unobserved, and a watcher kept across it would count the outage
// toward proofs that need CONTINUOUS sight — the scoped agents' absence
// grace armed by frames that bracket the hole, or a timed agent's prev row
// pairing across it. The next confirmed frame rebuilds the watch from
// scratch inside noteTaskRunTurnEvidence.
func (m *Manager) dropTaskRunTurnWatch(key string) {
	m.pausedMu.Lock()
	delete(m.taskRunTurnWatches, key)
	m.pausedMu.Unlock()
}

// sweepTaskRunTurnWatches drops per-run chrome watchers whose run is no longer
// in flight — the watcher exists only to fence the run's completion edge, so a
// finished run or a torn-down row never needs it again. The map is keyed by
// stableSessionKey, so a same-title replacement session cannot inherit the old
// run's partially-fed watcher either. Called once per poll from
// RefreshStatuses, beside sweepRemoteLossStates; m.mu and pausedMu are taken
// separately, never nested, per pausedPolls' lock discipline.
func (m *Manager) sweepTaskRunTurnWatches() {
	m.pausedMu.Lock()
	size := len(m.taskRunTurnWatches)
	m.pausedMu.Unlock()
	if size == 0 {
		return // nothing armed: skip the instance walk in the common case
	}
	m.mu.Lock()
	live := make(map[string]struct{}, size)
	for key, inst := range m.instances {
		if inst.TaskRunActive() {
			repoID, _ := splitDaemonInstanceKey(key)
			live[stableSessionKey(repoID, inst)] = struct{}{}
		}
	}
	m.mu.Unlock()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	for key := range m.taskRunTurnWatches {
		if _, ok := live[key]; !ok {
			delete(m.taskRunTurnWatches, key)
		}
	}
}
