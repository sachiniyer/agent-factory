package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The usage-limit auto-resume scheduler tests (#1146 PR3). Every test drives the
// injected clock (withFrozenClock swaps nowFunc) rather than the wall clock, so
// due-time and backoff assertions are deterministic. They reuse limitResumeBackend
// (limit_resume_test.go) — its Respawn/SendPromptCommand instrumentation records
// which resume path ran and what prompt was re-delivered.

// newAutoResumeManager builds a status test manager with limit_auto_resume
// enabled and the given fallback interval, plus a limitResumeBackend session
// parked at a usage-limit wall (LiveLimitReached with resetAt). alive selects the
// live-stall vs exited-agent arm of resumeFromLimit. Returns the manager, repoID,
// the parked instance, and its backend.
func newAutoResumeManager(t *testing.T, retryInterval string, alive bool, prompt string, resetAt time.Time) (*Manager, string, *session.Instance, *limitResumeBackend) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	manager.cfg.LimitAutoResume = true
	manager.cfg.LimitRetryInterval = retryInterval
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: alive}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = prompt
	inst.SetLimitReached(resetAt)
	return manager, repoID, inst, backend
}

// TestResumeLimitedSessions_ResumesAfterWindow is the core PR3 flow: a session
// parked at a usage-limit wall with a parsed reset time stays untouched until the
// clock passes reset + limitResumeGrace, then is auto-resumed exactly once — the
// stored task prompt re-delivered and the limit liveness cleared.
func TestResumeLimitedSessions_ResumesAfterWindow(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)

	// Before the window elapses: no resume, still parked.
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("resume fired before the window elapsed: prompts=%v", prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("session must stay LimitReached before the resume window")
	}

	// Advance to just before due (reset + grace): still no resume.
	advance(time.Hour + limitResumeGrace - time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("resume fired a second before due: prompts=%v", prompts)
	}

	// Advance past due: resume fires exactly once, re-delivering the stored prompt.
	advance(2 * time.Second)
	manager.ResumeLimitedSessions()
	recoverCalls, respawnCalls, prompts := backend.snapshot()
	if recoverCalls != 0 {
		t.Fatalf("auto-resume must never route through Recover, got %d calls", recoverCalls)
	}
	if respawnCalls != 0 {
		t.Fatalf("a live stall needs no re-spawn, got %d Respawn calls", respawnCalls)
	}
	if len(prompts) != 1 || prompts[0] != "finish the migration" {
		t.Fatalf("re-delivered prompts = %v, want [\"finish the migration\"]", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("limit liveness must be cleared after auto-resume")
	}
}

// TestResumeLimitedSessions_ExitedAgentRespawns: the same flow when the agent's
// tmux exited while blocked — auto-resume must re-spawn through the guard-free
// Respawn path (never Recover) before re-delivering the prompt.
func TestResumeLimitedSessions_ExitedAgentRespawns(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, _, backend := newAutoResumeManager(t, "", false, "resume work", base) // alive=false, reset in the past

	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()

	recoverCalls, respawnCalls, prompts := backend.snapshot()
	if recoverCalls != 0 {
		t.Fatalf("auto-resume must not touch Recover, got %d calls", recoverCalls)
	}
	if respawnCalls != 1 {
		t.Fatalf("an exited agent must be re-spawned once, got %d Respawn calls", respawnCalls)
	}
	if len(prompts) != 1 || prompts[0] != "resume work" {
		t.Fatalf("re-delivered prompts = %v, want [\"resume work\"]", prompts)
	}
}

// TestResumeLimitedSessions_DisabledIsSurfaceOnly: with limit_auto_resume off
// (the default), the scheduler does ZERO work even long after the reset time —
// the limit is surface-only (PR2 badge + manual retry).
func TestResumeLimitedSessions_DisabledIsSurfaceOnly(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "should not send", base.Add(time.Hour))
	manager.cfg.LimitAutoResume = false // override the helper's enable

	advance(48 * time.Hour) // well past the reset time
	manager.ResumeLimitedSessions()

	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("auto-resume must not fire when limit_auto_resume is off: prompts=%v", prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a disabled scheduler must leave the session parked (surface-only)")
	}
}

// TestResumeLimitedSessions_KilledBeforeReset: a session tombstoned before its
// reset time is never resumed — the eligibility guard rejects it — so the clock
// passing due is a no-op.
func TestResumeLimitedSessions_KilledBeforeReset(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "dead work", base.Add(time.Hour))
	inst.MarkUserKilled()

	advance(2 * time.Hour)
	manager.ResumeLimitedSessions()

	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("a tombstoned session must never be auto-resumed: prompts=%v", prompts)
	}
}

