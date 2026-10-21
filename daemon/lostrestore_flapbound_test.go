package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// restore off-ramp, naming THIS session. The title rides positionally, so
	// the hint terminates options before it (#3432, #5436).
	hint := shellsuggest.PositionalCommand("af", []string{"sessions", "restore"}, inst.Title)
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

// #5238 review: a MANUAL restore of a remote session can succeed through the
// probe-alive heal — the sandbox answers alive, the Lost mark clears, and the
// function returns BEFORE the respawn path's armRestoreConfirmation call that
// resets the flap history. The off-ramp contract (an operator restore wipes
// the confirmed-restore window) must hold on that arm too: otherwise a stale
// history outlives the operator's intervention, and the next automatic cycle
// hands the session a give-up verdict earned by runtimes the operator already
// signed off on.
func TestRestoreSession_RemoteProbeAliveClearsFlapHistory(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/alive" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-heal", srv.URL, session.Lost)
	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores-1)

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-heal", RepoID: repoID}); err != nil {
		t.Fatalf("manual remote restore through the probe-alive heal: %v", err)
	}
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("manual probe-alive restore left %d stale confirmations: the operator off-ramp must reset the flap history on every successful path, want 0", got)
	}
}

// #5238 review: for a remote probeAnsweredDead session the preserve-push arm
// sits BEFORE the irreversible respawn, so a bound check placed only at the
// respawn edge can be starved forever — every pass spends its attempt on a
// push that keeps failing and returns before the bound is ever consulted, and
// the #1892 watch-task slot stays held for the life of the outage. Once the
// confirmations fill the window the terminal verdict must land before the
// push is even attempted.
func TestRestoreLostSessions_RemotePreserveRespectsFlapBound(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, repoID, repoPath := newStatusTestManager(t)

	srv, url := newPreservePushServer(t, false /* archive fails persistently */)
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-flap-preserve", url, session.Lost)
	seedFlapHistory(manager, stableSessionKey(repoID, inst), lostRestoreFlapMaxRestores)

	manager.RestoreLostSessions()

	if got := srv.calls(); got != 0 {
		t.Fatalf("a flap-bound session still spent %d preserve pushes: the bound must verdict before preservation is attempted, want 0", got)
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("a flap-bound session was re-provisioned: Recover calls = %d, want 0", got)
	}
	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil {
		t.Fatal("a remote session at the flap bound kept preserving instead of reaching the durable give-up")
	}
	if !strings.Contains(data.LostRestoreFailure.Error, "confirmed restored") {
		t.Fatalf("failure = %q, want the flap-specific confirmed-restored diagnostic", data.LostRestoreFailure.Error)
	}
}

// #5238 review: the advertised off-ramp must not promise a retry the durable
// gate refuses. The flap verdict used to leave the pass's fresh retry episode
// in place, so a manual restore whose Recover then failed recorded "attempt 1"
// and armed a backoff LostRestoreGaveUp makes unreachable — a lying log line
// and a slot the session never stops holding. The episode must close with the
// verdict, and a failure after it must re-seed terminal.
func TestRestoreSession_ManualFailureAfterFlapGiveUpStaysTerminal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "manual-after-flap", backend, true, session.Lost)
	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores)

	manager.RestoreLostSessions()
	if inst.ToInstanceData().LostRestoreFailure == nil {
		t.Fatal("setup: the flap bound did not produce its durable give-up")
	}
	manager.mu.Lock()
	_, stale := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if stale {
		t.Fatal("the flap verdict left a retry episode in place: a later failure would reuse its zeroed budget")
	}

	// The operator takes the advertised off-ramp and the respawn itself fails —
	// a structural Recover failure after the durable verdict must STAY
	// terminal, not arm "retrying in …" for a retry the gate refuses to run.
	backend.failWith = errors.New("sandbox rebuild refused")
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual-after-flap", RepoID: repoID}); err == nil {
		t.Fatal("the manual restore's Recover failure should surface to the operator")
	}

	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("the failed manual restore left no retry episode to inspect")
	}
	if st.consecutiveFailures < lostRestoreMaxAttempts {
		t.Fatalf("a failed manual off-ramp recorded attempt %d and arms a retry the gate refuses: the episode must stay terminal at %d",
			st.consecutiveFailures, lostRestoreMaxAttempts)
	}
	if !st.nextAttempt.IsZero() {
		t.Fatalf("a failed manual off-ramp armed a retry for %v that LostRestoreGaveUp refuses to run", st.nextAttempt)
	}
}

