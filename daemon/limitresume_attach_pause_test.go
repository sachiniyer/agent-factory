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

// TestResumeLimitedSessions_MidPassAttachPauseDeferralLeavesBackoffUntouched
// pins the attempt-state rollback for a mid-pass attach-pause deferral (#5422
// review): the send-boundary recheck runs AFTER limitResumeAttempted has recorded
// the attempt, so without the rollback a deferral here would leave the retry gate
// charged and the scheduler would wait for the exponential backoff after detach
// instead of re-firing on the next tick — a genuine failure never attempted.
// resumeDeferredByAttachPause rolls the attempt state back so the next unpaused
// tick re-fires immediately, exactly like the top-level gate deferral.
func TestResumeLimitedSessions_MidPassAttachPauseDeferralLeavesBackoffUntouched(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "auto-resume work", resetAt)
	advance(limitResumeGrace + time.Second)

	stateKey := stableSessionKey(repoID, inst)
	// The lease is NOT held when ResumeLimitedSessions starts, so the top-level
	// gate passes and limitResumeAttempted records the attempt. The send-boundary
	// hook attaches it inside the body, after the attempt was charged — the
	// window the rollback exists for.
	prevHook := testHookResumeBeforeSend
	testHookResumeBeforeSend = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeSend = prevHook })

	manager.ResumeLimitedSessions()

	// The deferral must have rolled the attempt state back: no backoff step
	// charged, so the next unpaused tick re-fires immediately.
	manager.mu.Lock()
	st := manager.limitResumeStates[stateKey]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("a mid-pass deferral must keep the retry state so the scheduler re-fires on the next unpaused tick")
	}
	if st.attempts != 0 {
		t.Fatalf("a mid-pass attach-pause deferral must not burn a backoff step, got attempts=%d", st.attempts)
	}

	// Detach: the resume fires on the very next tick — no backoff wait, because
	// the deferral rolled the attempt state back. Clear the hook first so the
	// send-boundary recheck does not re-attach the lease on the second pass.
	testHookResumeBeforeSend = prevHook
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
// attempt. A mid-pass attach-pause deferral rolls the attempt state back
// (resumeDeferredByAttachPause), so the next tick after detach re-fires
// immediately instead of honoring the backoff a genuine failed attempt would
// (#5422 review). This test asserts the deferral itself; the companion
// TestResumeLimitedSessions_MidPassAttachPauseDeferralLeavesBackoffUntouched
// pins the attempt-state rollback.
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