// TestResumeLimitedSessions_ArchivedClearsTimer: a session archived after it was
// seen parked is not resumed, and its retry state is dropped once it leaves the
// limit liveness past its (never-set) backoff window — the timer is abandoned,
// no crash, no resurrection.
func TestResumeLimitedSessions_ArchivedClearsTimer(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "archived work", base.Add(time.Hour))
	key := daemonInstanceKey(repoID, "limited")

	// First pass (before due) creates the retry state.
	manager.ResumeLimitedSessions()
	manager.mu.Lock()
	_, hasState := manager.limitResumeStates[key]
	manager.mu.Unlock()
	if !hasState {
		t.Fatal("a parked session must record retry state on first sighting")
	}

	// The session is archived out from under the scheduler.
	_ = inst.Transition(session.ObserveLiveness(session.LiveArchived))
	advance(2 * time.Hour)
	manager.ResumeLimitedSessions()

	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("an archived session must never be auto-resumed: prompts=%v", prompts)
	}
	manager.mu.Lock()
	_, hasState = manager.limitResumeStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("retry state for a session that left the limit liveness must be dropped")
	}
}

// TestResumeLimitedSessions_NoResetTimeFallback: a limit banner with no parseable
// reset time is retried on the fixed limit_retry_interval fallback — nothing
// before the interval elapses, then a resume.
func TestResumeLimitedSessions_NoResetTimeFallback(t *testing.T) {
	advance := withFrozenClock(t)
	manager, _, _, backend := newAutoResumeManager(t, "30m", true, "no reset work", time.Time{}) // zero reset

	// First pass anchors parkedAt = now; nothing is due for a full interval.
	manager.ResumeLimitedSessions()
	advance(29 * time.Minute)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("resume fired before the fallback interval elapsed: prompts=%v", prompts)
	}

	advance(2 * time.Minute) // past 30m from the first sighting
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 || prompts[0] != "no reset work" {
		t.Fatalf("re-delivered prompts = %v, want [\"no reset work\"] after the fallback interval", prompts)
	}
}

// TestResumeLimitedSessions_NoResetNoIntervalNeverResumes: a limit banner with no
// parseable reset time AND no fallback interval (limit_retry_interval empty) is
// surface-only — never auto-resumed no matter how much time passes.
func TestResumeLimitedSessions_NoResetNoIntervalNeverResumes(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, _, backend := newAutoResumeManager(t, "", true, "orphan work", time.Time{}) // zero reset, no interval
	key := daemonInstanceKey(repoID, "limited")

	for i := 0; i < 5; i++ {
		advance(24 * time.Hour)
		manager.ResumeLimitedSessions()
	}

	if _, _, prompts := backend.snapshot(); len(prompts) != 0 {
		t.Fatalf("no reset time and no fallback interval must never auto-resume: prompts=%v", prompts)
	}
	// No schedulable state is retained either.
	manager.mu.Lock()
	_, hasState := manager.limitResumeStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("an unschedulable limit must not accumulate retry state")
	}
}

// TestResumeLimitedSessions_ReLimitBacksOff is the re-limit guard: after a resume
// the session re-hits the wall (re-enters LimitReached). The scheduler must NOT
// hammer — the exponential backoff throttles the next attempt — so repeated
// passes inside the backoff window fire nothing, and only one further resume
// lands once the window elapses.
func TestResumeLimitedSessions_ReLimitBacksOff(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base // reset already in the past
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "flaky work", resetAt)

	// First resume fires once the grace window passes.
	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("first resume: want 1 prompt, got %d", len(prompts))
	}

	// The session re-hits the wall with the same (past) reset time. Hammer the
	// scheduler for 9s of 1s ticks — all inside the 10s base backoff — and prove
	// it never re-fires.
	inst.SetLimitReached(resetAt)
	for i := 0; i < 9; i++ {
		advance(time.Second)
		manager.ResumeLimitedSessions()
	}
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("re-limit inside the backoff window must not re-fire: want 1 prompt, got %d", len(prompts))
	}

	// Once the backoff window elapses, exactly one further resume lands.
	advance(2 * time.Second) // now >10s since the first attempt
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 2 {
		t.Fatalf("after the backoff window a re-limited session resumes once more: want 2 prompts, got %d", len(prompts))
	}
}
