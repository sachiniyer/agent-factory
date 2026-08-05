package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// registerTaskSpawnedSession is registerArchivable for a session a TASK created:
// same real worktree and manager registration, but attributed to taskID and left
// mid-run (LiveRunning) so the completion edge can still be driven.
//
// The TaskID must go through NewInstance rather than being poked in afterwards —
// taskRunActive is derived from it at construction and is the whole subject here.
func registerTaskSpawnedSession(t *testing.T, m *Manager, repoID, repoPath, title, taskID string) *session.Instance {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+sanitizeArchiveTitle(title))
	branch := "af/" + sanitizeArchiveTitle(title)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0644))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: repoPath, Program: "claude", TaskID: taskID,
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	// Mid-run: the agent is working, so the run has not ended and the idle edge
	// below is a real transition INTO Ready rather than a no-op.
	inst.SetStatusForTest(session.Running)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()

	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")
	return inst
}

// stubTaskLifecycle points the task lookup at a fixed policy for repoID, so these
// tests exercise the lifecycle decision without standing up a task store.
func stubTaskLifecycle(t *testing.T, taskID, onComplete string) {
	t.Helper()
	prev := loadTasksForRepoID
	loadTasksForRepoID = func(string) ([]task.Task, []task.Task, error) {
		return []task.Task{{ID: taskID, OnComplete: onComplete}}, nil, nil
	}
	t.Cleanup(func() { loadTasksForRepoID = prev })
}

// endRunOnIdleEdge drives the transition that ends a task run: the agent settling
// idle. It is the same ObserveLiveness edge the status poll uses, so these tests
// exercise the real completion signal rather than poking taskRunActive.
func endRunOnIdleEdge(t *testing.T, inst *session.Instance) bool {
	t.Helper()
	was := inst.TaskRunActive()
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	require.False(t, inst.TaskRunActive(), "the idle edge must end the run")
	return was
}

// TestTaskSessionLifecycle_KeepLeavesTheFinishedSessionAlone is the compatibility
// case, and the one that matters most: every task written before #2595 declares
// nothing, and a finished run must still leave its session exactly where it was.
func TestTaskSessionLifecycle_KeepLeavesTheFinishedSessionAlone(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-keep")

	for _, verb := range []string{"", task.OnCompleteKeep} {
		stubTaskLifecycle(t, "task-keep", verb)
		was := inst.TaskRunActive()
		manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)
	}
	was := endRunOnIdleEdge(t, inst)
	stubTaskLifecycle(t, "task-keep", "")
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	// Nothing is dispatched, so a short settle is enough to prove no teardown ran.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, session.LiveReady, inst.GetLiveness(), "an undeclared policy must leave the session live")
	manager.mu.Lock()
	_, stillRegistered := manager.instances[daemonInstanceKey(repoID, "nightly")]
	manager.mu.Unlock()
	assert.True(t, stillRegistered)
}

// TestTaskSessionLifecycle_ArchiveArchivesTheFinishedSession is the headline
// behavior of #2595: the leak the issue reports stops accumulating.
func TestTaskSessionLifecycle_ArchiveArchivesTheFinishedSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-archive")
	stubTaskLifecycle(t, "task-archive", task.OnCompleteArchive)

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	require.Eventually(t, func() bool {
		return inst.GetLiveness() == session.LiveArchived
	}, 20*time.Second, 25*time.Millisecond, "the finished run's session must be archived")

	// Archived, not destroyed: the record survives so the run stays restorable.
	assert.NotNil(t, recordFor(t, repoID, "nightly"), "an archived session keeps its durable record")
}

// TestTaskSessionLifecycle_KillRemovesTheFinishedSession covers the other verb.
// It exists as its own case because the archive/kill choice is the actual product
// decision here — archive retains the worktree, kill reclaims it (#2573).
func TestTaskSessionLifecycle_KillRemovesTheFinishedSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, present := manager.instances[daemonInstanceKey(repoID, "nightly")]
		return !present
	}, 20*time.Second, 25*time.Millisecond, "the finished run's session must be removed")
}

