package daemon

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
	"strings"
	"sync"
	"testing"
	"time"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #1910 regression lock: a Lost session whose recovery spawn SUCCEEDS but
// whose runtime dies before it can be confirmed alive must be treated as a FAILED
// attempt and backed off exponentially — not respawned at poll cadence forever.
//
// The field shape: 465 identical errors, a respawn roughly every two seconds for
// ~28 minutes. Recover returned nil every time (the tmux spawn genuinely worked),
// so lostRestoreFailed never saw an error and the #1108 backoff never armed.

// observeAlive fakes the daemon's one positive liveness observation — a poll whose
// probe got an ANSWER out of the runtime. Tests drive it explicitly because that,
// not elapsed time, is what confirms a restore (#1917 round 6).
func observeAlive(m *Manager, repoID string, inst *session.Instance) {
	m.noteAliveObservation(repoID, inst)
}

// diesOnSpawnBackend models the reported agent: its Recover SUCCEEDS — the spawn
// is real and returns nil — but the runtime does not survive, so the row is Lost
// again by the time the next poll looks. This is the exact case the old code read
// as a fresh loss episode rather than as a failed recovery.
type diesOnSpawnBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	recovers int
}

func (b *diesOnSpawnBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	b.recovers++
	b.mu.Unlock()
	// The spawn succeeded and the instance went live (LocalBackend.Recover's real
	// success contract: ConfirmLive is an in-memory edge, not a liveness probe)...
	inst.SetStatusForTest(session.Running)
	// ...and then the agent immediately exited, so the next poll finds it Lost.
	inst.SetStatusForTest(session.Lost)
	return nil
}

func (b *diesOnSpawnBackend) recoverCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

func (b *diesOnSpawnBackend) Type() string { return "local" }

func TestRestoreLostSessions_ImmediateExitLogsTerminalGiveUp(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "always-exits", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	var errors bytes.Buffer
	previous := aflog.ErrorLog
	aflog.ErrorLog = stdlog.New(&errors, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = previous })

	for i := 0; i < 2*lostRestoreMaxAttempts+4; i++ {
		manager.RestoreLostSessions()
	}

	want := fmt.Sprintf("giving up after %d attempts: %v", lostRestoreMaxAttempts, errRestoreDiedBeforeConfirm)
	if !strings.Contains(errors.String(), want) {
		t.Fatalf("missing terminal %q log after an always-exiting program; logs:\n%s", want, errors.String())
	}
	if got := backend.recoverCount(); got != lostRestoreMaxAttempts {
		t.Fatalf("recovery spawns = %d, want terminal stop after %d", got, lostRestoreMaxAttempts)
	}
	data := inst.ToInstanceData()
	if data.Liveness != session.LiveLost || data.LostRestoreFailure == nil {
		t.Fatalf("terminal session = %#v, want LiveLost with surfaced restore failure", data)
	}
	if data.LostRestoreFailure.Attempts != lostRestoreMaxAttempts || data.LostRestoreFailure.Error != errRestoreDiedBeforeConfirm.Error() {
		t.Fatalf("surfaced failure = %#v, want %d attempts and last startup error", data.LostRestoreFailure, lostRestoreMaxAttempts)
	}
	if data.IdleReason != session.IdleReasonRestoreGaveUp {
		t.Fatalf("idle reason = %q, want %q", data.IdleReason, session.IdleReasonRestoreGaveUp)
	}

	manager.RestoreLostSessions()
	if got := backend.recoverCount(); got != lostRestoreMaxAttempts {
		t.Fatalf("gave-up session retried again: recovery spawns = %d", got)
	}
}

