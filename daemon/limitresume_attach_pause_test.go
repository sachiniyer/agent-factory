package daemon

import (
	"testing"
	"time"
)

// These tests pin the auto-resume scheduler's adherence to the attach pause
// lease (#1160): the scheduler is an automated delivery that pastes a stored
// prompt + Enter into a limit-parked session's pane, and the lease is the
// daemon's contract that an automated delivery into a pane a TUI is attached
// full-screen to must DEFER — so a scheduled resume cannot paste into and
// submit the user's in-progress input (#1586/#1638).
//
// Before the fix, resumeLimitedSession reached instance.SendPromptWithEvidence
// (and deliverManualAccountMission) without ever consulting isPollPaused, so a
// scheduled resume paste landed in a pane the user was actively typing into.
// The fix gates the scheduler's fire on the same lease every DeliverPrompt send
// path honors, checked in resumeLimitedSession after the locks are acquired and
// the eligibility re-check passes but BEFORE limitResumeAttempted records the
// attempt — so a deferral neither burns a backoff step nor logs a failed
// attempt. The explicit ResumeFromLimit RPC bypasses resumeLimitedSession
// entirely (it is an operator action like a manual send) and stays exempt.

// TestResumeLimitedSessions_DeliversWhileStatusPollPaused is the direct
// regression: a claude session parked at LiveLimitReached with a parsed reset
// time already in the past, LimitAutoResume enabled, the clock advanced past
// reset + limitResumeGrace, then PauseStatusPoll holds the attach lease, and
// ResumeLimitedSessions fires. The scheduler must NOT deliver the stored
// prompt into the pane while the lease is held — it must defer to a later tick.
func TestResumeLimitedSessions_DeliversWhileStatusPollPaused(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base // reset already in the past so the resume is immediately due
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "auto-resume work", resetAt)

	// Advance past reset + grace: the resume is due.
	advance(limitResumeGrace + time.Second)

	// A TUI attaches full-screen to the parked session: the daemon's pause
	// lease is the "attached and DEFER automated deliveries" signal (#1160).
	manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("PauseStatusPoll must hold the attach lease for the parked session")
	}

	manager.ResumeLimitedSessions()

	recovers, respawns, prompts := backend.snapshot()
	if recovers != 0 || respawns != 0 || len(prompts) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler delivered into the pane while the attach pause lease was held (isPollPaused=true): recovers=%d respawns=%d prompts=%v", recovers, respawns, prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a deferred resume must leave the session parked at the limit wall")
	}
}

// TestResumeLimitedSessions_AttachPauseDeferralLeavesBackoffUntouched pins the
// "neither burns a backoff step nor logs a failed attempt" contract of the fix:
// a deferral happens BEFORE limitResumeAttempted records the attempt, so the
// retry state's attempt counter and nextAttempt gate are unchanged. The resume
// must fire on the very first unpaused tick after detach, not be throttled by a
// backoff the deferral never should have incurred.
func TestResumeLimitedSessions_AttachPauseDeferralLeavesBackoffUntouched(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "auto-resume work", resetAt)
	advance(limitResumeGrace + time.Second)

	stateKey := stableSessionKey(repoID, inst)
	manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	manager.ResumeLimitedSessions()

	// The deferral must not have created retry state.
	manager.mu.Lock()
	st := manager.limitResumeStates[stateKey]
	manager.mu.Unlock()
	if st != nil && st.attempts != 0 {
		t.Fatalf("a deferred resume must not burn a backoff step, got attempts=%d", st.attempts)
	}

	// Detach: the resume fires on the very next tick — no backoff wait, because
	// the deferral recorded no attempt.
	manager.ResumeStatusPoll(repoID, inst.Title, inst.ID)
	manager.ResumeLimitedSessions()
	_, respawns, prompts := backend.snapshot()
	if respawns != 0 {
		t.Fatalf("a live stall needs no re-spawn, got %d Respawn calls", respawns)
	}
	if len(prompts) != 1 || prompts[0] != "auto-resume work" {
		t.Fatalf("the deferred resume must fire on the first unpaused tick: prompts=%v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("the limit liveness must be cleared once the deferred resume lands")
	}
}

// TestResumeFromLimit_ManualRPCExemptFromAttachPause pins the operator-action
// exemption: the explicit ResumeFromLimit RPC bypasses resumeLimitedSession
// entirely and reaches resumeFromLimitLockedOutcome with operatorInitiated=true,
// so it must still deliver immediately while a TUI is attached — exactly like a
// manual send-prompt. The deferral is scoped to the automated scheduler.
func TestResumeFromLimit_ManualRPCExemptFromAttachPause(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "manual retry work", resetAt)
	// No clock advance needed: the manual RPC is an explicit operator action
	// and ignores the scheduler's due-time gate too.
	_ = advance

	manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("PauseStatusPoll must hold the attach lease")
	}

	if _, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("manual ResumeFromLimit RPC must succeed while attached (operator action): %v", err)
	}

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 {
		t.Fatalf("a live stall manual retry needs no respawn, got %d", respawns)
	}
	if len(prompts) != 1 || prompts[0] != "manual retry work" {
		t.Fatalf("the manual retry must deliver the stored prompt immediately while attached: prompts=%v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("the manual retry must clear the limit liveness on delivery")
	}
}
