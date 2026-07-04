package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// recoverFakeBackend counts Recover calls and emulates LocalBackend.Recover's
// success contract (flip to Running) or a configured failure. Type is
// overridable so the remote-skip rule can be exercised.
type recoverFakeBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	failWith error
	recovers int
	typeName string
}

func (b *recoverFakeBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	if b.failWith != nil {
		return b.failWith
	}
	inst.SetStatus(session.Running)
	return nil
}

func (b *recoverFakeBackend) recoverCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

func (b *recoverFakeBackend) Type() string {
	if b.typeName != "" {
		return b.typeName
	}
	return "local"
}

// zeroRestoreBackoff makes every RestoreLostSessions pass an attempt.
func zeroRestoreBackoff(t *testing.T) {
	t.Helper()
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = 0
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
}

// TestRestoreLostSessions_RecoversLostInstance: the core loop — a Lost local
// session gets exactly one Recover, comes back Running, and its retry state is
// dropped.
func TestRestoreLostSessions_RecoversLostInstance(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after recovery", got)
	}
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[daemonInstanceKey(repoID, "stranded")]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("successful recovery must drop the retry state")
	}

	// A healed session must not be touched again.
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls after heal = %d, want still 1", got)
	}
}

// TestRestoreLostSessions_KeepsRetryingAndHeals mirrors the #1128 root-ensure
// guarantee for the general loop: failures past the escalation threshold never
// park the retry permanently, and the first pass after the cause clears heals.
func TestRestoreLostSessions_KeepsRetryingAndHeals(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("worktree unavailable")}
	inst := registerStarted(t, manager, repoID, repoPath, "unlucky", backend, true, session.Lost)

	attempts := lostRestoreEscalationThreshold + 3
	for i := 0; i < attempts; i++ {
		manager.RestoreLostSessions()
	}
	if got := backend.recoverCalls(); got != attempts {
		t.Fatalf("loop must keep attempting past the escalation threshold: want %d attempts, got %d", attempts, got)
	}

	// The cause clears (the outage ends): the very next pass must heal.
	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running on the first pass after the cause cleared", got)
	}
}

// TestRestoreLostSessions_BacksOffBetweenFailures: passes inside the backoff
// window must not re-attempt (the cheap-retry discipline, not a hot loop).
func TestRestoreLostSessions_BacksOffBetweenFailures(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = time.Hour
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("still down")}
	registerStarted(t, manager, repoID, repoPath, "waiting", backend, true, session.Lost)

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("passes inside the backoff window must not re-attempt: want 1, got %d", got)
	}
}

// TestRestoreLostSessions_SkipsIneligible: tombstoned records (their only
// future is a finished kill), sessions with a kill in flight, remote sessions
// (flagged, not auto-restored), non-Lost sessions, and the reserved root
// (EnsureRootAgents owns it) are never Recover'd.
func TestRestoreLostSessions_SkipsIneligible(t *testing.T) {
	t.Run("tombstoned", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerStarted(t, manager, repoID, repoPath, "corpse", backend, true, session.Lost)
		inst.MarkUserKilled()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a tombstoned session must never be restored, got %d recover calls", got)
		}
	})

	t.Run("kill in flight", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, "dying", backend, true, session.Lost)
		key := daemonInstanceKey(repoID, "dying")
		manager.mu.Lock()
		manager.killsInFlight[key] = struct{}{}
		manager.mu.Unlock()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a session mid-kill must not be restored, got %d recover calls", got)
		}
	})

	t.Run("remote", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), typeName: "remote"}
		registerStarted(t, manager, repoID, repoPath, "faraway", backend, true, session.Lost)
		manager.RestoreLostSessions()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("remote sessions are not auto-restored in v1, got %d recover calls", got)
		}
	})

	t.Run("non-lost statuses", func(t *testing.T) {
		// Archived (#1028) is included: an archived session is deliberately
		// quiescent and must NEVER be auto-restored — only an explicit
		// RestoreArchived brings it back. (In production it also loads
		// started=false, but registering it started here proves the ==Lost gate
		// alone already fences it out.)
		for _, status := range []session.Status{session.Running, session.Ready, session.Loading, session.Deleting, session.Archived} {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
			registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, status)
			manager.RestoreLostSessions()
			if got := backend.recoverCalls(); got != 0 {
				t.Fatalf("status %v must not be restored, got %d recover calls", status, got)
			}
		}
	})

	t.Run("reserved root", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, backend, true, session.Lost)
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("the reserved root is EnsureRootAgents' job, got %d recover calls", got)
		}
	})
}

// raceBackend is a readyFakeBackend whose Recover and Kill can each be held
// open on channels, and which counts both, so kill-vs-recover interleaving can
// be pinned exactly.
type raceBackend struct {
	readyFakeBackend
	mu             sync.Mutex
	kills          int
	recovers       int
	recoverStarted chan struct{}
	recoverBlock   chan struct{}
	killStarted    chan struct{}
	killBlock      chan struct{}
}

