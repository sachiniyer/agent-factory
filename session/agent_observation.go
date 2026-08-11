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
// concrete runtime. It samples the operation axis and state epoch only AFTER it
// owns that runtime's observation lock, so a delivery that won the lock cannot
// advance the epoch and then have its post-delivery snapshot rejected with the
// older one. A target retired while this call waited is retried against its
// successor. A replacement during Snapshot is revalidated after transport I/O
// and retried too: callers perform transport-liveness side effects before some
// epoch-scoped applies, so returning a retired observation is not safe merely
// because its later state mutation would be rejected.
func (i *Instance) SnapshotAgent() (Observation, AgentServer, InFlightOp, uint64, error) {
	for {
		target := i.agentObservationTarget()
		target.runtime.mu.Lock()
		i.mu.RLock()
		current := i.agentObservation == target.runtime
		op, epoch := i.inFlightOp, i.stateEpoch
		i.mu.RUnlock()
		if !current {
			target.runtime.mu.Unlock()
			continue
		}
		if op != OpNone {
			target.runtime.mu.Unlock()
			return Observation{}, target.server, op, epoch, nil
		}
		obs, err := target.server.Snapshot()
		i.mu.RLock()
		current = i.agentObservation == target.runtime
		i.mu.RUnlock()
		target.runtime.mu.Unlock()
		if !current {
			continue
		}
		return obs, target.server, op, epoch, err
	}
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