// TestTaskSessionLifecycle_OnlyFiresOnTheIdleEdge pins the predicate that keeps
// this from becoming a sweep. A session whose run ended long ago — the state a
// user may since have adopted and be working in — must never be revisited.
func TestTaskSessionLifecycle_OnlyFiresOnTheIdleEdge(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-archive")
	stubTaskLifecycle(t, "task-archive", task.OnCompleteArchive)

	endRunOnIdleEdge(t, inst)
	// A later tick observes the same finished session, but the edge is behind it:
	// taskRunWasActive is false because the run had already ended.
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, false)

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, session.LiveReady, inst.GetLiveness(),
		"a session whose run ended on an earlier tick must not be torn down by a later one")
}

// TestRunEndedIntoIdle covers the predicate directly, including the case that
// protects an uncertain create: the run marker also clears when startup settles
// terminal-unknown, and that session must never be reaped.
func TestRunEndedIntoIdle(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-x")

	assert.False(t, runEndedIntoIdle(inst, true), "a run still in flight has not ended")
	assert.False(t, runEndedIntoIdle(inst, false), "nor has one the caller never saw active")

	endRunOnIdleEdge(t, inst)
	assert.True(t, runEndedIntoIdle(inst, true), "ended + Ready is the completion edge")
	assert.False(t, runEndedIntoIdle(inst, false), "the edge needs the run to have been active")

	// Not idle: the run marker is clear but the agent is working again, so this is
	// not a finished run sitting quiet.
	inst.SetStatusForTest(session.Running)
	assert.False(t, runEndedIntoIdle(inst, true), "only a session that settled idle may be reaped")
}

// TestRunEndedIntoIdle_ExcludesAnUncertainCreate is the case that caught a wrong
// claim in review: session.Loading decomposes to LiveReady, and more importantly
// MarkStartupStateUnknown clears taskRunActive DIRECTLY while leaving liveness
// alone — so an uncertain create satisfies "run ended" and "is LiveReady" both.
//
// The daemon retains that record deliberately so an operator can inspect the
// workspace the create may have left behind (keepUncertainCreate). The poll's
// !Started() early return keeps it away from the lifecycle hook today, but the
// predicate must refuse it on its own rather than resting on a distant line.
func TestRunEndedIntoIdle_ExcludesAnUncertainCreate(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "uncertain", "task-x")

	// The production shape, which the shared fixture deliberately is not: a create
	// whose session was born LiveReady and whose agent never ran. The fixture opens
	// mid-run (LiveRunning) so the idle EDGE is drivable, so put it back to the
	// state MarkStartupStateUnknown actually finds. SetStatusForTest writes the
	// axes directly rather than transitioning, so this does not end the run itself.
	inst.SetStatusForTest(session.Ready)
	require.True(t, inst.TaskRunActive(), "precondition: the run is still marked in flight")

	inst.MarkStartupStateUnknown()
	require.False(t, inst.TaskRunActive(), "precondition: the marker clears without a completed run")
	require.Equal(t, session.LiveReady, inst.GetLiveness(),
		"precondition: liveness is untouched, which is what makes this indistinguishable on the first two conditions")

	assert.False(t, runEndedIntoIdle(inst, true),
		"a create whose startup outcome was never established is not a finished run")
}

// TestTaskSessionLifecycle_UnreadableTaskStoreKeepsTheSession: a lookup that
// cannot answer is not permission to tear anything down.
func TestTaskSessionLifecycle_UnreadableTaskStoreKeepsTheSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-boom")

	prev := loadTasksForRepoID
	loadTasksForRepoID = func(string) ([]task.Task, []task.Task, error) {
		return nil, nil, assert.AnError
	}
	t.Cleanup(func() { loadTasksForRepoID = prev })

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, session.LiveReady, inst.GetLiveness(),
		"an unreadable task store must leave the session in place")
}

