package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The #1910 regression lock: a Lost session whose recovery spawn SUCCEEDS but
// whose runtime dies before it can be confirmed alive must be treated as a FAILED
// attempt and backed off exponentially — not respawned at poll cadence forever.
//
// The field shape: 465 identical errors, a respawn roughly every two seconds for
// ~28 minutes. Recover returned nil every time (the tmux spawn genuinely worked),
// so lostRestoreFailed never saw an error and the #1108 backoff never armed.

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
	registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t) // every pass is due, so the loop is free to hot-loop if it can

	key := daemonInstanceKey(repoID, "flapper")
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

	key := daemonInstanceKey(repoID, "healthy")
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

	// The runtime stayed up past the settle interval: the next pass observes it
	// non-Lost and clears the history.
	prev := lostRestoreConfirmSettle
	lostRestoreConfirmSettle = 0
	t.Cleanup(func() { lostRestoreConfirmSettle = prev })
	manager.mu.Lock()
	st.confirmBy = time.Now().Add(-time.Second) // settle window has elapsed
	manager.mu.Unlock()

	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, stillTracked := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if stillTracked {
		t.Fatal("retry state survived a confirmed-alive runtime; it must clear on confirmation")
	}
}
