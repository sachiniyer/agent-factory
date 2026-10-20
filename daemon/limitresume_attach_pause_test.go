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

// TestResumeLimitedSessions_AttachPauseDuringLivenessProbeDefers pins the
// send-boundary recheck (#5422 review): the top-level gate in
// resumeLimitedSession runs BEFORE the liveness probe and account-swap readiness
// wait this body performs, and PauseStatusPoll takes only pausedMu — not the
// target or op locks the body holds — so a TUI can attach and renew the lease
// during that window. resumeFromLimitLockedOutcome rechecks isPollPaused right
// before the send, gated on !operatorInitiated, so an automated resume that
// reached the send with a lease since acquired still defers instead of pasting
// into the user's pane. testHookResumeBeforeSend fires in that window (after the
// top-level gate, before the send) and is the only seam that can attach the lease
// there, so this test exercises the recheck directly.
//
// The send-boundary recheck runs AFTER limitResumeAttempted has recorded the
// attempt, so unlike the top-level gate a deferral here burns one backoff step —
// an acceptable trade for the race window, since the user is attached and a short
// retry delay is preferable to pasting into their pane. This test asserts the
// deferral itself; the backoff-throttled re-fire is the same path the top-level
// deferral's companion test already pins.
func TestResumeLimitedSessions_AttachPauseDuringLivenessProbeDefers(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "auto-resume work", resetAt)
	advance(limitResumeGrace + time.Second)

	// The lease is NOT held when ResumeLimitedSessions starts, so the top-level
	// gate in resumeLimitedSession passes. The hook attaches it inside the body,
	// in the window the send-boundary recheck exists for.
	prevHook := testHookResumeBeforeSend
	testHookResumeBeforeSend = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeSend = prevHook })

	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 || len(prompts) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler delivered into the pane after a TUI attached during the liveness window (send-boundary recheck missed): respawns=%d prompts=%v", respawns, prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a send-boundary deferral must leave the session parked at the limit wall")
	}
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeFromLimit_ManualRPCExemptFromSendBoundaryAttachPause pins the
// operator-initiated exemption at the send boundary itself: even when the lease
// is acquired in the window the send-boundary recheck covers (via the same hook),
// the explicit ResumeFromLimit RPC reaches resumeFromLimitLockedOutcome with
// operatorInitiated=true and the recheck is skipped, so a manual retry still
// delivers immediately. The deferral is scoped to the automated scheduler.
func TestResumeFromLimit_ManualRPCExemptFromSendBoundaryAttachPause(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "manual retry work", resetAt)
	_ = advance

	prevHook := testHookResumeBeforeSend
	testHookResumeBeforeSend = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeSend = prevHook })

	if _, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("manual ResumeFromLimit RPC must succeed even when the lease is acquired at the send boundary (operator action): %v", err)
	}

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 {
		t.Fatalf("a live stall manual retry needs no respawn, got %d", respawns)
	}
	if len(prompts) != 1 || prompts[0] != "manual retry work" {
		t.Fatalf("the manual retry must deliver immediately despite the send-boundary lease: prompts=%v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("the manual retry must clear the limit liveness on delivery")
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeAccountSwapCommitDefers pins the
// pre-commit attach-lease recheck (#5422 review): an automatic account swap
// performs a liveness probe before the identity commit, and PauseStatusPoll takes
// only pausedMu — not the target or op locks the resume body holds — so a TUI
// can attach and renew the lease during that probe. Without a recheck before
// commitNewAccountSwapIdentity, the swap would commit the identity and proceed
// to replace the attached runtime before the send-boundary recheck ever ran. The
// pre-commit recheck (gated on !operatorInitiated) defers the whole swap instead,
// leaving the session parked and the pending swap pending for the next tick.
func TestResumeLimitedSessions_AttachPauseBeforeAccountSwapCommitDefers(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)
	configureLimitAccountCandidate(t, manager, "work")

	// The account choice is immediate; advance past the reset window so the
	// swap is due.
	advance(time.Second)

	// The lease is NOT held when ResumeLimitedSessions starts, so the top-level
	// gate in resumeLimitedSession passes. The hook attaches it inside the body,
	// after the account-swap liveness probe and before the identity commit — the
	// window the pre-commit recheck exists for.
	prevHook := testHookResumeBeforeAccountSwapCommit
	testHookResumeBeforeAccountSwapCommit = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeAccountSwapCommit = prevHook })

	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 || len(prompts) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler committed the account swap after a TUI attached before the identity commit (pre-commit recheck missed): respawns=%d prompts=%v", respawns, prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a pre-commit deferral must leave the session parked at the limit wall")
	}
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeAccountSwapTeardownDefers pins the
// in-commit teardown attach-lease recheck (#5422 review): the pre-commit recheck
// in resumeFromLimitLockedOutcome runs before the identity commit, but
// commitNewAccountSwapIdentity performs the account-swap admission and the rare
// in-commit liveness re-probe under the config/account/project fences before it
// stops the old panes via prepareRuntimeForAccountSwap — a window where a TUI can
// attach and renew the lease. The recheck immediately before that teardown (gated
// on !operatorInitiated) defers the swap so prepareRuntimeForAccountSwap never
// runs, leaving the session parked and the pending swap pending for the next tick.
func TestResumeLimitedSessions_AttachPauseBeforeAccountSwapTeardownDefers(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)
	configureLimitAccountCandidate(t, manager, "work")

	advance(time.Second)

	// The lease is NOT held when ResumeLimitedSessions starts, and the pre-commit
	// recheck passes (the hook only attaches inside commitNewAccountSwapIdentity,
	// after the admission and before prepareRuntimeForAccountSwap stops the old
	// panes). The in-commit teardown recheck is the only thing that keeps the
	// commit from tearing down a runtime a TUI just attached to.
	prevHook := testHookResumeBeforeAccountSwapTeardown
	testHookResumeBeforeAccountSwapTeardown = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeAccountSwapTeardown = prevHook })

	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 || len(prompts) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler tore down the runtime after a TUI attached inside the identity commit (pre-teardown recheck missed): respawns=%d prompts=%v", respawns, prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a pre-teardown deferral must leave the session parked at the limit wall")
	}
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeAccountSwapRespawnDefers pins the
// pre-respawn attach-lease recheck (#5422 review): the identity commit and the
// account-swap liveness probe have both run by the time the respawn replaces the
// runtime, so a TUI that attaches during the commit window (after the pre-commit
// recheck passed) must still be honored before the respawn. The pre-respawn
// recheck (gated on !operatorInitiated) defers the respawn, leaving the session
// parked; the committed identity stays pending for the next tick (the same
// recovery a send error uses, since ClearPendingAccountSwap is only called on a
// successful send).
func TestResumeLimitedSessions_AttachPauseBeforeAccountSwapRespawnDefers(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)
	configureLimitAccountCandidate(t, manager, "work")

	advance(time.Second)

	// The lease is NOT held when ResumeLimitedSessions starts, and the
	// pre-commit recheck passes (the hook only attaches at the respawn boundary).
	// The identity commit runs, but the pre-respawn recheck keeps the respawn
	// from replacing a runtime a TUI just attached to.
	prevHook := testHookResumeBeforeAccountSwapRespawn
	testHookResumeBeforeAccountSwapRespawn = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeAccountSwapRespawn = prevHook })

	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 0 || len(prompts) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler respawned after a TUI attached before the respawn (pre-respawn recheck missed): respawns=%d prompts=%v", respawns, prompts)
	}
	if !inst.LimitReached() {
		t.Fatal("a pre-respawn deferral must leave the session parked at the limit wall")
	}
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeFromLimit_ManualRPCExemptFromAccountSwapAttachPause pins the
// operator-initiated exemption for the pre-commit, pre-stop, and pre-respawn
// rechecks: the explicit ResumeFromLimit RPC reaches resumeFromLimitLockedOutcome
// with operatorInitiated=true, so every attach-lease recheck in the body is
// skipped exactly like the send-boundary recheck. The manual retry must still
// respawn and deliver immediately even when the lease is acquired in those
// windows, because the operator initiated the action.
func TestResumeFromLimit_ManualRPCExemptFromAccountSwapAttachPause(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	// alive=false drives the exited-agent respawn arm so the pre-respawn recheck
	// (inside if shouldRespawn) is reached on the operator-initiated path.
	manager, repoID, inst, backend := newAutoResumeManager(t, "", false, "manual retry work", resetAt)
	// No clock advance needed: the manual RPC is an explicit operator action and
	// ignores the scheduler's due-time gate too.
	_ = advance

	// Attach the lease at every destructive-boundary hook the body reaches on the
	// non-swap respawn path (the pre-stop hook after the liveness probe, and the
	// pre-respawn hook before the respawn). The pre-commit hook is inside the
	// account-swap block this path never enters, so it does not fire here.
	prevStopHook := testHookResumeBeforeAccountSwapStop
	testHookResumeBeforeAccountSwapStop = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeAccountSwapStop = prevStopHook })
	prevRespawnHook := testHookResumeBeforeAccountSwapRespawn
	testHookResumeBeforeAccountSwapRespawn = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeAccountSwapRespawn = prevRespawnHook })

	if _, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("manual ResumeFromLimit RPC must succeed even when the lease is acquired at the destructive boundaries (operator action): %v", err)
	}

	_, respawns, prompts := backend.snapshot()
	if respawns != 1 {
		t.Fatalf("an exited-agent manual retry must respawn despite the attach lease, got %d", respawns)
	}
	if len(prompts) != 1 || prompts[0] != "manual retry work" {
		t.Fatalf("the manual retry must deliver immediately despite the destructive-boundary lease: prompts=%v", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("the manual retry must clear the limit liveness on delivery")
	}
}