func TestRestoreLostSessions_ConfirmedAliveLogsTerminalSuccess(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "healed", backend, true, session.Lost)

	var info bytes.Buffer
	previous := aflog.InfoLog
	aflog.InfoLog = stdlog.New(&info, "INFO: ", 0)
	t.Cleanup(func() { aflog.InfoLog = previous })

	manager.RestoreLostSessions()
	if strings.Contains(info.String(), "restored after") {
		t.Fatalf("spawn was logged as terminal recovery before liveness confirmation:\n%s", info.String())
	}
	observeAlive(manager, repoID, inst)
	manager.RestoreLostSessions()

	want := fmt.Sprintf("restore of lost session %q (repo %s): restored after 1 attempt", inst.Title, repoID)
	if !strings.Contains(info.String(), want) {
		t.Fatalf("missing terminal %q log after confirmed recovery; logs:\n%s", want, info.String())
	}
}

func TestRestoreSession_FailureAfterPersistedGiveUpStaysTerminal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	newFailure := errors.New("agent still exits at startup")
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: newFailure}
	inst := registerStarted(t, manager, repoID, repoPath, "still-broken", backend, true, session.Lost)
	inst.SetLostRestoreFailure(lostRestoreMaxAttempts, errors.New("previous startup failure"))

	var terminal bytes.Buffer
	previous := aflog.ErrorLog
	aflog.ErrorLog = stdlog.New(&terminal, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = previous })

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, RepoID: repoID}); !errors.Is(err, newFailure) {
		t.Fatalf("RestoreSession error = %v, want %v", err, newFailure)
	}
	wantAttempts := lostRestoreMaxAttempts + 1
	want := fmt.Sprintf("giving up after %d attempts: %v", wantAttempts, newFailure)
	if !strings.Contains(terminal.String(), want) {
		t.Fatalf("missing terminal %q after explicit retry failed; logs:\n%s", want, terminal.String())
	}
	failure := inst.ToInstanceData().LostRestoreFailure
	if failure == nil || failure.Attempts != wantAttempts || failure.Error != newFailure.Error() {
		t.Fatalf("surfaced failure = %#v, want updated terminal attempt", failure)
	}
}

// TestRestoreLostSessions_SpawnSucceedsButRuntimeDies_BacksOff drives many poll
// passes against a session that can never stay up, and asserts the loop does NOT
// respawn once per pass.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: restoreLostSession cleared the retry state on
// Recover success, and RestoreLostSessions' sweep cleared it again the moment the
// row read non-Lost — so every pass looked like attempt #1 with a zero backoff and
// recovers == passes. Against the unfixed loop this fails with 20 spawns.
func TestRestoreLostSessions_SpawnSucceedsButRuntimeDies_BacksOff(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)

	// A backoff long enough that, once armed, no further attempt is due within the
	// test. If it arms at all, attempts stop; if it never arms (the bug), every
	// pass respawns.
	prevBase, prevMax := lostRestoreBackoffBase, lostRestoreBackoffMax
	lostRestoreBackoffBase, lostRestoreBackoffMax = time.Hour, time.Hour
	t.Cleanup(func() { lostRestoreBackoffBase, lostRestoreBackoffMax = prevBase, prevMax })

	const passes = 20
	for i := 0; i < passes; i++ {
		manager.RestoreLostSessions()
	}

	got := backend.recoverCount()
	// One spawn, then one pass that observes the death and arms the backoff. Never
	// one spawn per pass — that is the hot loop.
	if got > 2 {
		t.Fatalf("hot loop: %d recovery spawns across %d poll passes; a spawn that dies "+
			"before confirmation must count as a FAILED attempt and back off (#1910)", got, passes)
	}
	if got == 0 {
		t.Fatal("no recovery was ever attempted; the test is not exercising the restore loop")
	}
}

// TestRestoreLostSessions_RepeatedImmediateExits_EscalateExponentially pins the
// other half of the #1910 acceptance criteria: the retained state must actually
// ESCALATE across attempts (consecutive failures accumulate against one episode)
// rather than each death being recorded as a first failure.
func TestRestoreLostSessions_RepeatedImmediateExits_EscalateExponentially(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t) // every pass is due, so the loop is free to hot-loop if it can

	key := stableSessionKey(repoID, inst)
	// spawn, observe-death, spawn, observe-death, ...
	for i := 0; i < 6; i++ {
		manager.RestoreLostSessions()
	}

	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	var failures int
	if st != nil {
		failures = st.consecutiveFailures
	}
	manager.mu.Unlock()

	if st == nil {
		t.Fatal("retry state was dropped despite a session that never stays up")
	}
	if failures < 2 {
		t.Fatalf("consecutiveFailures = %d after repeated immediate exits; the deaths must "+
			"accumulate against ONE episode so the backoff escalates and the #1108 "+
			"escalation eventually fires (#1910)", failures)
	}
}

