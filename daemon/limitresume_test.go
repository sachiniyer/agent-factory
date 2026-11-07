package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureInfoLog routes log.InfoLog into a buffer for the test's lifetime, so a
// test can assert what the daemon claims in its own success log (#3240).
func captureInfoLog(t *testing.T) *logCapture {
	t.Helper()
	return captureInfo(t)
}

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
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, tmux.ProgramClaude)
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, repoID, repoPath := newStatusTestManager(t)
	manager.cfg.LimitAutoResume = true
	manager.cfg.LimitRetryInterval = retryInterval
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: alive}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	// A started production session always has its Agent tab. Recovery metadata
	// (notably a pending injected Claude id) is promoted into that durable tab,
	// so a fixture without one exercises an impossible record and masks delivery.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
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
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)
	infoLog := captureInfoLog(t)

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
	// The success log states the trigger the daemon observed: on this path it
	// scheduled against the banner's parsed reset time, and that time passing is
	// what it actually knows (#3240).
	want := fmt.Sprintf("auto-resumed limit-blocked session %q (repo %s) after its parsed usage-limit reset time passed (attempt 1)", inst.Title, repoID)
	if logged := infoLog.String(); !strings.Contains(logged, want) {
		t.Fatalf("resume log = %q, want it to contain %q", logged, want)
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
	key := stableSessionKey(repoID, inst)

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
	manager, repoID, inst, backend := newAutoResumeManager(t, "30m", true, "no reset work", time.Time{}) // zero reset
	infoLog := captureInfoLog(t)

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
	// No reset time was ever parsed here: the resume proves only that the fallback
	// interval fired and the retry succeeded. The log must state that trigger and
	// must not claim a usage-limit window elapsed — a fact about the provider the
	// daemon never observed (#3240).
	logged := infoLog.String()
	want := fmt.Sprintf("auto-resumed limit-blocked session %q (repo %s) after the limit_retry_interval fallback elapsed (no reset time was parsed) (attempt 1)", inst.Title, repoID)
	if !strings.Contains(logged, want) {
		t.Fatalf("fallback resume log = %q, want it to contain %q", logged, want)
	}
	if strings.Contains(logged, "window elapsed") {
		t.Fatalf("fallback resume log asserts a usage-limit window elapsed, which the daemon cannot know: %q", logged)
	}
}

// TestResumeLimitedSessions_NoResetNoIntervalNeverResumes: a limit banner with no
// parseable reset time AND no fallback interval (limit_retry_interval empty) is
// surface-only — never auto-resumed no matter how much time passes.
func TestResumeLimitedSessions_NoResetNoIntervalNeverResumes(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "orphan work", time.Time{}) // zero reset, no interval
	key := stableSessionKey(repoID, inst)

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

// TestResumeLimitedSessions_SendsPromptOnceWhileResidueBannerShows is the
// #6959 review's prompt-replay wedge, exercised end to end: the resume's send
// lands and the wall clears, the next poll re-detects the banner still on the
// pane, and the sweep must NOT send the prompt again. Before the fix the
// re-detection re-parked the row onto the expired claim; the scheduler's
// state-drop on the Running row then made the re-park a fresh, already-due
// episode — so every idle poll re-sent the stored prompt forever.
func TestResumeLimitedSessions_SendsPromptOnceWhileResidueBannerShows(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(-time.Hour))

	// The parked claim carries the banner's own anchor and phrase — the
	// record a send-path or poll park writes — so the residue check can
	// prove the re-read is the same emission. Seed it by sighting the exact
	// pane content the poll will re-detect: real transcript above the banner,
	// so the anchor is the position read a live capture produces.
	const residuePane = "some earlier agent output\n$ af sessions list\n" +
		"Claude usage limit reached. Your limit will reset at 2pm (America/New_York)"
	sighting := manager.limitDetector.Load().Sight(residuePane, inst.ResolvedAgent(), nowFunc())
	if !sighting.Hit {
		t.Fatal("the fixture banner must hit the claude limit matcher")
	}
	inst.SetLimitReachedAtEvidence(base.Add(-time.Hour), sighting.Evidence,
		session.LimitBanner{Prefix: sighting.BannerPrefix, Count: sighting.BannerCount})

	// Past reset+grace: the resume fires once and re-delivers the prompt.
	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the resume must deliver the stored prompt once, got %v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("the landed send clears the wall")
	}

	// The unchanged banner still sits in the capture; the next poll's
	// re-detection of it is residue — never a wall — so the row must not
	// re-park, and no retry episode may re-arm. The residue verdict must also
	// let the poll continue ordinary liveness classification: a cleared row
	// stalling on its own stale banner would sit Running until the banner
	// scrolls off instead of settling on the capture's real state.
	_, epoch := inst.InFlightOpAndEpoch()
	manager.resolveIdleLiveness(inst, residuePane, epoch)
	if inst.LimitReached() {
		t.Fatal("the row's own expired parked banner is residue, not a wall")
	}
	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("a residue sighting must not block ordinary classification: liveness = %v, want ready", got)
	}
	for i := 0; i < 5; i++ {
		advance(time.Minute)
		manager.ResumeLimitedSessions()
	}
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the prompt must be sent exactly once while the residue banner shows: got %v", prompts)
	}
}