// TestResumeLimitedSessions_AttachPauseAfterRemotePreserveDefers pins the
// attach-lease recheck after the remote-sandbox preservation push (#5422 review):
// a probeAnsweredDead remote resume runs preserveSandboxBeforeReap and
// requireDurableSandboxBranch (a long network round-trip) before the pane-stop
// and respawn arms, and PauseStatusPoll takes only pausedMu — not the target or
// op locks the resume body holds — so a TUI can attach and renew the lease
// during that push. The pre-stop recheck at the top of the switch ran before the
// liveness probe, so it does not cover this window. The recheck immediately after
// the preservation (gated on !operatorInitiated, like the other rechecks) defers
// the automated resume so the stop/respawn cannot replace a runtime a TUI just
// attached to. A deferral returns resumeNotPerformed without clearing the limit,
// so the scheduler re-fires on the next tick once the lease lapses.
func TestResumeLimitedSessions_AttachPauseAfterRemotePreserveDefers(t *testing.T) {
	// A remote sandbox whose agent exited (alive=false) drives the
	// probeAnsweredDead arm, which runs the remote-sandbox preservation push
	// (preserveSandboxBeforeReap/requireDurableSandboxBranch) before the
	// stop/respawn arms. No account swap is configured, so this path exercises
	// the no-swap respawn arm; the recheck after the preservation gates the
	// same destructive replacement the stop arms would gate with a swap.
	oldSandbox := newMockSandbox(t, false) // answers: the agent exited
	freshSandbox := newMockSandbox(t, true)
	manager, repoID, inst, backend := newRemoteLimitedSession(t, oldSandbox, freshSandbox, "finish the migration")
	manager.cfg.LimitAutoResume = true

	// The lease is NOT held when ResumeLimitedSessions starts, so the top-level
	// gate and the pre-stop recheck pass. The hook attaches it inside the body,
	// after the remote-sandbox preservation push and before the post-preserve
	// recheck — the window between the preservation's network round-trip and the
	// stop/respawn arms.
	prevHook := testHookResumeAfterRemotePreserve
	testHookResumeAfterRemotePreserve = func() {
		manager.PauseStatusPoll(repoID, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeAfterRemotePreserve = prevHook })

	manager.ResumeLimitedSessions()

	if got := backend.respawnCount(); got != 0 {
		t.Fatalf("BUG: the auto-resume scheduler respawned after a TUI attached during the remote-sandbox preservation (post-preserve recheck missed): respawns=%d", got)
	}
	if got := freshSandbox.gotPrompts(); len(got) != 0 {
		t.Fatalf("BUG: the auto-resume scheduler delivered a prompt after a TUI attached during the remote-sandbox preservation: prompts=%v", got)
	}
	if !inst.LimitReached() {
		t.Fatal("a post-preserve deferral must leave the session parked at the limit wall")
	}
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeAccountSwapTeardownClearsLaunch pins the
// in-commit teardown recheck's ClearUncommittedAccountSwapLaunch (#5422 review): the
// admission (ValidateAccountSwap) stores accountSwapLaunch before the recheck, and
// tabSpawnBlockedLocked refuses tab spawns while it is set. The explicit clear on the
// deferral (in addition to EndLimitResume's conditional clear) ensures an attached TUI
// is not blocked from adding or attaching tabs after the deferral.
func TestResumeLimitedSessions_AttachPauseBeforeAccountSwapTeardownClearsLaunch(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)
	configureLimitAccountCandidate(t, manager, "work")

	advance(time.Second)

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
	// accountSwapLaunch was staged by the admission (ValidateAccountSwap) before the
	// recheck deferred. The explicit ClearUncommittedAccountSwapLaunch on the deferral
	// (and EndLimitResume's conditional clear) must retire it so tab spawns are not
	// blocked while the lease is held.
	if err := inst.TabSpawnBlocked(); err != nil {
		t.Fatalf("a pre-teardown deferral must clear the staged account-swap launch plan so an attached TUI is not blocked: %v", err)
	}
}