// TestRestoreLostSessions_ConfirmedAliveClearsRetryState is the definition's other
// side: retry state clears ONLY after a liveness confirmation. A runtime that
// stays up past the settle interval must have its history forgotten, so a genuine,
// much-later loss starts from a clean backoff instead of inheriting an old
// episode's escalation.
func TestRestoreLostSessions_ConfirmedAliveClearsRetryState(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	key := stableSessionKey(repoID, inst)
	manager.RestoreLostSessions() // recovers; arms the confirmation window

	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state was dropped on spawn success: a runtime that dies before " +
			"confirmation would then re-enter as a fresh episode with a zeroed backoff (#1910)")
	}
	if !st.awaitingConfirm {
		t.Fatal("a successful spawn must leave the restore awaiting liveness confirmation")
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after recovery", got)
	}

	// A poll gets an ANSWER out of the new runtime: THAT is the confirmation. No
	// clock is advanced anywhere in this test.
	observeAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, stillTracked := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if stillTracked {
		t.Fatal("retry state survived a confirmed-alive runtime; it must clear on confirmation")
	}
}

// TestRestoreLostSessions_NeverObservedAlive_BacksOffRegardlessOfElapsedTime is
// #1917 round-6 finding (2): the confirmation was a clock, not an observation.
//
// "Elapsed time without a successful liveness observation is not proof that the
// runtime survived." Two real configurations broke the old fixed 15s window, and
// this test covers BOTH with one property, because the fix makes them the same
// case:
//
//   - daemon_poll_interval > the window: a restored process exits IMMEDIATELY, but
//     nothing looks at it until after the window expires — so its history was
//     cleared and treated as a fresh episode, and #1910's backoff never armed.
//   - remote at DEFAULT settings: unanswered probes deliberately keep a session
//     non-Lost for remoteLostGracePeriod (60s), four times the old window, with the
//     same outcome.
//
// In both, the daemon never got an ANSWER out of the runtime. So: no observation,
// no confirmation, no matter how much time passes.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the history is cleared and each death re-enters
// as attempt #1, so consecutiveFailures never climbs.
func TestRestoreLostSessions_NeverObservedAlive_BacksOffRegardlessOfElapsedTime(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	key := stableSessionKey(repoID, inst)
	// Many passes, and NOT ONE observation — exactly what a poll interval longer
	// than any window, or a remote inside its 60s grace, produces. No clock is
	// advanced: the point is that time is irrelevant without an answer.
	for i := 0; i < 6; i++ {
		manager.RestoreLostSessions()
	}

	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	failures := 0
	if st != nil {
		failures = st.consecutiveFailures
	}
	manager.mu.Unlock()

	if st == nil {
		t.Fatal("the retry history was cleared for a runtime nothing ever observed alive: elapsed " +
			"time is not proof of survival, so the backoff never arms and the session respawns at " +
			"poll cadence forever (#1917 round 6)")
	}
	if failures < 2 {
		t.Fatalf("consecutiveFailures = %d after repeated unobserved deaths; each one must count "+
			"against the SAME episode so the backoff escalates", failures)
	}
}

