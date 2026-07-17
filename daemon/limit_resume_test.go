package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// limitResumeBackend is a FakeBackend instrumented for the usage-limit manual-
// retry tests (#1146). It reproduces LocalBackend.Recover's !Lost guard, so a
// regression that routes a LimitReached retry back through Recover (the #1204 P1
// — "respawn path always fails") trips that guard here exactly as it did against
// the real backend. Respawn — the guard-free re-spawn core the fix uses instead —
// and SendPromptCommand record their calls so the test can assert which path ran
// and what prompt was re-delivered.
//
// onRespawn stands in for the durable worktree mutation a real respawn can make
// (RebuildFreshFromRecordedBase recreating the branch); sendPromptErr fails the
// prompt delivery that follows it. Both are the #1854 fixture and default to
// inert, so the tests predating it are unaffected.
type limitResumeBackend struct {
	*session.FakeBackend
	mu            sync.Mutex
	alive         bool
	recoverCalls  int
	respawnCalls  int
	sentPrompts   []string
	onRespawn     func(*session.Instance)
	sendPromptErr error
}

func (b *limitResumeBackend) IsAlive(*session.Instance) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive, nil
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
	_ = i.Transition(session.ConfirmLive())
	return nil
}

func (b *limitResumeBackend) Respawn(i *session.Instance) error {
	b.mu.Lock()
	b.respawnCalls++
	mutate := b.onRespawn
	b.mu.Unlock()
	// A real respawn can rebuild the worktree on its way to success, mutating
	// durable state before anything downstream runs (#1854).
	if mutate != nil {
		mutate(i)
	}
	// The guard-free core: re-spawn regardless of liveness (matches the real
	// LocalBackend.respawn, which ends by marking the session live).
	_ = i.Transition(session.ConfirmLive())
	return nil
}

func (b *limitResumeBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sendPromptErr != nil {
		return b.sendPromptErr
	}
	b.sentPrompts = append(b.sentPrompts, prompt)
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

// TestResumeFromLimit_PersistsRespawnMutationsWhenSendPromptFails is the
// regression for #1854, the adjacent call-site of #1841: Respawn shares
// LocalBackend.respawn, so it can rebuild a vanished worktree — recreating the
// branch, flipping branchCreatedByUs true and rewriting baseCommitSHA — on its
// way to SUCCESS. resumeFromLimit persisted only at the very end, so a
// SendPrompt failure after that rebuild returned early and dropped the mutation:
// a daemon restart reloaded a record with no rebuilt branch recorded and the
// branch af itself created was orphaned, never cleaned up on kill.
//
// The poll does not paper over it — persistPollChange writes only when the
// liveness or reset time changed, and the respawn already left the instance
// LiveRunning, so the next tick compares LiveRunning → LiveRunning and skips.
func TestResumeFromLimit_PersistsRespawnMutationsWhenSendPromptFails(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// The worktree the rebuild leaves behind, with branchCreatedByUs=true. The
	// seeded disk record carries no worktree data at all, so anything the respawn
	// rebuilt exists only in memory until something writes it back.
	wtPath := filepath.Join(filepath.Dir(repoPath), "repo-1854")
	branch := "af/persist-1854"
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	rebuilt, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, "persist-1854", branch, "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}

	// alive=false → probeDead → the respawn arm. The respawn rebuilds the worktree
	// and succeeds; the SendPrompt that follows it fails.
	backend := &limitResumeBackend{
		FakeBackend:   session.NewFakeBackend(),
		alive:         false,
		onRespawn:     func(i *session.Instance) { i.SetGitWorktreeForTest(rebuilt) },
		sendPromptErr: errors.New("agent-server refused the prompt"),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "persist-1854", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(time.Time{})

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "persist-1854", RepoID: repoID}); err == nil {
		t.Fatal("resumeFromLimit returned nil; a failed SendPrompt must still surface to the caller")
	}

	if _, respawnCalls, _ := backend.snapshot(); respawnCalls != 1 {
		t.Fatalf("Respawn called %d times, want 1 (the fixture must reach the rebuild arm)", respawnCalls)
	}

	rec := recordFor(t, repoID, "persist-1854")
	if rec == nil {
		t.Fatal("record must still exist after a failed resume")
	}
	if rec.Worktree.BranchCreatedByUs == nil || !*rec.Worktree.BranchCreatedByUs {
		t.Fatalf("persisted branchCreatedByUs = %v, want true (the rebuild's flag must survive a failed SendPrompt)", rec.Worktree.BranchCreatedByUs)
	}
	// The branch name matters as much as the flag: a restart that reloads a record
	// with no branch recorded cannot clean up what the rebuild created.
	if rec.Worktree.BranchName != branch {
		t.Fatalf("persisted branch = %q, want %q (the rebuilt branch must be recorded or kill orphans it)", rec.Worktree.BranchName, branch)
	}
}