func (b *raceBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	b.recovers++
	b.mu.Unlock()
	if b.recoverStarted != nil {
		close(b.recoverStarted)
		b.recoverStarted = nil
	}
	if b.recoverBlock != nil {
		<-b.recoverBlock
	}
	inst.SetStatus(session.Running)
	return nil
}

func (b *raceBackend) Kill(inst *session.Instance) error {
	b.mu.Lock()
	b.kills++
	b.mu.Unlock()
	if b.killStarted != nil {
		close(b.killStarted)
		b.killStarted = nil
	}
	if b.killBlock != nil {
		<-b.killBlock
	}
	return b.readyFakeBackend.Kill(inst)
}

func (b *raceBackend) counts() (kills, recovers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.kills, b.recovers
}

// installRaceBackend registers backend as the factory product and creates a
// tracked session titled title, returning the manager, repoID, and instance.
func installRaceBackend(t *testing.T, backend *raceBackend, title string) (*Manager, string, *session.Instance) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.readyFakeBackend = readyFakeBackend{fake}
		return backend, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, title)]
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("created instance not tracked")
	}
	return manager, repo.ID, inst
}

// TestKillSession_WaitsForInFlightRecover is the Greptile P1 on #1137: a
// KillSession arriving while the restore loop is mid-Recover must WAIT for
// the recover attempt to finish and then tear the freshly-restored session
// down — never interleave. End state: record deleted, instance dropped,
// exactly one teardown, exactly one recover.
func TestKillSession_WaitsForInFlightRecover(t *testing.T) {
	backend := &raceBackend{
		recoverStarted: make(chan struct{}),
		recoverBlock:   make(chan struct{}),
	}
	manager, repoID, inst := installRaceBackend(t, backend, "contested")
	zeroRestoreBackoff(t)
	inst.SetStatus(session.Lost)

	recoverStarted := backend.recoverStarted
	restoreDone := make(chan struct{})
	go func() {
		manager.RestoreLostSessions()
		close(restoreDone)
	}()
	<-recoverStarted // the loop is inside Recover, holding the op lock

	killDone := make(chan error, 1)
	go func() {
		killDone <- manager.KillSession(KillSessionRequest{Title: "contested", RepoID: repoID})
	}()

	// The kill must be blocked behind the in-flight recover attempt.
	select {
	case err := <-killDone:
		t.Fatalf("KillSession completed mid-Recover (err=%v); it must wait for the attempt", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(backend.recoverBlock) // recover finishes (session "restored")
	<-restoreDone
	select {
	case err := <-killDone:
		if err != nil {
			t.Fatalf("KillSession after recover: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession never proceeded after the recover attempt finished")
	}

	kills, recovers := backend.counts()
	if kills != 1 || recovers != 1 {
		t.Fatalf("want exactly one teardown and one recover, got kills=%d recovers=%d", kills, recovers)
	}
	if rec := recordFor(t, repoID, "contested"); rec != nil {
		t.Fatalf("killed session's record must be deleted, still present: %+v", rec)
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "contested")]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("killed session must be dropped from the manager")
	}
}

// TestRestoreLostSessions_SkipsDuringInFlightKill is the reverse ordering: a
// restore pass arriving while a KillSession teardown is in flight must not
// touch the session — no recover call, ever — and after the kill completes
// the session is gone for good.
func TestRestoreLostSessions_SkipsDuringInFlightKill(t *testing.T) {
	backend := &raceBackend{
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	manager, repoID, inst := installRaceBackend(t, backend, "doomed")
	zeroRestoreBackoff(t)
	inst.SetStatus(session.Lost)

	killStarted := backend.killStarted
	killDone := make(chan error, 1)
	go func() {
		killDone <- manager.KillSession(KillSessionRequest{Title: "doomed", RepoID: repoID})
	}()
	<-killStarted // teardown in flight, op lock + killsInFlight held

	manager.RestoreLostSessions() // must return promptly without recovering

	close(backend.killBlock)
	if err := <-killDone; err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	manager.RestoreLostSessions() // session is gone; still nothing to recover

	if _, recovers := backend.counts(); recovers != 0 {
		t.Fatalf("a session being killed must never be recovered, got %d recover calls", recovers)
	}
	if rec := recordFor(t, repoID, "doomed"); rec != nil {
		t.Fatalf("killed session's record must be deleted, still present: %+v", rec)
	}
}

// TestRestoreLostSessions_StateCleanedForGoneSessions: retry state for
// sessions that were killed or healed does not accumulate.
func TestRestoreLostSessions_StateCleanedForGoneSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("down")}
	registerStarted(t, manager, repoID, repoPath, "doomed", backend, true, session.Lost)

	manager.RestoreLostSessions()
	key := daemonInstanceKey(repoID, "doomed")
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if !hasState {
		t.Fatal("a failed attempt must record retry state")
	}

	// The session disappears (user killed it; record + map entry gone).
	manager.mu.Lock()
	delete(manager.instances, key)
	manager.mu.Unlock()
	manager.RestoreLostSessions()

	manager.mu.Lock()
	_, hasState = manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("retry state for a gone session must be dropped")
	}
}
