package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests pin the auto-resume scheduler's caller-side handling of a
// resumeFromLimitOutcome no-op (resumeNotPerformed, nil) on the candidate-swap
// path. resumeFromLimitLockedWithAccount returns that outcome for several
// mid-pass aborts; the one exercised here is the LimitAutoResume opt-out
// observed at the final-fence config recheck (daemon/limit.go:605-611) AFTER the
// pass-start config snapshot saw the feature enabled and the candidate swap
// was already preflighted.
//
// Before the fix, resumeLimitedSession treated every nil error from the helper
// as a successful resume and logged "auto-resumed ... on <agent> account
// <name>" even though no swap happened — because the helper discarded the
// outcome. The fix gates the success log on outcome == resumePerformed, so a
// no-op produces no success log. The regression below proves a mid-pass opt-out
// logs nothing, and the guard proves a genuine swap still logs the account-named
// success (preventing over-suppression). The ordinary-resume success log is
// already covered by TestResumeLimitedSessions_ResumesAfterWindow.

// TestResumeLimitedSessions_MidPassOptOutDoesNotLogSuccessForNonResume is the
// regression for the caller-side misclassification. A candidate account swap is
// preflighted under a pass-start config that enables LimitAutoResume; the
// final-fence recheck then observes the feature disabled (the window the fence
// comment at daemon/limit.go:596-600 exists to observe) and returns
// (resumeNotPerformed, nil). No swap happens, so no success log may fire — but
// before the fix the caller collapsed resumeNotPerformed onto resumePerformed
// and logged "auto-resumed ... on claude account \"work\"" for a session still
// parked at the wall.
//
// The config flip is delivered through testHookAccountSwapBeforeFinalFence, the
// same production seam where the live fence recheck happens. In this test the
// manager's live atomic config is unseeded, so m.Config() falls back to m.cfg;
// mutating manager.cfg.LimitAutoResume in the hook is the same observable the
// production atomic swap would produce (pass-start reads true, fence reads
// false) and is sufficient to drive the opt-out branch.
func TestResumeLimitedSessions_MidPassOptOutDoesNotLogSuccessForNonResume(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "continue", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	infoLog := captureInfoLog(t)

	// Flip the feature OFF at the final fence — after the pass-start snapshot
	// read it ON and preflighted the "work" candidate, but before the fence's
	// liveConfig read. This is the mid-pass opt-out the fence is designed to
	// observe, and the path that returns (resumeNotPerformed, nil).
	previousHook := testHookAccountSwapBeforeFinalFence
	testHookAccountSwapBeforeFinalFence = func() {
		manager.cfg.LimitAutoResume = false
	}
	t.Cleanup(func() { testHookAccountSwapBeforeFinalFence = previousHook })

	advance(time.Second)
	manager.ResumeLimitedSessions()

	// No resume happened: the session is still parked at the wall and nothing
	// was re-delivered or re-spawned. The candidate swap was preflighted but the
	// opt-out aborted it before any identity commit.
	if !inst.LimitReached() {
		t.Fatal("mid-pass opt-out must leave the session parked at the limit wall")
	}
	recovers, respawns, prompts := backend.snapshot()
	if recovers != 0 || respawns != 0 || len(prompts) != 0 {
		t.Fatalf("mid-pass opt-out must perform no resume work: recovers=%d respawns=%d prompts=%v", recovers, respawns, prompts)
	}
	if account, automatic := inst.AccountSelection(); account != "" || automatic {
		t.Fatalf("mid-pass opt-out must not commit the candidate identity: got (%q, %v)", account, automatic)
	}

	// The fix: a no-op resume must not emit a success log. Before the fix this
	// asserted the log CONTAINED the account-named success for a session that
	// was never resumed; after the fix the log must be free of it.
	logged := infoLog.String()
	if strings.Contains(logged, "auto-resumed limit-blocked session") {
		t.Fatalf("mid-pass opt-out logged a success for a non-resume:\n%s", logged)
	}
	if strings.Contains(logged, `claude account "work"`) {
		t.Fatalf("mid-pass opt-out logged an account swap that never happened:\n%s", logged)
	}
}

// TestResumeLimitedSessions_SwapSuccessStillLogsAccountName is the guard against
// over-suppression: a candidate swap that genuinely completes (resumePerformed)
// must still log the account-named success the feature introduced. The fix
// gates the success log on outcome == resumePerformed, which is exactly the
// outcome a completed swap returns, so the success log must fire unchanged.
// (The ordinary-resume success log is already asserted by
// TestResumeLimitedSessions_ResumesAfterWindow; this covers the swap branch,
// which no existing test asserts.)
func TestResumeLimitedSessions_SwapSuccessStillLogsAccountName(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	infoLog := captureInfoLog(t)

	advance(time.Second)
	manager.ResumeLimitedSessions()

	// A real swap completed: one respawn, one account-named prompt, the
	// candidate identity committed, and the limit wall cleared.
	recovers, respawns, prompts := backend.snapshot()
	if recovers != 0 {
		t.Fatalf("auto-resume must never route through Recover, got %d calls", recovers)
	}
	if respawns != 1 {
		t.Fatalf("account replacement respawns = %d, want 1", respawns)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], `claude account "work"`) ||
		!strings.Contains(prompts[0], "finish the migration") {
		t.Fatalf("swap prompt must name the identity change and retain the task, got %q", prompts)
	}
	if account, automatic := inst.AccountSelection(); account != "work" || !automatic {
		t.Fatalf("account selection = (%q, %v), want scheduler-selected work", account, automatic)
	}
	if inst.LimitReached() {
		t.Fatal("a successful account replacement must clear the old limit wall")
	}

	// And the account-named success log must still fire for the genuine swap.
	want := fmt.Sprintf("auto-resumed limit-blocked session %q (repo %s) on %s account %q (attempt %d)", inst.Title, repoID, "claude", "work", 1)
	if !strings.Contains(infoLog.String(), want) {
		t.Fatalf("swap success log = %q, want it to contain %q", infoLog.String(), want)
	}
}
