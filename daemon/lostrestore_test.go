package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// recoverFakeBackend counts Recover calls and emulates LocalBackend.Recover's
// success contract (flip to Running) or a configured failure. Type is
// overridable so the remote-skip rule can be exercised.
type recoverFakeBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	failWith error
	recovers int
	typeName string
}

func (b *recoverFakeBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	if b.failWith != nil {
		return b.failWith
	}
	inst.SetStatusIfNotDeleting(session.Running)
	return nil
}

func (b *recoverFakeBackend) recoverCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

func (b *recoverFakeBackend) Type() string {
	if b.typeName != "" {
		return b.typeName
	}
	return "local"
}

// zeroRestoreBackoff makes every RestoreLostSessions pass an attempt.
func zeroRestoreBackoff(t *testing.T) {
	t.Helper()
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = 0
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
}

// TestRestoreLostSessions_RecoversLostInstance: the core loop — a Lost local
// session gets exactly one Recover, comes back Running, and its retry state is
// dropped.
func TestRestoreLostSessions_RecoversLostInstance(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after recovery", got)
	}
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[daemonInstanceKey(repoID, "stranded")]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("successful recovery must drop the retry state")
	}

	// A healed session must not be touched again.
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls after heal = %d, want still 1", got)
	}
}

// TestRestoreLostSessions_KeepsRetryingAndHeals mirrors the #1128 root-ensure
// guarantee for the general loop: failures past the escalation threshold never
// park the retry permanently, and the first pass after the cause clears heals.
func TestRestoreLostSessions_KeepsRetryingAndHeals(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("worktree unavailable")}
	inst := registerStarted(t, manager, repoID, repoPath, "unlucky", backend, true, session.Lost)

	attempts := lostRestoreEscalationThreshold + 3
	for i := 0; i < attempts; i++ {
		manager.RestoreLostSessions()
	}
	if got := backend.recoverCalls(); got != attempts {
		t.Fatalf("loop must keep attempting past the escalation threshold: want %d attempts, got %d", attempts, got)
	}

	// The cause clears (the outage ends): the very next pass must heal.
	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running on the first pass after the cause cleared", got)
	}
}

// TestRestoreLostSessions_BacksOffBetweenFailures: passes inside the backoff
// window must not re-attempt (the cheap-retry discipline, not a hot loop).
func TestRestoreLostSessions_BacksOffBetweenFailures(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = time.Hour
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("still down")}
	registerStarted(t, manager, repoID, repoPath, "waiting", backend, true, session.Lost)

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("passes inside the backoff window must not re-attempt: want 1, got %d", got)
	}
}

// TestRestoreLostSessions_SkipsIneligible: tombstoned records (their only
// future is a finished kill), sessions with a kill in flight, remote sessions
// (flagged, not auto-restored), non-Lost sessions, and the reserved root
// (EnsureRootAgents owns it) are never Recover'd.
func TestRestoreLostSessions_SkipsIneligible(t *testing.T) {
	t.Run("tombstoned", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerStarted(t, manager, repoID, repoPath, "corpse", backend, true, session.Lost)
		inst.MarkUserKilled()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a tombstoned session must never be restored, got %d recover calls", got)
		}
	})

	t.Run("kill in flight", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, "dying", backend, true, session.Lost)
		key := daemonInstanceKey(repoID, "dying")
		manager.mu.Lock()
		manager.killsInFlight[key] = struct{}{}
		manager.mu.Unlock()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a session mid-kill must not be restored, got %d recover calls", got)
		}
	})

	t.Run("remote", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), typeName: "remote"}
		registerStarted(t, manager, repoID, repoPath, "faraway", backend, true, session.Lost)
		manager.RestoreLostSessions()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("remote sessions are not auto-restored in v1, got %d recover calls", got)
		}
	})

	t.Run("non-lost statuses", func(t *testing.T) {
		for _, status := range []session.Status{session.Running, session.Ready, session.Loading, session.Deleting} {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
			registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, status)
			manager.RestoreLostSessions()
			if got := backend.recoverCalls(); got != 0 {
				t.Fatalf("status %v must not be restored, got %d recover calls", status, got)
			}
		}
	})

	t.Run("reserved root", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, backend, true, session.Lost)
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("the reserved root is EnsureRootAgents' job, got %d recover calls", got)
		}
	})
}

// TestRestoreLostSessions_StateCleanedForGoneSessions: retry state for
// sessions that were killed or healed does not accumulate.
func TestRestoreLostSessions_StateCleanedForGoneSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("down")}
	registerStarted(t, manager, repoID, repoPath, "doomed", backend, true, session.Lost)

	manager.RestoreLostSessions()
	key := daemonInstanceKey(repoID, "doomed")
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if !hasState {
		t.Fatal("a failed attempt must record retry state")
	}

	// The session disappears (user killed it; record + map entry gone).
	manager.mu.Lock()
	delete(manager.instances, key)
	manager.mu.Unlock()
	manager.RestoreLostSessions()

	manager.mu.Lock()
	_, hasState = manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("retry state for a gone session must be dropped")
	}
}
