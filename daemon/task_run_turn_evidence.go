package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

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
	if !obs.Captured {
		return false
	}
	// An interrupted run whose agent resumed on its own (#6223) is watched from
	// the interruption instead of a prompt boundary; chrome re-arms the run.
	resumeFrom, resuming := time.Time{}, false
	if !instance.RunGateActive() {
		if resumeFrom, resuming = instance.TaskRunResumableSince(nowFunc()); !resuming {
			return false
		}
	}
	content := obs.Content
	// ResolvedAgent, not Program: the pane runs what the last handoff or
	// program override installed, and only that agent's chrome proves a turn.
	// Read under i.mu BEFORE pausedMu — the sweep takes the two separately and
	// never nests them.
	agent := instance.ResolvedAgent()
	boundary := instance.TaskRunPromptAttemptAt()
	if resuming {
		boundary = resumeFrom
	}
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
	turning := entry.watch.Observe(content, capturedAt)
	m.pausedMu.Unlock()
	if !turning {
		return false
	}
	if resuming {
		return instance.ResumeInterruptedTaskRunAtEpoch(nowFunc(), epoch)
	}
	return instance.RecordTaskRunTurnAtEpoch(nowFunc(), epoch)
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
		if inst.TaskRunObservable(nowFunc()) {
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
