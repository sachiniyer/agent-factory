package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// refreshInstanceStatusPublishingOutage runs one status refresh and publishes
// session.updated when it moved the unreachable flag (#5571 W1.3). Browsers
// follow the events plane, not periodic snapshots, and the flag can set, change
// its error, or clear with liveness untouched — so persistPollChange's
// liveness/projection gate would otherwise skip the publish and leave a
// connected client showing a stale outage (or none). A duplicate publish with a
// liveness change is harmless: the payload is a whole-row projection.
func (m *Manager) refreshInstanceStatusPublishingOutage(repoID string, instance *session.Instance) {
	if instance == nil {
		return
	}
	before := instance.ToInstanceData().Unreachable
	m.refreshInstanceStatus(repoID, instance)
	// Off-box tab bookkeeping rides the same per-instance sweep: pending closes,
	// uncertain creates and exit evidence all wait on the sandbox answering, and
	// this is where its reachability is already being probed (#5955, #5957).
	instance.ReconcileAgentTabID()
	tabsChanged := instance.ReconcileOffBoxTabs()
	after := instance.ToInstanceData().Unreachable
	if (unreachableDataEqual(before, after) && !tabsChanged) || instance.GetInFlightOp() != session.OpNone {
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	// Re-read under the lock, same ordering domain as the poll's own publish, so
	// the last event on the wire is the newest state.
	data := instance.ToInstanceData()
	if data.InFlightOp != session.OpNone {
		return
	}
	// The refresh above can be what finishes a kill, and a kill can commit while
	// it runs. A session.updated published after session.killed makes a thin
	// client upsert the row back from a whole-row payload (#5723). The registry
	// check and the publish share m.mu, and every kill removes its map entry under
	// m.mu before it announces session.killed — so either this publish precedes
	// the removal (and so the killed event) or the check sees the row gone.
	// publishEvent never blocks and never takes m.mu.
	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[key] != instance || instance.UserKilled() {
		return
	}
	if tabsChanged {
		// A reconciled tab change (an adopted create, a retired close, a recorded
		// exit) has no other writer: persist it with the publish so a stable
		// session neither shows stale tabs to event-driven clients nor loses the
		// change across a restart. Best-effort — the next write carries it too.
		if err := persistInstanceData(repoID, data); err != nil {
			m.warn().Printf("off-box tab reconciliation for %q could not be persisted: %v", instance.Title, err)
		}
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
}

func unreachableDataEqual(a, b *session.UnreachableData) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Since.Equal(b.Since) && a.Error == b.Error
}

// markUnreachableIfCurrent raises the outage flag for an unanswered probe of the
// CURRENT runtime (#5571 W1.3). The not-provisioned sentinel is definitive
// absence — af knows there is nothing to reach — so it is not an outage.
func markUnreachableIfCurrent(instance *session.Instance, generation session.AgentObservationGeneration, err error) {
	if instance.AgentObservationCurrent(generation) && !errors.Is(err, session.ErrRemoteSandboxNotProvisioned) {
		instance.SetUnreachable(err)
	}
}
