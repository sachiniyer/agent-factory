package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// #5238: a runtime that survives the liveness-confirmation window and then
// exits retires its restore episode debt-free — the episode is CONFIRMED and
// deleted, so the consecutive-failure budget that bounds failed restores can
// never accumulate across confirmed-then-lost cycles. The codex startup
// update-dialog death is the observed shape: the agent answers plenty of
// polls, exits ~15s after restore, and the loop repeats forever. The confirmed-
// flap bound gives that cycle the same terminal verdict the structural budget
// gives startup failures.

// confirmedRestoreCycle drives one lost→restored→CONFIRMED→lost cycle and
// leaves the row Lost. The confirmation retires at the sweep's live-edge (the
// row still reads Running when the sweep looks), which is the retirement that
// used to erase all memory of the cycle. The backend is assumed to mark the
// row Running on Recover (recoverFakeBackend's contract).
func confirmedRestoreCycle(t *testing.T, m *Manager, repoID string, inst *session.Instance) {
	t.Helper()
	inst.SetStatusForTest(session.Lost)
	m.RestoreLostSessions() // spawn; arms confirmation
	confirmAlive(m, repoID, inst)
	m.RestoreLostSessions() // sweep confirms and retires the episode
}

// flapCount reads the recorded confirmed-restore history for a session.
func flapCount(m *Manager, key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lostRestoreConfirmations[key])
}

// seedFlapHistory writes n confirmed-restore timestamps directly, so a test
// can start mid-window without replaying full cycles first.
func seedFlapHistory(m *Manager, key string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		m.noteLostRestoreConfirmationLocked(key, nowFunc())
	}
}

// TestRestoreLostSessions_ConfirmedFlapGivesUpAtBound is the reported bug end
// to end: an agent that survives confirmation and exits right after used to
// loop lost→restored→lost forever. At the bound the loop must produce the
// durable give-up — with the flap reason and the operator's explicit way back —
// and never spawn again.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: every cycle logs "restored after N
// attempt(s)", the retry state is deleted at confirmation, and Recover is
// called once per pass forever with no give-up ever firing.
func TestRestoreLostSessions_ConfirmedFlapGivesUpAtBound(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "confirmed-flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	key := stableSessionKey(repoID, inst)

	for cycle := 0; cycle < lostRestoreFlapMaxRestores; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	if got := flapCount(manager, key); got != lostRestoreFlapMaxRestores {
		t.Fatalf("confirmed restores recorded = %d, want %d", got, lostRestoreFlapMaxRestores)
	}
	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores {
		t.Fatalf("recovery spawns = %d before the bound pass, want %d", got, lostRestoreFlapMaxRestores)
	}

	// The next loss must NOT respawn: the bound turns it into the durable
	// terminal verdict.
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores {
		t.Fatalf("a confirmed flap past the bound respawned again: spawns = %d", got)
	}
	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil {
		t.Fatalf("terminal session surfaced no restore failure: %#v", data)
	}
	if data.LostRestoreFailure.Attempts != lostRestoreFlapMaxRestores {
		t.Fatalf("surfaced attempts = %d, want %d", data.LostRestoreFailure.Attempts, lostRestoreFlapMaxRestores)
	}
	// The diagnostic must tell the operator what to do next — the explicit
	// restore off-ramp, naming THIS session.
	hint := shellsuggest.Command("af", "sessions", "restore", inst.Title)
	if !strings.Contains(data.LostRestoreFailure.Error, hint) {
		t.Fatalf("surfaced flap reason = %q, want it to name the operator off-ramp %q",
			data.LostRestoreFailure.Error, hint)
	}
	if data.IdleReason != session.IdleReasonRestoreGaveUp {
		t.Fatalf("idle reason = %q, want %q", data.IdleReason, session.IdleReasonRestoreGaveUp)
	}
	want := fmt.Sprintf("giving up after %d confirmed restores in %s", lostRestoreFlapMaxRestores, lostRestoreFlapWindow)
	if !strings.Contains(ownLogs.errors.String(), want) {
		t.Fatalf("missing terminal %q log for a confirmed flapper; logs:\n%s", want, ownLogs.errors.String())
	}

	// And it stays stopped: later passes never spawn again.
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores {
		t.Fatalf("gave-up session respawned again: spawns = %d", got)
	}
}