// TestRestoreLostSessions_ObservationConfirms_NotElapsedTime pins the positive half:
// an ANSWER from the runtime — and only that — clears the history.
func TestRestoreLostSessions_ObservationConfirms_NotElapsedTime(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	key := stableSessionKey(repoID, inst)

	manager.RestoreLostSessions() // spawn; awaiting confirmation

	// Passes WITHOUT an observation must not confirm, however many there are.
	manager.RestoreLostSessions()
	manager.mu.Lock()
	stillAwaiting := manager.lostRestoreStates[key] != nil
	manager.mu.Unlock()
	if !stillAwaiting {
		t.Fatal("the history was cleared without any observation: a non-Lost row is not proof of " +
			"life (a remote inside its loss grace reads non-Lost while dead)")
	}

	// One ANSWER, and it is confirmed.
	observeAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, tracked := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("an observed-alive runtime must have its retry history cleared")
	}
}

// deadPaneBackend models the exact trap of #1917 round 7: the agent-server's
// Snapshot reports (false,false,"") with a NIL ERROR — which localAgentServer does
// unconditionally, because it wraps HasUpdated and HasUpdated suppresses
// capture/session-gone failures — while the liveness probe correctly answers DEAD.
//
// Absence of an error is not evidence of life. A counter that advances on "the call
// didn't error" is fooled by exactly this.
type deadPaneBackend struct{ *session.FakeBackend }

func (b *deadPaneBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return false, false, "" // what a DEAD session's suppressed capture returns
}
func (b *deadPaneBackend) IsAlive(*session.Instance) (bool, error) { return false, nil } // answered dead
func (b *deadPaneBackend) Type() string                            { return "local" }

// TestRefreshInstanceStatus_SnapshotNilErrorOnDeadSession_IsNotAnObservation is
// round-7 finding (1), and it is the counter being fooled by the same disease it
// was built to cure.
//
// The poll's Snapshot returns nil for a session that is already dead, and the very
// next probe marks it Lost. If that nil error counted as a liveness observation,
// RestoreLostSessions would read "previously confirmed", clear the failure history,
// and respawn with no backoff — #1910's hot loop, rebuilt out of an absent error.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: aliveObservations advances for a session the
// same tick marks Lost.
func TestRefreshInstanceStatus_SnapshotNilErrorOnDeadSession_IsNotAnObservation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &deadPaneBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "corpse", backend, true, session.Running)

	obsKey := stableSessionKey(repoID, inst)
	manager.refreshInstanceStatus(repoID, inst)

	manager.mu.Lock()
	count := manager.aliveObservations[obsKey]
	manager.mu.Unlock()

	if got := inst.GetStatus(); got != session.Lost {
		t.Fatalf("setup: the probe must mark this dead session Lost, got %v", got)
	}
	if count != 0 {
		t.Fatal("a Snapshot that returned NIL for a session this very tick marked Lost was counted " +
			"as a liveness observation: localAgentServer.Snapshot never errors (it wraps HasUpdated, " +
			"which suppresses capture/session-gone failures), so absence of an error masqueraded as " +
			"evidence — and the restore loop then clears the failure history and respawns with no " +
			"backoff, rebuilding #1910's hot loop (#1917 round 7)")
	}
}

// TestRefreshInstanceStatus_LiveSessionIsObserved is the positive guard: requiring
// affirmative evidence must not mean nothing ever confirms. A session whose pane
// produces output IS affirmative — a dead pane cannot.
func TestRefreshInstanceStatus_LiveSessionIsObserved(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "busy", backend, true, session.Running)

	obsKey := stableSessionKey(repoID, inst)
	manager.refreshInstanceStatus(repoID, inst)

	manager.mu.Lock()
	count := manager.aliveObservations[obsKey]
	manager.mu.Unlock()
	if count == 0 {
		t.Fatal("a live session produced no liveness observation: confirmation would never happen " +
			"and every restored session would be charged a failure it did not earn")
	}
}

type retiredAliveProbeBackend struct {
	*session.FakeBackend
	aliveStarted chan struct{}
	releaseAlive chan struct{}
	once         sync.Once
}

func (b *retiredAliveProbeBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return false, false, ""
}

func (b *retiredAliveProbeBackend) IsAlive(*session.Instance) (bool, error) {
	b.once.Do(func() {
		close(b.aliveStarted)
		<-b.releaseAlive
	})
	return true, nil
}