// TestTaskSessionLifecycle_DeletedTaskKeepsItsSessions: sessions outlive the task
// that made them, and removing a task must not retroactively authorize destroying
// the work its runs produced.
func TestTaskSessionLifecycle_DeletedTaskKeepsItsSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-gone")

	prev := loadTasksForRepoID
	loadTasksForRepoID = func(string) ([]task.Task, []task.Task, error) {
		return nil, nil, nil // the task is no longer in the store
	}
	t.Cleanup(func() { loadTasksForRepoID = prev })

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, session.LiveReady, inst.GetLiveness())
}

// TestTaskSessionLifecycle_IgnoresSessionsNoTaskSpawned: a session a user made by
// hand has no TaskID, so no policy can reach it however it settles.
func TestTaskSessionLifecycle_IgnoresSessionsNoTaskSpawned(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "mine")
	stubTaskLifecycle(t, "", task.OnCompleteKill)

	assert.False(t, inst.TaskRunActive(), "precondition: a hand-made session has no run")
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, true)

	time.Sleep(200 * time.Millisecond)
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	manager.mu.Lock()
	_, stillRegistered := manager.instances[daemonInstanceKey(repoID, "mine")]
	manager.mu.Unlock()
	assert.True(t, stillRegistered, "a user's own session is never task-reaped")
}

// TestTaskSessionLifecycle_BindsTheTeardownToTheStableID is the stale-identity
// guard. The teardown runs later on a goroutine, and ArchiveSession/KillSession
// fall back to {Title, RepoID} only when ID is empty — so a title-only request
// would let a kill plus a same-titled re-create in that gap retarget the reap
// onto the REPLACEMENT session. #2779 was the last time a lifecycle op keyed on
// a title reached the wrong worktree.
func TestTaskSessionLifecycle_BindsTheTeardownToTheStableID(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	// A channel, not shared vars: the seam runs on the teardown goroutine while the
	// assertion runs on the test's, so plain fields would be a data race (-race
	// catches it) and the handoff is what publishes the write.
	got := make(chan KillSessionRequest, 1)
	prev := killSessionForLifecycle
	killSessionForLifecycle = func(m *Manager, req KillSessionRequest) error {
		got <- req
		return nil
	}
	t.Cleanup(func() { killSessionForLifecycle = prev })

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)

	select {
	case req := <-got:
		assert.Equal(t, inst.ID, req.ID, "the teardown must name the session that actually finished")
		assert.Equal(t, "nightly", req.Title)
	case <-time.After(20 * time.Second):
		t.Fatal("the declared teardown never ran")
	}
}

// TestTaskSessionLifecycle_DeferredWhileAttached is the paused-path gap: a run
// can finish while a TUI is attached, and that path spends the completion edge
// without being able to act on it. Losing the edge there skipped the declared
// policy permanently — the exact leak #2595 exists to fix, through the one door
// the first cut did not cover.
func TestTaskSessionLifecycle_DeferredWhileAttached(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-archive")
	stubTaskLifecycle(t, "task-archive", task.OnCompleteArchive)

	was := endRunOnIdleEdge(t, inst)
	manager.deferTaskSessionLifecycleWhilePaused(repoID, inst, was)

	// Nothing happens while the intent is parked.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, session.LiveReady, inst.GetLiveness(), "the attach must not be torn down under")

	manager.applyDeferredTaskSessionLifecycle(repoID, inst)
	require.Eventually(t, func() bool {
		return inst.GetLiveness() == session.LiveArchived
	}, 20*time.Second, 25*time.Millisecond, "the owed lifecycle must be applied once the attach ends")
}

// TestTaskSessionLifecycle_DeferredIntentDropsWhenTheUserAdoptsTheSession: the
// rule the edge enforces has to survive the deferral. A user who attached, read
// the result, and set the session working again has adopted it.
func TestTaskSessionLifecycle_DeferredIntentDropsWhenTheUserAdoptsTheSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	was := endRunOnIdleEdge(t, inst)
	manager.deferTaskSessionLifecycleWhilePaused(repoID, inst, was)

	// The user picks the work back up during the attach.
	inst.SetStatusForTest(session.Running)
	manager.applyDeferredTaskSessionLifecycle(repoID, inst)

	time.Sleep(200 * time.Millisecond)
	manager.mu.Lock()
	_, present := manager.instances[daemonInstanceKey(repoID, "nightly")]
	manager.mu.Unlock()
	assert.True(t, present, "work a user started is theirs, not the task's")

	// And the intent is spent, so returning to idle does not reap it later.
	inst.SetStatusForTest(session.Ready)
	manager.applyDeferredTaskSessionLifecycle(repoID, inst)
	time.Sleep(200 * time.Millisecond)
	manager.mu.Lock()
	_, stillPresent := manager.instances[daemonInstanceKey(repoID, "nightly")]
	manager.mu.Unlock()
	assert.True(t, stillPresent, "a dropped intent must not resurface")
}