// TestResumeLimitedSessions_NoReplayAfterEarlyRetryRepark is the #6972 item-4
// wedge: an explicit retry BEFORE the claimed reset lands its send and clears
// the wall; the unchanged banner still reads on the pane, so the poll re-parks
// the row at the same R. At R+grace the scheduler must NOT re-deliver the
// prompt — the wall it would be answering was already sent past. Without a
// "the send landed" tombstone on the claim the re-park looks like a fresh
// scheduling decision and replays it.
func TestResumeLimitedSessions_NoReplayAfterEarlyRetryRepark(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(30 * time.Minute) // the wall lifts in half an hour
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)

	const banner = "some earlier agent output\n$ af sessions list\n" +
		"Claude usage limit reached. Your limit will reset at 2pm (America/New_York)"
	sighting := manager.limitDetector.Load().Sight(banner, inst.ResolvedAgent(), nowFunc())
	if !sighting.Hit {
		t.Fatal("the fixture banner must hit the claude limit matcher")
	}
	inst.SetLimitReachedAtEvidence(resetAt, sighting.Evidence,
		session.LimitBanner{Prefix: sighting.BannerPrefix, Count: sighting.BannerCount})

	// The explicit retry before R: the send lands and clears the wall.
	if _, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("the early retry must land its send: %v", err)
	}
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the early retry sends the prompt exactly once, got %v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("a landed send clears the wall")
	}

	// The unchanged banner still sits in the capture; the poll re-reads it
	// and re-parks at the same R — the claim the send already answered.
	_, epoch := inst.InFlightOpAndEpoch()
	manager.resolveIdleLiveness(inst, banner, epoch)
	if !inst.LimitReached() {
		t.Fatal("the unchanged banner before R re-parks the row at its recorded reset")
	}

	// At R + grace the scheduler must not replay the prompt.
	advance(30*time.Minute + limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the wall the early retry already answered must not re-send at R+grace: prompts=%v", prompts)
	}
}

// TestResumeLimitedSessions_NoIntervalReplayForResetlessWall is the #6972
// item-3 wedge: a wall the detector could not parse a reset from retries on
// the fixed limit_retry_interval. After the send lands, the unchanged
// reset-less emission is residue — never a new wall — so the next interval
// must not fire the retry loop again.
func TestResumeLimitedSessions_NoIntervalReplayForResetlessWall(t *testing.T) {
	advance := withFrozenClock(t)
	manager, _, inst, backend := newAutoResumeManager(t, "30m", true, "no reset work", time.Time{})

	const banner = "agent output before the wall\n" +
		"Claude usage limit reached. Try again later."
	sighting := manager.limitDetector.Load().Sight(banner, inst.ResolvedAgent(), nowFunc())
	if !sighting.Hit {
		t.Fatal("a time-free claude banner must still hit the limit matcher")
	}
	if !sighting.ResetAt.IsZero() {
		t.Fatal("the fixture banner must parse no reset")
	}
	inst.SetLimitReachedAtEvidence(time.Time{}, "",
		session.LimitBanner{Prefix: sighting.BannerPrefix, Count: sighting.BannerCount})

	// First pass anchors parkedAt; a full interval later the retry fires.
	manager.ResumeLimitedSessions()
	advance(31 * time.Minute)
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the interval retry delivers the prompt once, got %v", prompts)
	}

	// The unchanged reset-less emission is residue: the poll must not
	// re-park it, and no later interval may send again.
	_, epoch := inst.InFlightOpAndEpoch()
	manager.resolveIdleLiveness(inst, banner, epoch)
	if inst.LimitReached() {
		t.Fatal("a cleared reset-less wall's unchanged emission is residue, not a new wall")
	}
	for i := 0; i < 4; i++ {
		advance(31 * time.Minute)
		manager.ResumeLimitedSessions()
	}
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the reset-less wall's retired emission must not replay on the interval: prompts=%v", prompts)
	}
}