// TestRestoreLostSessions_ConfirmedFlapGiveUpViaLostEdge covers the other
// retirement: the agent dies AFTER accumulating enough answers but BEFORE a
// live-edge sweep can retire the episode — the confirmation is then detected
// on the next Lost pass itself. That is the #5238 shape in miniature (agent
// exits seconds after restore), and it must count the same way.
func TestRestoreLostSessions_ConfirmedFlapGiveUpViaLostEdge(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "lost-edge-flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions() // spawn 1, armed
	for cycle := 0; cycle < lostRestoreFlapMaxRestores; cycle++ {
		confirmAlive(manager, repoID, inst)
		inst.SetStatusForTest(session.Lost)
		manager.RestoreLostSessions() // confirms prior spawn, then restores or gives up
	}

	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores {
		t.Fatalf("recovery spawns = %d, want the loop stopped at %d confirmed restores", got, lostRestoreFlapMaxRestores)
	}
	if inst.ToInstanceData().LostRestoreFailure == nil {
		t.Fatal("a fast confirmed flapper produced no terminal restore failure")
	}
}

// TestRestoreLostSessions_ConfirmedFlapUnderBoundStillRestores pins the other
// side of the bound: fewer than lostRestoreFlapMaxRestores confirmed restores
// in the window keeps automatic recovery going — a session that genuinely
// came back is not penalized for a couple of unlucky losses.
func TestRestoreLostSessions_ConfirmedFlapUnderBoundStillRestores(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "unlucky", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	for cycle := 0; cycle < lostRestoreFlapMaxRestores-1; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores {
		t.Fatalf("recovery spawns = %d, want %d (under the bound the session still restores)",
			got, lostRestoreFlapMaxRestores)
	}
	if inst.ToInstanceData().LostRestoreFailure != nil {
		t.Fatal("the flap bound gave up before it was reached")
	}
}

// TestRestoreLostSessions_ManualRestoreClearsFlapHistory is the operator
// off-ramp contract: an explicit `af sessions restore` resets the flap window
// so working around a stuck session cannot immediately re-trip the bound, and
// the manual restore's own confirmation is not charged to it either.
func TestRestoreLostSessions_ManualRestoreClearsFlapHistory(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "manual-offramp", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	key := stableSessionKey(repoID, inst)

	for cycle := 0; cycle < lostRestoreFlapMaxRestores-1; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	if got := flapCount(manager, key); got != lostRestoreFlapMaxRestores-1 {
		t.Fatalf("setup: confirmed restores = %d, want %d", got, lostRestoreFlapMaxRestores-1)
	}

	inst.SetStatusForTest(session.Lost)
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, RepoID: repoID}); err != nil {
		t.Fatalf("manual restore of a flapping session failed: %v", err)
	}
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("flap history = %d after a manual restore, want it cleared", got)
	}
	// The manual arm's own confirmation retires without feeding the history —
	// the off-ramp is not charged against the automatic bound.
	confirmAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("a manual restore's confirmation was counted as a lost-restore cycle: %d", got)
	}

	// And a fresh window takes fresh cycles: bound-1 more confirmed restores
	// still restore rather than giving up.
	for cycle := 0; cycle < lostRestoreFlapMaxRestores-1; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure != nil {
		t.Fatal("the flap bound fired inside a window the manual restore had reset")
	}
	if got := flapCount(manager, key); got != lostRestoreFlapMaxRestores-1 {
		t.Fatalf("flap history = %d after %d fresh cycles, want %d",
			got, lostRestoreFlapMaxRestores-1, lostRestoreFlapMaxRestores-1)
	}
}

