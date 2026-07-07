package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// limitResumeBackend is a FakeBackend instrumented for the usage-limit manual-
// retry tests (#1146). It reproduces LocalBackend.Recover's !Lost guard, so a
// regression that routes a LimitReached retry back through Recover (the #1204 P1
// — "respawn path always fails") trips that guard here exactly as it did against
// the real backend. Respawn — the guard-free re-spawn core the fix uses instead —
// and SendPromptCommand record their calls so the test can assert which path ran
// and what prompt was re-delivered.
type limitResumeBackend struct {
	*session.FakeBackend
	mu           sync.Mutex
	alive        bool
	recoverCalls int
	respawnCalls int
	sentPrompts  []string
}

func (b *limitResumeBackend) IsAlive(*session.Instance) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive
}

func (b *limitResumeBackend) Recover(i *session.Instance) error {
	b.mu.Lock()
	b.recoverCalls++
	b.mu.Unlock()
	// Mirror LocalBackend.Recover's guard: only a Lost session is recoverable.
	// A LimitReached session composes to Ready, so this rejects it — which is the
	// whole point of the P1 regression the fix removes.
	if s := i.GetStatus(); s != session.Lost {
		return fmt.Errorf("recover: session %q is %v, not Lost", i.Title, s)
	}
	i.MarkLive()
	return nil
}

func (b *limitResumeBackend) Respawn(i *session.Instance) error {
	b.mu.Lock()
	b.respawnCalls++
	b.mu.Unlock()
	// The guard-free core: re-spawn regardless of liveness (matches the real
	// LocalBackend.respawn, which ends by marking the session live).
	i.MarkLive()
	return nil
}

func (b *limitResumeBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	b.sentPrompts = append(b.sentPrompts, prompt)
	b.mu.Unlock()
	return nil
}

func (b *limitResumeBackend) snapshot() (recover, respawn int, prompts []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recoverCalls, b.respawnCalls, append([]string(nil), b.sentPrompts...)
}

// TestResumeFromLimit_ExitedAgent_RespawnsNotRecover is the #1204 P1 regression:
// retrying a session that hit a usage-limit wall AND whose agent tmux exited
// while blocked must re-spawn through the guard-free Respawn path, never through
// Recover — Recover's !Lost guard rejects a LimitReached session, so the old
// instance.Recover() call made every such retry fail. A task-driven session (one
// carrying a stored prompt) re-delivers that prompt so its work resumes.
func TestResumeFromLimit_ExitedAgent_RespawnsNotRecover(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// alive=false → the agent tmux exited while blocked, forcing the re-spawn arm.
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: false}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(time.Time{})

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
		// Before the fix this returned `recover: session "limited" is Ready, not
		// Lost` — the P1. It must now succeed.
		t.Fatalf("resumeFromLimit returned %v; a LimitReached retry must not fail the Recover !Lost guard (#1204 P1)", err)
	}

	recoverCalls, respawnCalls, prompts := backend.snapshot()
	if recoverCalls != 0 {
		t.Fatalf("Recover called %d times; the limit retry must NOT route through Recover's !Lost guard (#1204 P1)", recoverCalls)
	}
	if respawnCalls != 1 {
		t.Fatalf("Respawn called %d times, want 1 (an exited agent must be re-spawned)", respawnCalls)
	}
	if inst.LimitReached() {
		t.Fatal("limit state must be cleared after a successful resume")
	}
	if len(prompts) != 1 || prompts[0] != "finish the migration" {
		t.Fatalf("re-delivered prompts = %v, want [\"finish the migration\"] (a task session resumes its stored prompt)", prompts)
	}
}