// TestResumeLimitedSessions_AttachPauseAfterRepairValidationDefers pins the
// post-validation attach-lease recheck in the probeAlive arm (#5422 review): a
// committed-swap recovery (alreadySet) with an incomplete pane set re-runs
// ValidateAccountSwap before StopForAccountSwap, and a TUI can attach during that
// validation. The recheck after ValidateAccountSwap (gated on !operatorInitiated)
// defers the repair so the stop cannot tear down a runtime a TUI just attached to.
func TestResumeLimitedSessions_AttachPauseAfterRepairValidationDefers(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, true)
	// The handoff replaced the instance in m.instances; resolve the current one.
	inst2, _, _, _, _, err := m.resolveActionSession("", inst.Title, repo)
	if err != nil || inst2 == nil {
		t.Fatalf("resolve current instance: %v", err)
	}
	inst2.SetAccountSwapReplacementPanesStartedForTest(false)
	key := daemonInstanceKey(repo, inst2.Title)
	swap := committedAccountSwap(inst2)

	prevHook := testHookResumeAfterRepairValidation
	testHookResumeAfterRepairValidation = func() {
		m.PauseStatusPoll(repo, inst2.Title, inst2.ID)
	}
	t.Cleanup(func() { testHookResumeAfterRepairValidation = prevHook })

	_, respawnsBefore, _ := backend.snapshot()
	outcome, err := m.resumeFromLimitLockedWithAccount(repo, key, inst2, inst2.Title, swap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != resumeDeferredByAttachPause {
		t.Fatalf("BUG: the scheduler resumed after a TUI attached during the repair validation (post-validation recheck missed): outcome=%v", outcome)
	}
	_, respawns, prompts := backend.snapshot()
	if respawns != respawnsBefore || len(prompts) != 0 {
		t.Fatalf("BUG: the scheduler repaired and respawned after a TUI attached during the repair validation: respawns=%d prompts=%v", respawns, prompts)
	}
	if !m.isPollPaused(repo, inst2.Title, inst2.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeLimitedSessions_AttachPauseAfterGoneRevalidationDefers pins the
// post-validation attach-lease recheck in the probeAbsent arm (#5422 review): a
// committed-swap recovery with an absent agent re-runs revalidateGoneAccountSwap
// (which calls ValidateAccountSwap) before StopRemainingPanesForAccountSwap, and a
// TUI can attach during that validation. The recheck after revalidateGoneAccountSwap
// (gated on !operatorInitiated) defers the recovery so the stop cannot tear down a
// runtime a TUI just attached to.
func TestResumeLimitedSessions_AttachPauseAfterGoneRevalidationDefers(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, false)
	inst2, _, _, _, _, err := m.resolveActionSession("", inst.Title, repo)
	if err != nil || inst2 == nil {
		t.Fatalf("resolve current instance: %v", err)
	}
	key := daemonInstanceKey(repo, inst2.Title)
	swap := committedAccountSwap(inst2)

	prevHook := testHookResumeAfterGoneRevalidation
	testHookResumeAfterGoneRevalidation = func() {
		m.PauseStatusPoll(repo, inst2.Title, inst2.ID)
	}
	t.Cleanup(func() { testHookResumeAfterGoneRevalidation = prevHook })

	_, respawnsBefore, _ := backend.snapshot()
	outcome, err := m.resumeFromLimitLockedWithAccount(repo, key, inst2, inst2.Title, swap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != resumeDeferredByAttachPause {
		t.Fatalf("BUG: the scheduler resumed after a TUI attached during the gone revalidation (post-validation recheck missed): outcome=%v", outcome)
	}
	_, respawns, prompts := backend.snapshot()
	if respawns != respawnsBefore || len(prompts) != 0 {
		t.Fatalf("BUG: the scheduler respawned or delivered after a TUI attached during the gone revalidation: respawns=%d prompts=%v", respawns, prompts)
	}
	if !m.isPollPaused(repo, inst2.Title, inst2.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeManualMissionSendDefers pins the
// pre-submission attach-lease recheck inside deliverManualAccountMission (#5422
// review): the send-boundary recheck in resumeFromLimitLockedOutcome runs before
// deliverManualAccountMission, but the durable settlement write and the readiness
// wait inside WaitForReadyAndSubmitPrompt can both outlast it. A TUI can attach
// during either, and the mission would be submitted without another attach check.
// The pre-submission recheck (via WaitForReadyAndSubmitPrompt's beforeSubmit, gated
// on !operatorInitiated) defers the mission so the prompt is not pasted into an
// attached pane. The limit is re-parked and the pending mission delivery state is
// restored, so the scheduler re-fires on the next tick.
func TestResumeLimitedSessions_AttachPauseBeforeManualMissionSendDefers(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, false)
	inst2, _, _, _, _, err := m.resolveActionSession("", inst.Title, repo)
	if err != nil || inst2 == nil {
		t.Fatalf("resolve current instance: %v", err)
	}
	key := daemonInstanceKey(repo, inst2.Title)
	swap := committedAccountSwap(inst2)

	prevHook := testHookResumeBeforeManualMissionSend
	testHookResumeBeforeManualMissionSend = func() {
		m.PauseStatusPoll(repo, inst2.Title, inst2.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeManualMissionSend = prevHook })

	_, respawnsBefore, _ := backend.snapshot()
	outcome, err := m.resumeFromLimitLockedWithAccount(repo, key, inst2, inst2.Title, swap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != resumeDeferredByAttachPause {
		t.Fatalf("BUG: the scheduler delivered the manual mission after a TUI attached during the readiness wait (pre-submission recheck missed): outcome=%v", outcome)
	}
	_, respawns, prompts := backend.snapshot()
	// The respawn happens before the pre-submission recheck (the recheck is inside
	// deliverManualAccountMission, after the settlement write and the readiness wait),
	// so respawns may increase; the recheck defers the mission delivery, not the respawn.
	if len(prompts) != 0 {
		t.Fatalf("BUG: the scheduler delivered a prompt after a TUI attached during the readiness wait (pre-submission recheck missed): prompts=%v", prompts)
	}
	if respawns != respawnsBefore {
		// A repair-path respawn may have happened before the recheck; only the prompt
		// delivery is gated. Assert no NEW prompt was delivered.
		if len(prompts) != 0 {
			t.Fatalf("the pre-submission recheck must defer the mission delivery: prompts=%v", prompts)
		}
	}
	// A pre-submission deferral must not re-park a healthy committed swap to
	// LiveLimitReached: the row was LiveRunning (accountSwapScheduledResumeEligible
	// admits a committed swap that is not limit-blocked), so re-parking it would
	// mislabel a healthy handoff as quota-blocked. Preserve the liveness instead,
	// and keep the pending swap pending so the scheduler re-fires via
	// accountSwapScheduledResumeEligible on the preserved liveness (#5422 review).
	if inst2.LimitReached() {
		t.Fatal("a pre-submission deferral must not re-park a healthy committed swap (the row was LiveRunning, not limit-blocked)")
	}
	if _, _, pending := inst2.PendingAccountSwap(); !pending {
		t.Fatal("a pre-submission deferral must keep the pending manual swap pending so the scheduler re-fires")
	}
	if !m.isPollPaused(repo, inst2.Title, inst2.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
}

// TestResumeFromLimit_ManualRPCExemptFromManualMissionSendAttachPause pins the
// operator-initiated exemption at the pre-submission recheck: the explicit
// ResumeFromLimit RPC reaches deliverManualAccountMission with operatorInitiated=true,
// so the pre-submission recheck is skipped and the manual retry delivers despite the
// lease being acquired in the readiness window.
func TestResumeFromLimit_ManualRPCExemptFromManualMissionSendAttachPause(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, false)

	prevHook := testHookResumeBeforeManualMissionSend
	testHookResumeBeforeManualMissionSend = func() {
		m.PauseStatusPoll(repo, inst.Title, inst.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeManualMissionSend = prevHook })

	_, respawnsBefore, _ := backend.snapshot()
	if _, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}); err != nil {
		t.Fatalf("manual ResumeFromLimit RPC must succeed even when the lease is acquired at the pre-submission boundary (operator action): %v", err)
	}
	_, respawns, prompts := backend.snapshot()
	if respawns != respawnsBefore+1 {
		t.Fatalf("an exited-agent manual retry must respawn despite the attach lease, got %d", respawns)
	}
	if len(prompts) != 1 {
		t.Fatalf("the manual retry must deliver immediately despite the pre-submission lease: prompts=%v", prompts)
	}
}

// TestResumeLimitedSessions_AttachPauseBeforeManualMissionSendDefersLimitBlocked pins
// the limit-identity restoration on a limit-blocked manual swap's attach-pause
// deferral (#5422 review): when the committed swap was parked at the limit by
// ParkManualAccountSwapAtLimit, ClearLimitReached in deliverManualAccountMission
// erases limitAgent/limitAccount, and the re-park (ReparkLimitUnderResumeFence)
// restored only the liveness and reset window. Without RestoreLimitIdentityUnderResumeFence
// the re-parked row persisted LiveLimitReached with no incoming-account attribution,
// so LimitIdentity and the reloaded session no longer identified which account
// produced the current wall. The deferral must restore the identity alongside the
// re-park so the durable row keeps the incoming-wall attribution.
func TestResumeLimitedSessions_AttachPauseBeforeManualMissionSendDefersLimitBlocked(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, true)
	inst2, _, _, _, _, err := m.resolveActionSession("", inst.Title, repo)
	if err != nil || inst2 == nil {
		t.Fatalf("resolve current instance: %v", err)
	}
	key := daemonInstanceKey(repo, inst2.Title)
	swap := committedAccountSwap(inst2)

	// Park the committed swap at the limit so the row is LiveLimitReached with the
	// incoming account's identity, the state ParkManualAccountSwapAtLimit records.
	// BeginManualAccountSwap raises the OpRespawning fence ParkManualAccountSwapAtLimit
	// requires; EndLimitResume releases it so the resume can re-acquire it.
	resetAt := time.Now().Add(time.Hour)
	if err := inst2.BeginManualAccountSwap(); err != nil {
		t.Fatalf("BeginManualAccountSwap: %v", err)
	}
	if err := inst2.ParkManualAccountSwapAtLimit(resetAt); err != nil {
		t.Fatalf("ParkManualAccountSwapAtLimit: %v", err)
	}
	inst2.EndLimitResume()
	if !inst2.LimitReached() {
		t.Fatal("the committed swap must be parked at the limit before the resume")
	}
	parkedAgent, parkedAccount, parkedOK := inst2.LimitIdentity()
	if !parkedOK || parkedAgent == "" || parkedAccount == "" {
		t.Fatalf("ParkManualAccountSwapAtLimit must attribute the limit identity: agent=%q account=%q", parkedAgent, parkedAccount)
	}

	prevHook := testHookResumeBeforeManualMissionSend
	testHookResumeBeforeManualMissionSend = func() {
		m.PauseStatusPoll(repo, inst2.Title, inst2.ID)
	}
	t.Cleanup(func() { testHookResumeBeforeManualMissionSend = prevHook })

	outcome, err := m.resumeFromLimitLockedWithAccount(repo, key, inst2, inst2.Title, swap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != resumeDeferredByAttachPause {
		t.Fatalf("BUG: the scheduler delivered the manual mission after a TUI attached during the readiness wait (pre-submission recheck missed): outcome=%v", outcome)
	}
	_, _, prompts := backend.snapshot()
	if len(prompts) != 0 {
		t.Fatalf("BUG: the scheduler delivered a prompt after a TUI attached during the readiness wait (pre-submission recheck missed): prompts=%v", prompts)
	}
	if !m.isPollPaused(repo, inst2.Title, inst2.ID) {
		t.Fatal("the attach lease must still be held after the deferral")
	}
	// The re-park must restore the limit identity ClearLimitReached erased, not just
	// the liveness and reset window: a LiveLimitReached row with no incoming-account
	// attribution loses which account produced the current wall (#5422 review).
	if !inst2.LimitReached() {
		t.Fatal("a limit-blocked deferral must re-park the limit (the row was LiveLimitReached, not LiveRunning/LiveReady)")
	}
	gotReset, gotResetOK := inst2.LimitResetAt()
	if !gotResetOK || !gotReset.Equal(resetAt) {
		t.Fatalf("the re-park must restore the reset window: got %v ok=%v want %v", gotReset, gotResetOK, resetAt)
	}
	gotAgent, gotAccount, gotOK := inst2.LimitIdentity()
	if !gotOK {
		t.Fatal("the re-park must restore the limit identity (LimitIdentity returned ok=false after the deferral)")
	}
	if gotAgent != parkedAgent || gotAccount != parkedAccount {
		t.Fatalf("the re-park must restore the incoming-wall attribution: agent=%q account=%q want agent=%q account=%q", gotAgent, gotAccount, parkedAgent, parkedAccount)
	}
	if _, _, pending := inst2.PendingAccountSwap(); !pending {
		t.Fatal("a pre-submission deferral must keep the pending manual swap pending so the scheduler re-fires")
	}
}
