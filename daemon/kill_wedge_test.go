package daemon

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The #1917 regression locks: a session kill must never wedge indefinitely and
// leave the session undeletable for the daemon's whole lifetime.
//
// Both tests below reproduce the reported wedge against the pre-fix code (each
// documents the exact failure it reproduces), and both assert the same three
// properties the fix owes the user: the kill RETURNS, the killsInFlight guard is
// RELEASED, and a RETRY works. A kill that fails cleanly is recoverable; a kill
// that never returns is not.

// holdFileLock takes the same exclusive flock config.WithFileLock uses on path,
// from an independent file description, and returns a release func. flock
// contends per open-file-description, so this blocks an in-process caller exactly
// as a second af process would — no subprocess needed.
func holdFileLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatalf("flock: %v", err)
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// killGuardHeld reports whether the manager still has a kill registered for key —
// the guard whose permanent retention is the #1917 symptom (every later kill is
// rejected with "kill already in progress", every other action with "session is
// being deleted").
func killGuardHeld(m *Manager, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, held := m.killsInFlight[key]
	return held
}

// TestKillSession_ContendedInstancesLock_IsBoundedAndRetryable holds the repo's
// instances flock — exactly what a second af process does — and asserts the kill
// stays bounded end to end.
//
// Every flock the kill path touches is on this one file, so this exercises all
// three bounds at once. Against the unfixed tree, a goroutine dump taken while
// this test hung named the FIRST one precisely:
//
//	syscall.Flock
//	config.WithFileLock                          config/filelock.go
//	config.LoadAndMigrateSchemaFile              config/schema_migration.go:179
//	config.MigrateAllRepoInstancesForDaemonLoad
//	daemon.refreshDaemonInstances                daemon/daemon.go:369
//	daemon.(*Manager).refreshLocked              daemon/manager.go:272  <- holds m.mu
//	daemon.(*Manager).findSession
//	daemon.(*Manager).resolveActionSession
//	daemon.(*Manager).KillSession                daemon/manager_sessions.go:19
//
// That one parks before the kill guard is even registered, and it holds the
// manager's GLOBAL lock while it does — so it freezes every RPC, which is the
// reported "daemon did not exit within 5s of SIGTERM". The two later bounds (the
// tombstone write via config.UpdateRepoInstances, and the record delete via
// session.Storage — the step the field's surviving tombstone implicates) sit
// behind it on the same file and are covered by the same hold.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: KillSession never returns (fails on the 10s
// "HUNG" deadline), so the guard is never released and a retry is impossible. It
// is not that the error was wrong — there was no error at all.
func TestKillSession_ContendedInstancesLock_IsBoundedAndRetryable(t *testing.T) {
	backend := &raceBackend{}
	manager, repoID, _ := installRaceBackend(t, backend, "wedged")

	// Short budgets: this test is about the bounds existing, not their production
	// sizes. BOTH flock sites on the kill path are bounded and both are exercised
	// here — the tombstone write (config.UpdateRepoInstances) comes first and the
	// record delete (session.Storage) last, and they contend for the same lock.
	prevDelete := session.InstanceDeleteLockTimeout
	session.InstanceDeleteLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { session.InstanceDeleteLockTimeout = prevDelete })
	prevUpdate := config.RepoInstancesLockTimeout
	config.RepoInstancesLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { config.RepoInstancesLockTimeout = prevUpdate })
	// The refresh path migrates the same file BEFORE the kill resolves its session,
	// holding the manager's global lock while it does — so it is the first bound
	// this test crosses, not the last.
	prevSchema := config.SchemaMigrationLockTimeout
	config.SchemaMigrationLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { config.SchemaMigrationLockTimeout = prevSchema })

	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	release := holdFileLock(t, path)
	defer release()

	key := daemonInstanceKey(repoID, "wedged")

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "wedged", RepoID: repoID})
		killDone <- kerr
	}()

	var kerr error
	select {
	case kerr = <-killDone:
	case <-time.After(10 * time.Second):
		t.Fatal("HUNG: KillSession never returned while the instances lock was held — " +
			"this is the #1917 wedge: killsInFlight stays held, every retry is rejected, " +
			"and the session is undeletable until the daemon restarts")
	}
	if kerr == nil {
		t.Fatal("KillSession reported success while the record could not be written")
	}
	// The error must be actionable and name the real cause, not a bare I/O failure:
	// a typed, retryable cause the caller can match on, and a message that says what
	// is holding things up rather than "operation failed".
	if !errors.Is(kerr, config.ErrLockTimeout) {
		t.Fatalf("kill error does not wrap config.ErrLockTimeout (a retryable, diagnosable cause): %v", kerr)
	}
	if !strings.Contains(kerr.Error(), "holding it") {
		t.Fatalf("kill error does not explain the contention: %v", kerr)
	}

	// The guard MUST be released, or the session is undeletable exactly as before —
	// a bounded wait that still strands the guard fixes nothing.
	if killGuardHeld(manager, key) {
		t.Fatal("killsInFlight still held after a failed kill: the session is undeletable and a retry would be rejected")
	}

	// And the retry must actually work once the contention clears.
	release()
	if _, err := manager.KillSession(KillSessionRequest{Title: "wedged", RepoID: repoID}); err != nil {
		t.Fatalf("retry after the lock cleared must succeed, got: %v", err)
	}
	manager.mu.Lock()
	_, stillTracked := manager.instances[key]
	manager.mu.Unlock()
	if stillTracked {
		t.Fatal("session still tracked after a successful retry")
	}
}

