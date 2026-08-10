package session

import (
	"sync"
	"time"
)

type agentObservationRuntime struct {
	mu sync.Mutex
}

type agentObservationTarget struct {
	server  AgentServer
	runtime *agentObservationRuntime
}

// agentObservationTarget binds the server and its observation lock in the same
// i.mu section. Runtime replacement clears i.agentObservation alongside the
// derived server cache, so predecessor I/O and replacement I/O never share a
// lock merely because the Instance pointer survived the swap.
func (i *Instance) agentObservationTarget() agentObservationTarget {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agentObservation == nil {
		i.agentObservation = &agentObservationRuntime{}
	}
	return agentObservationTarget{
		server:  i.agentServerLocked(),
		runtime: i.agentObservation,
	}
}

// SnapshotAgent serializes pane observation with prompt delivery for one
// concrete runtime. The returned server is the one that supplied the snapshot;
// callers use it for any follow-up liveness probe governed by the same epoch.
func (i *Instance) SnapshotAgent() (Observation, AgentServer, error) {
	target := i.agentObservationTarget()
	target.runtime.mu.Lock()
	defer target.runtime.mu.Unlock()
	obs, err := target.server.Snapshot()
	return obs, target.server, err
}

// SendPromptWithEvidence starts the attempt timestamp only after any pane
// snapshot already in flight has completed, then records the delivery verdict
// before another snapshot can begin.
func (i *Instance) SendPromptWithEvidence(prompt string, now func() time.Time) (PromptDeliveryStatus, error) {
	for {
		target := i.agentObservationTarget()
		target.runtime.mu.Lock()
		i.mu.RLock()
		current := i.agentObservation == target.runtime
		i.mu.RUnlock()
		if !current {
			target.runtime.mu.Unlock()
			continue
		}
		attemptedAt := now()
		status, err := SendPromptWithStatus(target.server, prompt)
		i.recordPromptAttemptForObservation(status, attemptedAt, target.runtime)
		target.runtime.mu.Unlock()
		return status, err
	}
}