// TestResumeFromLimit_KeepsLimitBlockedWhenSendPromptFails is the post-#1857
// codex finding. On the probeDead arm Respawn ends in ConfirmLive, so the session
// reads LiveRunning before its pending prompt has been delivered — and #1857's
// checkpoint serializes the whole instance right there. A SendPrompt failure (or
// a crash before the send) therefore left an UNBLOCKED session on both axes while
// the prompt never landed, and every retry path gates on the limit still being
// set: resumeFromLimit's own !LimitReached guard, and the auto-resume scheduler's
// GetLiveness() != LiveLimitReached. Nothing re-delivered the prompt, so the
// session was stranded — in memory immediately, and after #1857 across a restart
// too. It must stay parked until the send actually succeeds.
func TestResumeFromLimit_KeepsLimitBlockedWhenSendPromptFails(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// A real parsed window, not the zero time: the reset time is what the
	// auto-resume scheduler schedules off (reset + grace), so it has to survive the
	// respawn round-trip as well as the block itself.
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	// alive=false → probeDead → the respawn arm, whose ConfirmLive drops the limit.
	// The SendPrompt that follows it fails.
	backend := &limitResumeBackend{
		FakeBackend:   session.NewFakeBackend(),
		alive:         false,
		sendPromptErr: errors.New("agent-server refused the prompt"),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "parked-1857", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(resetAt)

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "parked-1857", RepoID: repoID}); err == nil {
		t.Fatal("resumeFromLimit returned nil; a failed SendPrompt must still surface to the caller")
	}
	if _, respawnCalls, _ := backend.snapshot(); respawnCalls != 1 {
		t.Fatalf("Respawn called %d times, want 1 (the fixture must reach the respawn arm)", respawnCalls)
	}

	// In memory: the live daemon's auto-resume scheduler reads exactly this, and
	// skips anything that is not LiveLimitReached.
	if !inst.LimitReached() {
		t.Fatal("session is not limit-blocked in memory after a failed resume; auto-resume's LiveLimitReached guard skips such a row forever, so the prompt that never landed would never be re-delivered")
	}
	gotReset, ok := inst.LimitResetAt()
	if !ok || !gotReset.Equal(resetAt) {
		t.Fatalf("in-memory reset time = (%v, %v), want (%v, true): the scheduler fires at reset+grace, so a zeroed window silently re-dates the retry", gotReset, ok, resetAt)
	}

	// On disk: what a daemon restart reloads. This is the axis #1857 regressed —
	// before this fix the checkpoint serialized the post-ConfirmLive LiveRunning.
	rec := recordFor(t, repoID, "parked-1857")
	if rec == nil {
		t.Fatal("record must still exist after a failed resume")
	}
	if rec.Liveness != session.LiveLimitReached {
		t.Fatalf("persisted liveness = %v, want LiveLimitReached: a restart must reload the session still parked, or neither the manual retry nor auto-resume will ever retry it", rec.Liveness)
	}
	if !rec.LimitResetAt.Equal(resetAt) {
		t.Fatalf("persisted reset time = %v, want %v (a reload must schedule off this episode's window)", rec.LimitResetAt, resetAt)
	}
}

// TestResumeFromLimit_RespawnArm_ClearsLimitDurablyOnSuccess pins the other half
// of the post-#1857 fix: re-parking the session across the respawn is TRANSIENT.
// Once the prompt lands, ClearLimitReached lifts the block and the second
// checkpoint records it, so a restart reloads a resumed session rather than one
// wedged at a wall it already cleared. Without this the fix above would trade a
// stranded-parked bug for a stranded-blocked one.
func TestResumeFromLimit_RespawnArm_ClearsLimitDurablyOnSuccess(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// alive=false → probeDead → the respawn arm; SendPrompt succeeds (no error
	// hook), which is the resume's single completion point.
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: false}
	inst := registerStarted(t, manager, repoID, repoPath, "resumed-1857", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(time.Now().Add(30 * time.Minute).UTC())

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "resumed-1857", RepoID: repoID}); err != nil {
		t.Fatalf("resumeFromLimit returned %v, want nil", err)
	}
	if _, respawnCalls, prompts := backend.snapshot(); respawnCalls != 1 || len(prompts) != 1 || prompts[0] != "finish the migration" {
		t.Fatalf("respawn=%d prompts=%v, want respawn=1 and the stored prompt re-delivered once", respawnCalls, prompts)
	}
	if inst.LimitReached() {
		t.Fatal("limit must be cleared in memory once the prompt landed; the re-park across the respawn is transient, not sticky")
	}

	rec := recordFor(t, repoID, "resumed-1857")
	if rec == nil {
		t.Fatal("record must exist after a successful resume")
	}
	if rec.Liveness == session.LiveLimitReached {
		t.Fatal("persisted liveness is still LiveLimitReached after a successful resume: a restart would reload a resumed session as parked and re-resume it")
	}
	if !rec.LimitResetAt.IsZero() {
		t.Fatalf("persisted reset time = %v, want zero (a cleared limit must not carry a stale window to disk)", rec.LimitResetAt)
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