// TestRestoreLostSessions_ManualRestoreStillWorksAfterFlapGiveUp keeps the
// other off-ramp guarantee: the durable flap give-up stops AUTOMATIC restores,
// never the explicit one.
func TestRestoreLostSessions_ManualRestoreStillWorksAfterFlapGiveUp(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "gave-up", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	key := stableSessionKey(repoID, inst)

	for cycle := 0; cycle < lostRestoreFlapMaxRestores; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure == nil {
		t.Fatal("setup: the flap bound never gave up")
	}

	inst.SetStatusForTest(session.Lost)
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, RepoID: repoID}); err != nil {
		t.Fatalf("the explicit off-ramp must still work after the automatic bound: %v", err)
	}
	if got := backend.recoverCalls(); got != lostRestoreFlapMaxRestores+1 {
		t.Fatalf("manual restore spawns = %d total, want %d", got, lostRestoreFlapMaxRestores+1)
	}
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("flap history = %d after the off-ramp restore, want it cleared", got)
	}
}

// TestRestoreLostSessions_FlapWindowExpiresOldConfirmations makes the window a
// WINDOW: confirmations older than lostRestoreFlapWindow stop counting, so a
// session with a bad afternoon is not penalized a week later.
func TestRestoreLostSessions_FlapWindowExpiresOldConfirmations(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "slow-flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	for cycle := 0; cycle < lostRestoreFlapMaxRestores-1; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	advance(lostRestoreFlapWindow + time.Minute)

	// A fresh window takes fresh cycles: bound-1 more still cannot trip it.
	for cycle := 0; cycle < lostRestoreFlapMaxRestores-1; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure != nil {
		t.Fatal("confirmations from an expired window tripped the flap bound")
	}
	if got := backend.recoverCalls(); got != 2*lostRestoreFlapMaxRestores-1 {
		t.Fatalf("recovery spawns = %d, want %d", got, 2*lostRestoreFlapMaxRestores-1)
	}
}

// TestResumeFromLimit_RespawnDoesNotFeedFlapHistory: the usage-limit auto-
// resume replaces the runtime through its own path and never arms a lost-
// restore confirmation — so it must not be counted as a lost-restore cycle,
// even though its respawned agent then answers liveness polls (#5238).
func TestResumeFromLimit_RespawnDoesNotFeedFlapHistory(t *testing.T) {
	manager, repoID, inst, backend := newAutoResumeManager(t, "", false, "finish the migration", time.Now().Add(-time.Hour))
	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores-1)

	if _, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("limit-resume respawn failed: %v", err)
	}
	if _, respawns, _ := backend.snapshot(); respawns != 1 {
		t.Fatalf("setup: respawns = %d, want 1 (the limit-resume replacement must have run)", respawns)
	}
	// The respawned runtime answers polls: still not a lost-restore
	// confirmation.
	confirmAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	if got := flapCount(manager, key); got != lostRestoreFlapMaxRestores-1 {
		t.Fatalf("a limit-resume respawn fed the flap history: %d, want %d",
			got, lostRestoreFlapMaxRestores-1)
	}

	// One more real loss still restores: the bound never got its fourth.
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure != nil {
		t.Fatal("a limit-resume respawn was charged as a confirmed lost-restore, tripping the bound early")
	}
}

// TestResumeLimitedSessions_AccountSwapDoesNotFeedFlapHistory: an account-swap
// respawn is the same class of replacement — the runtime it lands answers
// polls, but it was never a lost-session restore and must not count as one
// (#5238).
func TestResumeLimitedSessions_AccountSwapDoesNotFeedFlapHistory(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores-1)

	advance(time.Second)
	manager.ResumeLimitedSessions()

	if _, respawns, _ := backend.snapshot(); respawns != 1 {
		t.Fatalf("setup: account replacement respawns = %d, want 1 (the swap path must have run)", respawns)
	}
	confirmAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	if got := flapCount(manager, key); got != lostRestoreFlapMaxRestores-1 {
		t.Fatalf("an account-swap respawn fed the flap history: %d, want %d",
			got, lostRestoreFlapMaxRestores-1)
	}

	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure != nil {
		t.Fatal("an account-swap respawn was charged as a confirmed lost-restore, tripping the bound early")
	}
}