// The same floor covers the restart path: a daemon restart drops the in-memory
// episode but keeps the durable verdict, whose Attempts count is the evidence
// that earned it — 4 confirmations for the flap bound, not a structural budget
// burn — so a post-restart manual failure re-seeds terminal rather than
// restarting the budget at the recorded count (#5238 review).
func TestRecordLostRestoreFailure_PersistedFlapVerdictSeedsTerminal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "post-restart-flap", backend, true, session.Lost)
	key := stableSessionKey(repoID, inst)
	inst.SetLostRestoreFailure(lostRestoreFlapMaxRestores, errors.New("the flap verdict"))

	manager.recordLostRestoreFailure(daemonInstanceKey(repoID, "post-restart-flap"), repoID, inst,
		errors.New("manual restore failed"), lostRestoreManual)

	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("the failed manual restore left no retry episode to inspect")
	}
	if st.consecutiveFailures < lostRestoreMaxAttempts {
		t.Fatalf("a persisted flap verdict seeded a sub-budget episode at %d — every further failure must stay terminal at %d",
			st.consecutiveFailures, lostRestoreMaxAttempts)
	}
	if !st.nextAttempt.IsZero() {
		t.Fatalf("the seeded episode armed a retry for %v the durable gate refuses to run", st.nextAttempt)
	}
}

// The archive-restore off-ramp must close the flap episode too (#5238 review).
// A session that flapped, was archived, and is restored through `af sessions
// restore` between two sweep ticks never passes through a live-edge the sweep
// can observe — archived rows are not in its live set — so the pre-archive
// confirmations ride forward onto the runtime the operator just replaced, and
// the next loss can hit the bound on its first automatic pass. The restore
// succeeding is the episode's close, the same contract the manual heal keeps.
func TestRestoreArchived_ArchiveRestoreClearsConfirmedFlapHistory(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	if _, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID}); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores-1)

	if _, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("confirmed-flap history survived a successful archive restore: count = %d, want 0 — "+
			"the next loss would inherit %d/%d toward the bound on a runtime the operator just replaced",
			got, got, lostRestoreFlapMaxRestores)
	}
}

// The remote arm of the same off-ramp: an archived sandbox session restored by
// re-provision closes the flap episode its pre-archive confirmations counted.
func TestRestoreArchived_RemoteReprovisionClearsConfirmedFlapHistory(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-archived", "http://127.0.0.1:1", session.Running)

	key := stableSessionKey(repoID, inst)
	seedFlapHistory(manager, key, lostRestoreFlapMaxRestores-1)

	inst.SetStatusForTest(session.Archived)
	if _, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "remote-archived", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	if got := flapCount(manager, key); got != 0 {
		t.Fatalf("confirmed-flap history survived a remote archive restore: count = %d, want 0", got)
	}
}

// The give-up's advertised off-ramp must survive a title that begins with a
// dash (#5436): `af sessions restore -worker` makes pflag read the title as a
// flag and the command the operator runs back is the broken one. The hint is
// built with PositionalCommand so every title rides behind `--`.
func TestRestoreLostSessions_ConfirmedFlapGiveUpHintTerminatesOptions(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "-flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	for cycle := 0; cycle < lostRestoreFlapMaxRestores; cycle++ {
		confirmedRestoreCycle(t, manager, repoID, inst)
	}
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()

	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil {
		t.Fatal("terminal session surfaced no restore failure")
	}
	hint := shellsuggest.PositionalCommand("af", []string{"sessions", "restore"}, inst.Title)
	if !strings.Contains(data.LostRestoreFailure.Error, hint) {
		t.Fatalf("surfaced flap reason = %q, want the off-ramp behind an option terminator %q",
			data.LostRestoreFailure.Error, hint)
	}
	if strings.Contains(data.LostRestoreFailure.Error, "restore "+inst.Title) {
		t.Fatalf("dash-leading title surfaced unterminated in %q — pflag would read it as a flag",
			data.LostRestoreFailure.Error)
	}
}

// An automatic restore's pending confirmation arm must not survive the
// archive off-ramp (#5436): when `af sessions archive` + `af sessions
// restore` land while the previous automatic restore is still
// awaitingConfirm, the new runtime's first poll answers charge the STALE
// arm's confirmCountsFlap — one flap datapoint the operator's explicit
// off-ramp never earned. The restore succeeding retires the whole episode
// entry, the same contract the manual heal keeps.
func TestRestoreArchived_ArchiveRestoreRetiresPendingConfirmation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "pending-arm")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	if _, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "pending-arm", RepoID: repoID}); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	key := stableSessionKey(repoID, inst)
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{awaitingConfirm: true, confirmCountsFlap: true}
	manager.mu.Unlock()

	if _, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "pending-arm", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if st != nil {
		t.Fatalf("the stale awaitingConfirm arm survived a successful archive restore: %+v — "+
			"the new runtime's first confirmations would charge its flap datapoint", st)
	}
}

// The remote arm of the same off-ramp: an archived sandbox session restored
// by re-provision retires the pending automatic-restore arm it carried.
func TestRestoreArchived_RemoteReprovisionRetiresPendingConfirmation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-pending-arm", "http://127.0.0.1:1", session.Running)

	key := stableSessionKey(repoID, inst)
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{awaitingConfirm: true, confirmCountsFlap: true}
	manager.mu.Unlock()

	inst.SetStatusForTest(session.Archived)
	if _, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "remote-pending-arm", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if st != nil {
		t.Fatalf("the stale awaitingConfirm arm survived a remote archive restore: %+v", st)
	}
}
