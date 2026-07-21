package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// unknownStartBackend models a launcher that timed out after it may have
// created a runtime. Kill deliberately reports success: if CreateSession asks
// the same uncertain binding to tear down, that confident answer would let the
// create path discard the only record of the workspace.
type unknownStartBackend struct {
	readyFakeBackend

	mu        sync.Mutex
	killCalls int
}

func (b *unknownStartBackend) Start(*session.Instance, bool) error {
	return fmt.Errorf("startup readiness timed out: %w", session.ErrPaneMayBeLive)
}

func (b *unknownStartBackend) Kill(*session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killCalls++
	return nil
}

func (b *unknownStartBackend) kills() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.killCalls
}

func nextCreateLifecycleEvent(t *testing.T, events <-chan agentproto.Event) (agentproto.EventType, session.InstanceData) {
	t.Helper()
	select {
	case event := <-events:
		var data session.InstanceData
		if len(event.Data) > 0 {
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatalf("unmarshal %s payload: %v", event.Type, err)
			}
		}
		return event.Type, data
	case <-time.After(3 * time.Second):
		t.Fatal("no create lifecycle event published within the deadline")
		return "", session.InstanceData{}
	}
}

func TestCreateSession_UnknownStartDoesNotAttemptDestructiveCleanup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return backend, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, events := manager.events.subscribe()

	_, createErr := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "uncertain-start", RepoPath: repoPath, Program: "claude",
	})
	if createErr == nil {
		t.Fatal("CreateSession reported success though startup state is unknown")
	}
	pendingType, pending := nextCreateLifecycleEvent(t, events)
	if pendingType != agentproto.EventSessionUpdated || pending.InFlightOp != session.OpCreating {
		t.Fatalf("first create event = (%s, %+v), want pending session.updated", pendingType, pending)
	}
	settledType, settled := nextCreateLifecycleEvent(t, events)
	if settledType != agentproto.EventSessionUpdated {
		t.Fatalf("retained uncertain create event = %s, want session.updated; session.killed removes the only cleanup handle from live clients", settledType)
	}
	if settled.ID != pending.ID || !settled.StartupStateUnknown || settled.InFlightOp != session.OpNone {
		t.Fatalf("retained uncertain create did not settle the pending identity: pending=%+v settled=%+v", pending, settled)
	}
	if calls := backend.kills(); calls != 0 {
		t.Fatalf("CreateSession attempted %d destructive cleanup(s) after startup already reported an unknown state; the same uncertain binding cannot prove the runtime absent", calls)
	}

	rec := recordFor(t, repo.ID, "uncertain-start")
	if rec == nil {
		t.Fatal("CreateSession discarded an uncertain startup with no record of the workspace")
	}
	if !rec.StartupStateUnknown {
		t.Fatal("the retained record did not durably classify its startup as unknown")
	}
	if rec.UserKilled {
		t.Fatal("an uncertain startup was recorded as a kill tombstone; the daemon would automatically retry destructive cleanup")
	}
	manager.mu.Lock()
	inst, tracked := manager.instances[daemonInstanceKey(repo.ID, "uncertain-start")]
	manager.mu.Unlock()
	if !tracked {
		t.Fatal("CreateSession did not keep the uncertain startup addressable in memory")
	}

	// Polling must leave the inert record alone. A tombstone would route this tick
	// to finishUserKill, call the same suspect binding, and turn its false "absent"
	// into workspace deletion after CreateSession had ostensibly preserved it.
	manager.refreshInstanceStatus(repo.ID, inst)
	if calls := backend.kills(); calls != 0 {
		t.Fatalf("the daemon poll attempted %d destructive cleanup(s) against an uncertain startup", calls)
	}
	if rec := recordFor(t, repo.ID, "uncertain-start"); rec == nil || !rec.StartupStateUnknown {
		t.Fatal("the daemon poll dropped the durable startup-unknown record")
	}
}
