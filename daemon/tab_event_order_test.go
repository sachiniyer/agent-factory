package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The post-merge Codex findings on #1815, ordering side.
//
// session.updated carries a WHOLE InstanceData, and every client re-projects the
// session wholesale from it (web's upsertSession, the TUI's
// ReconcileTabsFromData). That makes publish ORDER load-bearing: the last event to
// land is the state the client shows. CreateTab/CloseTab publish inside the repo
// start lock they persisted under, but persistPollChange used to snapshot and
// persist under that lock, RELEASE it, and only then publish. A poll preempted in
// that window could capture the pre-tab roster, let a tab create/delete persist and
// announce the fresh roster, and then land its older payload last — re-projecting
// the just-created tab out of existence (or a just-closed one back in) until some
// later update happened to repair it. On a quiet session that repair never comes,
// which is the exact failure #1815 set out to fix.
//
// These lock the invariant in from both ends: the poll publishes inside the
// critical section, and its payload is the state as of that section.

// TestPersistPollChange_PublishesUnderRepoLock is the discriminating test: at the
// moment the poll publishes, it must still hold the repo start lock.
//
// The window between unlock and publish is only a few instructions wide, so racing
// two goroutines would almost never catch a regression — a probabilistic test that
// passes on broken code is worse than none. Instead this asserts the property
// directly at the seam (testHookPollBeforePublish, mirroring
// testHookSpawnPingPassed): TryLock from inside the hook must FAIL, because this
// goroutine already owns the mutex. Move the publish back after the Unlock and the
// TryLock succeeds — nobody holds it — and this test goes red deterministically.
func TestPersistPollChange_PublishesUnderRepoLock(t *testing.T) {
	const title = "pollunderlock"
	manager, repo := tabEventSession(t, title)
	instance := manager.instances[daemonInstanceKey(repo.ID, title)]
	if instance == nil {
		t.Fatalf("session %q missing from the manager", title)
	}

	lock := manager.startLockForRepo(repo.ID)
	probed := false
	heldAtPublish := false
	testHookPollBeforePublish = func() {
		probed = true
		// Non-reentrant: acquiring here can only mean the poll has ALREADY released
		// the lock it persisted under, i.e. the publish escaped the critical section.
		if lock.TryLock() {
			lock.Unlock()
			return
		}
		heldAtPublish = true
	}
	t.Cleanup(func() { testHookPollBeforePublish = func() {} })

	// A liveness that differs from the instance's own makes the poll treat this tick
	// as a real transition, which is what drives it to persist + publish at all.
	manager.persistPollChange(repo.ID, instance, otherLiveness(instance.GetLiveness()), time.Time{}, false)

	if !probed {
		t.Fatal("the poll never reached its publish: persistPollChange returned without announcing a change")
	}
	if !heldAtPublish {
		t.Fatal("persistPollChange published session.updated after releasing the repo start lock; " +
			"an older whole-session payload can then land after a newer tab roster and undo it (#1815 follow-up)")
	}
}

// TestPersistPollChange_PublishesRosterAsOfItsLock: the payload half of the same
// invariant. A poll that acquires the lock AFTER a tab create must publish the
// roster INCLUDING that tab — never the roster as it looked before it blocked.
// Snapshot and publish therefore have to sit in one critical section together;
// hoisting the snapshot out would reintroduce a stale payload by another route.
func TestPersistPollChange_PublishesRosterAsOfItsLock(t *testing.T) {
	const title = "pollroster"
	manager, repo := tabEventSession(t, title)
	instance := manager.instances[daemonInstanceKey(repo.ID, title)]
	if instance == nil {
		t.Fatalf("session %q missing from the manager", title)
	}

	// Make the tab mutation land in the exact stale-snapshot window: after the
	// projection-only poll decided to publish and captured its payload, but before
	// it acquires the ordering lock. Tab mutations do not move the lifecycle epoch,
	// so an epoch-conditional re-read misses this change.
	var name string
	prev := testHookPollBeforePersistLock
	t.Cleanup(func() { testHookPollBeforePersistLock = prev })
	testHookPollBeforePersistLock = func() {
		var err error
		created, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "livepreview",
		})
		if err != nil {
			t.Fatalf("CreateTab(web): %v", err)
		}
		name = created.Name
	}

	_, ch := manager.events.subscribe()
	beforeReset, _ := instance.LimitResetAt()
	manager.persistPollChange(repo.ID, instance, instance.GetLiveness(), beforeReset, true)

	created := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if !tabNamed(created, name) {
		t.Fatalf("the tab-create event roster %v is missing its new tab %q", created.Tabs, name)
	}
	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if !tabNamed(got, name) {
		t.Fatalf("the poll published roster %v without the already-created tab %q: "+
			"a status tick would re-project it out of every open client", got.Tabs, name)
	}
}

// otherLiveness returns any liveness value that is not `l`, so a caller can force
// persistPollChange's changed-liveness branch without reaching into the poller.
func otherLiveness(l session.Liveness) session.Liveness {
	if l == session.LiveRunning {
		return session.LiveReady
	}
	return session.LiveRunning
}