// TestResumeFromLimit_LiveStall_SendsContinueNoRespawn: the common case — the
// agent is still alive but stalled at the limit wall. No re-spawn is needed (and
// Recover is never touched); an interactive session with no stored prompt just
// gets a bare "continue" to un-stall it, and the limit state clears.
func TestResumeFromLimit_LiveStall_SendsContinueNoRespawn(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// alive=true → a live stall, so no re-spawn should happen.
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "stalled", backend, true, session.Running)
	inst.Prompt = "" // interactive session: no stored prompt
	inst.SetLimitReached(time.Now())

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "stalled", RepoID: repoID}); err != nil {
		t.Fatalf("resumeFromLimit returned %v, want nil", err)
	}

	recoverCalls, respawnCalls, prompts := backend.snapshot()
	if recoverCalls != 0 {
		t.Fatalf("Recover called %d times; a live stall must never touch Recover", recoverCalls)
	}
	if respawnCalls != 0 {
		t.Fatalf("Respawn called %d times, want 0 (a live agent needs no re-spawn)", respawnCalls)
	}
	if inst.LimitReached() {
		t.Fatal("limit state must be cleared after a successful resume")
	}
	if len(prompts) != 1 || prompts[0] != "continue" {
		t.Fatalf("re-delivered prompts = %v, want [\"continue\"] (an interactive session gets a bare un-stall)", prompts)
	}
}

// TestResumeFromLimit_TeardownInFlightNoops pins #1263: a manual retry must not
// send a prompt or respawn while a kill/delete already owns the session.
func TestResumeFromLimit_TeardownInFlightNoops(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *Manager, string, *session.Instance)
	}{
		{
			name: "optimistic killing op",
			setup: func(_ *testing.T, _ *Manager, _ string, inst *session.Instance) {
				inst.SetInFlightOpForTest(session.OpKilling)
			},
		},
		{
			name: "manager kill in flight",
			setup: func(_ *testing.T, manager *Manager, repoID string, inst *session.Instance) {
				key := daemonInstanceKey(repoID, inst.Title)
				manager.mu.Lock()
				manager.killsInFlight[key] = struct{}{}
				manager.mu.Unlock()
			},
		},
		{
			name: "op lock busy",
			setup: func(t *testing.T, manager *Manager, repoID string, inst *session.Instance) {
				key := daemonInstanceKey(repoID, inst.Title)
				opLock := manager.opLockFor(key)
				opLock.Lock()
				t.Cleanup(opLock.Unlock)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
			inst := registerStarted(t, manager, repoID, repoPath, "deleting-limit", backend, true, session.Running)
			inst.Prompt = "finish the migration"
			inst.SetLimitReached(time.Now())
			tt.setup(t, manager, repoID, inst)

			if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
				t.Fatalf("resumeFromLimit returned %v, want nil no-op", err)
			}

			recoverCalls, respawnCalls, prompts := backend.snapshot()
			if recoverCalls != 0 || respawnCalls != 0 || len(prompts) != 0 {
				t.Fatalf("teardown no-op must not act: recover=%d respawn=%d prompts=%v", recoverCalls, respawnCalls, prompts)
			}
			if !inst.LimitReached() {
				t.Fatal("teardown no-op must leave the limit state intact")
			}
		})
	}
}

// TestResumeFromLimit_NotLimited_Errors: resuming a session that is not blocked
// on a usage limit is rejected before any re-spawn or prompt delivery, so PR3's
// scheduler and the manual retry can both call it without a pre-check racing the
// poll's self-recovery.
func TestResumeFromLimit_NotLimited_Errors(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "ready", backend, true, session.Ready)

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "ready", RepoID: repoID}); err == nil {
		t.Fatal("resumeFromLimit on a non-limit session must return an error")
	}
	recoverCalls, respawnCalls, prompts := backend.snapshot()
	if recoverCalls != 0 || respawnCalls != 0 || len(prompts) != 0 {
		t.Fatalf("a rejected resume must not act: recover=%d respawn=%d prompts=%v", recoverCalls, respawnCalls, prompts)
	}
	if inst.LimitReached() {
		t.Fatal("a Ready session must not become limit-blocked")
	}
}