// TestTaskSessionLifecycle_WaitsForPostWorktreeHooks: post_worktree_commands run
// concurrently with the agent (task.WaitForReady deliberately does not charge a
// slow build hook against the startup budget), so a short task can finish while
// its own provisioning is mid-flight. Archiving MOVES that worktree and killing
// REMOVES it, either of which pulls the ground out from under a running hook.
func TestTaskSessionLifecycle_WaitsForPostWorktreeHooks(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	hooks := make(chan struct{})
	reaped := make(chan struct{})
	prev := killSessionForLifecycle
	killSessionForLifecycle = func(m *Manager, req KillSessionRequest) error {
		close(reaped)
		return nil
	}
	t.Cleanup(func() { killSessionForLifecycle = prev })

	go manager.runTaskSessionLifecycle(repoID, inst.ID, "nightly", "task-kill", task.OnCompleteKill, hooks)

	select {
	case <-reaped:
		t.Fatal("the teardown ran while post-worktree hooks were still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(hooks)
	select {
	case <-reaped:
	case <-time.After(20 * time.Second):
		t.Fatal("the teardown never ran after the hooks finished")
	}
}

// TestTaskSessionLifecycle_ParkedIntentIsReclaimedWhenTheSessionGoes: an intent
// is drained by a normal poll of the session that owes it, so a session killed
// while one is parked would never come back to collect. Leaking a map entry
// inside the change that exists to stop leaks is not acceptable, and it is the
// same reclamation the paused-poll lease sweep performs.
func TestTaskSessionLifecycle_ParkedIntentIsReclaimedWhenTheSessionGoes(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-archive")
	stubTaskLifecycle(t, "task-archive", task.OnCompleteArchive)

	was := endRunOnIdleEdge(t, inst)
	manager.deferTaskSessionLifecycleWhilePaused(repoID, inst, was)

	key := daemonInstanceKey(repoID, "nightly")
	manager.mu.Lock()
	_, parked := manager.deferredTaskLifecycle[key]
	manager.mu.Unlock()
	require.True(t, parked, "precondition: the intent is parked")

	// The session goes away without ever being polled again.
	manager.mu.Lock()
	delete(manager.instances, key)
	manager.sweepDeferredTaskLifecycleLocked()
	_, stillParked := manager.deferredTaskLifecycle[key]
	manager.mu.Unlock()
	assert.False(t, stillParked, "an intent whose session is gone must be reclaimed")
}

// TestTaskSessionLifecycle_ParkedIntentIsReclaimedOnTitleReuse: the map is keyed
// by daemon instance key, which is a title — so a same-titled replacement must
// not inherit the original's owed teardown.
func TestTaskSessionLifecycle_ParkedIntentIsReclaimedOnTitleReuse(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	was := endRunOnIdleEdge(t, inst)
	manager.deferTaskSessionLifecycleWhilePaused(repoID, inst, was)

	// A different session now holds the title.
	replacement, err := session.NewInstance(session.InstanceOptions{
		Title: "nightly", Path: repoPath, Program: "claude", TaskID: "task-kill",
	})
	require.NoError(t, err)
	key := daemonInstanceKey(repoID, "nightly")
	manager.mu.Lock()
	manager.instances[key] = replacement
	manager.sweepDeferredTaskLifecycleLocked()
	_, stillParked := manager.deferredTaskLifecycle[key]
	manager.mu.Unlock()
	assert.False(t, stillParked, "a replacement session must not inherit the original's owed teardown")
}