// TestKillSession_StuckPeerOperation_DoesNotWedgeIndefinitely covers the issue's
// own leading hypothesis: a kill blocking forever on the per-session op lock
// behind a Recover that never finishes.
//
// The exclusion itself is correct and is NOT what this changes — a kill must
// still never interleave with a respawn (TestKillSession_WaitsForInFlightRecover
// pins that, and still passes). What changes is only the failure mode: an
// unbounded Lock() inside the killsInFlight guard turns a stuck peer into a
// permanently undeletable session, while a bounded wait turns it into a retryable
// error.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: KillSession parks in opLock.Lock() forever
// (fails on the "HUNG" deadline below).
func TestKillSession_StuckPeerOperation_DoesNotWedgeIndefinitely(t *testing.T) {
	backend := &raceBackend{
		recoverStarted: make(chan struct{}),
		recoverBlock:   make(chan struct{}),
	}
	manager, repoID, inst := installRaceBackend(t, backend, "contested")
	zeroRestoreBackoff(t)
	inst.SetStatusForTest(session.Lost)

	prev := opLockTimeout
	opLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { opLockTimeout = prev })

	recoverStarted := backend.recoverStarted
	restoreDone := make(chan struct{})
	go func() {
		manager.RestoreLostSessions()
		close(restoreDone)
	}()
	<-recoverStarted // the restore loop is inside Recover, holding the op lock

	key := daemonInstanceKey(repoID, "contested")
	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "contested", RepoID: repoID})
		killDone <- kerr
	}()

	var kerr error
	select {
	case kerr = <-killDone:
	case <-time.After(10 * time.Second):
		close(backend.recoverBlock)
		t.Fatal("HUNG: KillSession never returned while a Recover held the op lock — " +
			"this is the #1917 wedge: the guard is held forever and the session is undeletable")
	}
	if kerr == nil {
		t.Fatal("KillSession reported success without ever acquiring the op lock")
	}
	if !strings.Contains(kerr.Error(), "retry") {
		t.Fatalf("kill error is not actionable (must tell the user to retry): %v", kerr)
	}
	if killGuardHeld(manager, key) {
		t.Fatal("killsInFlight still held after a timed-out kill: the session would be undeletable")
	}

	// Nothing may have been torn down: the timeout happens BEFORE the tombstone, so
	// the session must be exactly as it was, which is what makes the retry a true
	// retry rather than a resumption of a half-finished kill.
	if kills, _ := backend.counts(); kills != 0 {
		t.Fatalf("a kill that never got the lock tore something down anyway (kills=%d)", kills)
	}
	if inst.UserKilled() {
		t.Fatal("a kill that never got the lock left a kill-intent tombstone behind")
	}

	// Once the peer releases, the retry must succeed.
	close(backend.recoverBlock)
	<-restoreDone
	if _, err := manager.KillSession(KillSessionRequest{Title: "contested", RepoID: repoID}); err != nil {
		t.Fatalf("retry after the peer operation finished must succeed, got: %v", err)
	}
}
