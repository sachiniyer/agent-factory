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

	// Not Ready: the run marker is clear but the session is not a healthy idle
	// one, which is the shape an uncertain create leaves behind.
	inst.SetStatusForTest(session.Loading)
	assert.False(t, runEndedIntoIdle(inst, true), "only a session that settled idle may be reaped")
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
