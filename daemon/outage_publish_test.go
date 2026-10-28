package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

func drainUpdatedFor(events <-chan agentproto.Event, title string) int {
	n := 0
	for {
		select {
		case ev := <-events:
			if ev.Type != agentproto.EventSessionUpdated {
				continue
			}
			var payload session.InstanceData
			if err := json.Unmarshal(ev.Data, &payload); err == nil && payload.Title == title {
				n++
			}
		case <-time.After(100 * time.Millisecond):
			return n
		}
	}
}

// The outage publisher must not announce a row the daemon no longer holds
// (#5723): a kill that commits while the refresh runs removes the map entry and
// publishes session.killed, and a session.updated after it would resurrect the
// row in an event-driven client.
func TestRefreshStatusPublishingOutage_NoUpdatedAfterTheRowIsGone(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "outage-gone", "http://127.0.0.1:1", session.Running)

	// Control: while the row is registered, the flag change IS announced.
	_, events := manager.events.subscribe()
	manager.refreshInstanceStatusPublishingOutage(repoID, inst)
	if inst.UnreachableSnapshot() == nil {
		t.Fatal("premise: the unanswered probe must raise the unreachable flag")
	}
	if got := drainUpdatedFor(events, "outage-gone"); got == 0 {
		t.Fatal("control: a registered row's outage change must publish session.updated")
	}

	// The kill commits during the refresh: the row leaves the registry. The next
	// outage change (a cleared flag here) must not be announced for it.
	inst.ClearUnreachable()
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "outage-gone"))
	manager.mu.Unlock()
	manager.refreshInstanceStatusPublishingOutage(repoID, inst)
	if got := drainUpdatedFor(events, "outage-gone"); got != 0 {
		t.Fatalf("published %d session.updated for a row no longer registered — a late event resurrects a killed session", got)
	}
}