func (b *retiredAliveProbeBackend) Type() string { return "local" }

// A successful Snapshot can still belong to the predecessor if replacement
// rotates its runtime after SnapshotAgent's final check but before the daemon
// records its liveness side effects. The late Alive answer must not confirm the
// replacement whose confirmation counter was armed while that probe was blocked.
func TestRefreshInstanceStatus_RetiredProbeCannotConfirmReplacement(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &retiredAliveProbeBackend{
		FakeBackend:  session.NewFakeBackend(),
		aliveStarted: make(chan struct{}),
		releaseAlive: make(chan struct{}),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "retired-probe", backend, true, session.Running)

	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-backend.aliveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("status poll never reached the predecessor liveness probe")
	}

	inst.ClearIdleEvidence()
	manager.armRestoreConfirmation(repoID, inst)
	close(backend.releaseAlive)
	select {
	case <-pollDone:
	case <-time.After(2 * time.Second):
		t.Fatal("retired status poll did not return")
	}

	manager.mu.Lock()
	state := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	confirmed := state != nil && manager.observedAliveSinceSpawnLocked(repoID, inst, state)
	manager.mu.Unlock()
	if confirmed {
		t.Fatal("predecessor liveness was counted after replacement armed its confirmation boundary")
	}
}

// wedgedProbeBackend models #1917 round 8's ROOT: a primitive that cannot say "I
// don't know". Its liveness probe TIMED OUT — tmux never answered — which
// sessionExists laundered into `true` and Alive reported as (true, nil).
//
// The Snapshot is deliberately the same (false,false,"") a dead session gives, so
// the poll takes its idle branch and asks the liveness probe. Everything then hinges
// on what that probe is ALLOWED to say.
type wedgedProbeBackend struct{ *session.FakeBackend }

func (b *wedgedProbeBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return false, false, ""
}

func (b *wedgedProbeBackend) IsAlive(*session.Instance) (bool, error) {
	// The tri-state: could not ask. Before round 8 this signature was `bool`, so
	// this exact situation was FORCED to be reported as `true`.
	return false, fmt.Errorf("%w: has-session while probing liveness", tmux.ErrTmuxTimeout)
}

func (b *wedgedProbeBackend) Type() string { return "local" }

// TestRefreshInstanceStatus_WedgedProbe_IsNotAnObservation is round-8 finding (2) —
// the same disease as round 7, one primitive over.
//
// A timed-out `tmux has-session` became `true` via sessionExists; Alive reported
// (true, nil); the poll recorded a positive liveness observation though tmux NEVER
// ANSWERED — which after a Lost recovery clears lostRestoreStates, resets the
// backoff, and can mark a wedged pane READY.
//
// Fixing it at the counter would have made the next caller round 9's finding. It is
// fixed at the PRIMITIVE: Backend.IsAlive returns (bool, error), so "could not ask"
// is sayable and probeLiveness maps it to probeUnknown.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: aliveObservations advances for a server that
// never answered.
func TestRefreshInstanceStatus_WedgedProbe_IsNotAnObservation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &wedgedProbeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "wedged", backend, true, session.Running)

	obsKey := stableSessionKey(repoID, inst)
	manager.refreshInstanceStatus(repoID, inst)

	manager.mu.Lock()
	count := manager.aliveObservations[obsKey]
	manager.mu.Unlock()

	if count != 0 {
		t.Fatal("a liveness probe that TIMED OUT was counted as affirmative evidence: a bool cannot " +
			"say 'I don't know', so sessionExists laundered the timeout into `true` and Alive " +
			"reported (true, nil) — and the restore loop then clears the failure history and can " +
			"mark a wedged pane Ready (#1917 round 8). The fix is the primitive's type, not this " +
			"caller: the next caller would have been round 9's finding.")
	}
	// Nor may it be marked Lost: an unanswered probe is not evidence of death
	// either — that is what the debounce is for.
	if got := inst.GetStatus(); got == session.Lost {
		t.Fatal("an unanswerable probe marked the session Lost: silence is not death either")
	}
}
