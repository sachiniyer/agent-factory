package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// backfilledGenerationForTest has the form the task store's upgrade backfill
// mints. Tests that construct rows by hand use it to stand for a backfilled row.
const backfilledGenerationForTest = "legacy-0123456789abcdef0123456789abcdef"

// writePreFieldTasks replaces tasks.json with rows as a binary from before
// generation_id existed would have written them: no generation and no retained
// repo binding.
func writePreFieldTasks(t *testing.T, rows ...task.Task) {
	t.Helper()
	for i := range rows {
		require.Empty(t, rows[i].GenerationID, "precondition: a pre-field row has no generation")
		rows[i].RepoID = ""
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	path, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, raw, 0o644))
}

// TestPreFieldTaskAppliesOnCompleteAfterUpgrade is the upgrade regression the
// #4224 review blocked on. A task row written before generation_id existed
// declares on_complete = archive. After this daemon arms it, and the scheduler's
// real RunTask fires it, the session that run creates must be archived when the
// run ends, as it was before generations existed.
//
// Without the backfill, the arming load returns the row with an empty
// generation (the first require below). The run's session is then stamped with
// the empty generation, taskSessionLifecycle answers keep, and the final
// Eventually times out with the session still live.
func TestPreFieldTaskAppliesOnCompleteAfterUpgrade(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	legacy := enabledCronTask("legacy42", repoPath)
	legacy.OnComplete = task.OnCompleteArchive
	writePreFieldTasks(t, legacy)

	// Daemon start arms task automation before any cron entry can fire.
	require.NoError(t, armTaskAutomation(manager, newTaskScheduler(), nil))
	armed, err := task.GetTask(legacy.ID)
	require.NoError(t, err)
	require.NotEmpty(t, armed.GenerationID,
		"arming must durably give a pre-field row a generation; RunTask reads it from disk")
	assert.True(t, task.IsBackfilledGeneration(armed.GenerationID))
	assert.Equal(t, task.OnCompleteArchive, armed.SessionLifecycle())

	var run *session.Instance
	previous := createSessionForTask
	createSessionForTask = func(req CreateSessionRequest) (*session.InstanceData, error) {
		run = registerTaskSpawnedSessionForGeneration(
			t, manager, repoID, repoPath, "nightly-after-upgrade", req.TaskID, req.TaskGenerationID,
		)
		data := run.ToInstanceData()
		return &data, nil
	}
	t.Cleanup(func() { createSessionForTask = previous })

	require.NoError(t, RunTask(legacy.ID, task.ProjectExpectation{}))
	require.NotNil(t, run, "precondition: the run created its session")
	assert.Equal(t, armed.GenerationID, run.TaskRun().TaskGenerationID,
		"the run's session carries the backfilled generation")

	was := endRunOnIdleEdge(t, run)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, run, was)
	require.Eventually(t, func() bool {
		return run.GetLiveness() == session.LiveArchived
	}, 20*time.Second, 25*time.Millisecond,
		"a pre-field task's declared on_complete must apply to runs started after the upgrade")
}

// A session stamped with the empty generation before the backfill stays kept:
// the empty generation cannot tell this row's runs apart from a removed
// namesake's. The backfill changes which generation new runs carry, not this.
func TestPreBackfillSessionStaysKeptAfterBackfill(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	legacy := enabledCronTask("legacy43", repoPath)
	legacy.OnComplete = task.OnCompleteKill
	writePreFieldTasks(t, legacy)
	inst := registerTaskSpawnedSessionForGeneration(
		t, manager, repoID, repoPath, "started-before-upgrade", legacy.ID, "",
	)

	require.NoError(t, armTaskAutomation(manager, newTaskScheduler(), nil))
	armed, err := task.GetTask(legacy.ID)
	require.NoError(t, err)
	require.True(t, task.IsBackfilledGeneration(armed.GenerationID), "precondition: the row was backfilled")

	verb, err := manager.taskSessionLifecycle(repoID, legacy.ID, "")
	require.NoError(t, err)
	assert.Equal(t, task.OnCompleteKeep, verb)

	was := endRunOnIdleEdge(t, inst)
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)
	time.Sleep(200 * time.Millisecond)
	manager.mu.Lock()
	_, stillRegistered := manager.instances[daemonInstanceKey(repoID, inst.Title)]
	manager.mu.Unlock()
	assert.True(t, stillRegistered, "a pre-backfill session must not be killed by the backfilled row's policy")
}

// A pre-upgrade run still in flight holds a slot of the row the upgrade
// backfilled, so the first runs after the upgrade cannot exceed
// max_concurrent_runs. It does not hold a slot of a row an add minted.
//
// Without taskRunChargesGeneration's backfill case, the first count is zero:
// the empty generation matched only an empty-generation row, and the backfill
// removed that row's empty generation.
func TestPreUpgradeRunChargesBackfilledGenerationCapacity(t *testing.T) {
	const (
		repoID  = "repo-id"
		taskID  = "legacy-task"
		freshID = "0123456789abcdef0123456789abcdef"
	)
	require.True(t, task.IsBackfilledGeneration(backfilledGenerationForTest))
	require.False(t, task.IsBackfilledGeneration(freshID))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "pre-upgrade-run", Path: t.TempDir(), Program: "claude", TaskID: taskID,
	})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Running)
	legacyKey := taskRunReservationKey(repoID, taskID, "")
	manager := &Manager{
		instances:        map[string]*session.Instance{daemonInstanceKey(repoID, inst.Title): inst},
		reservedTaskRuns: map[string]int{legacyKey: 1},
		ghostTaskRuns:    map[string]int{legacyKey: 1},
	}

	assert.Equal(t, 3, manager.countTaskRunsLocked(repoID, taskID, backfilledGenerationForTest),
		"live, reserved and unloadable pre-upgrade runs all hold the backfilled row's slots")
	assert.ErrorIs(t, manager.admitTaskRunLocked(repoID, taskID, backfilledGenerationForTest, 3),
		errAtConcurrencyLimit)
	assert.Equal(t, 0, manager.countTaskRunsLocked(repoID, taskID, freshID),
		"a task an add minted is a new incarnation and owns none of the empty generation's runs")
	assert.Equal(t, 3, manager.countTaskRunsLocked(repoID, taskID, ""),
		"the empty generation still counts its own runs")
}

// A watch backlog queued before the upgrade lives under the task-ID-only stem.
// The upgrade backfills a generation onto the row, which moves its queue stem.
// The backlog must follow the row, not be swept as a removed generation's.
//
// Without adoption, the second supervisor opens an empty generation-qualified
// queue, cleanOrphanQueues deletes the legacy file, and the replay wait below
// times out.
func TestPreUpgradeWatchBacklogFollowsBackfilledGeneration(t *testing.T) {
	dir := t.TempDir()
	const id = "ab13b001"

	s1, _ := newTestSupervisor(t, staticTasks(watchTask(id, `echo e1; echo e2; sleep 60`, dir)))
	fd1 := &flakyDeliver{}
	s1.deliver = fd1.deliver
	queueDir, _ := s1.queueDir()
	require.NoError(t, s1.Reload())
	waitUntil(t, 10*time.Second, "pre-upgrade backlog to persist", func() bool {
		return newEventQueue(queueDir, id).pendingCount() == 2
	})
	s1.Stop()

	upgraded := watchTask(id, `sleep 60`, dir)
	upgraded.GenerationID = backfilledGenerationForTest
	s2, _ := newTestSupervisor(t, staticTasks(upgraded))
	fd2 := &flakyDeliver{}
	fd2.healed.Store(true)
	s2.deliver = fd2.deliver
	s2.queueDir = func() (string, error) { return queueDir, nil }
	require.NoError(t, s2.Reload())
	waitUntil(t, 10*time.Second, "the adopted backlog to replay in order", func() bool {
		got := fd2.delivered()
		return len(got) == 2 && got[0] == "e1" && got[1] == "e2"
	})
	for _, suffix := range []string{".jsonl", ".cursor"} {
		_, err := os.Stat(filepath.Join(queueDir, id+suffix))
		assert.True(t, os.IsNotExist(err), "the legacy %s file must be moved, not left behind: %v", suffix, err)
	}
}

// A disabled backfilled row has no watcher to adopt its legacy backlog yet. The
// reload must keep that backlog for re-enable, as it keeps a disabled task's own
// backlog (#1129), and re-enabling must replay it.
//
// Without ownsQueueStem's legacy case, the first reload deletes the file, which
// fails the first assertion.
func TestDisabledBackfilledRowKeepsLegacyBacklogForReenable(t *testing.T) {
	dir := t.TempDir()
	queueDir := t.TempDir()
	const id = "ab13b002"
	legacyQueue := newEventQueue(queueDir, id)
	require.NoError(t, legacyQueue.enqueue("queued before the upgrade"))
	require.Equal(t, 1, legacyQueue.pendingCount())

	upgraded := watchTask(id, `sleep 60`, dir)
	upgraded.GenerationID = backfilledGenerationForTest
	upgraded.Enabled = false
	current := upgraded
	s, _ := newTestSupervisor(t, func() ([]task.Task, error) { return []task.Task{current}, nil })
	fd := &flakyDeliver{}
	fd.healed.Store(true)
	s.deliver = fd.deliver
	s.queueDir = func() (string, error) { return queueDir, nil }
	require.NoError(t, s.Reload())
	_, err := os.Stat(filepath.Join(queueDir, id+".jsonl"))
	require.NoError(t, err, "a disabled backfilled row's legacy backlog must survive the reload")

	current.Enabled = true
	require.NoError(t, s.Reload())
	waitUntil(t, 10*time.Second, "the re-enabled row to replay its legacy backlog", func() bool {
		got := fd.delivered()
		return len(got) == 1 && got[0] == "queued before the upgrade"
	})
}

// Only a backfilled row adopts. A row an add minted is a new incarnation, so a
// removed namesake's legacy backlog is swept, never replayed into it.
func TestMintedGenerationDoesNotAdoptLegacyBacklog(t *testing.T) {
	dir := t.TempDir()
	queueDir := t.TempDir()
	const id = "ab13b003"
	legacyQueue := newEventQueue(queueDir, id)
	require.NoError(t, legacyQueue.enqueue("a removed namesake's event"))

	replacement := watchTask(id, `sleep 60`, dir)
	replacement.GenerationID = "0123456789abcdef0123456789abcdef"
	s, _ := newTestSupervisor(t, staticTasks(replacement))
	fd := &flakyDeliver{}
	fd.healed.Store(true)
	s.deliver = fd.deliver
	s.queueDir = func() (string, error) { return queueDir, nil }
	require.NoError(t, s.Reload())
	waitUntil(t, 10*time.Second, "the legacy backlog to be swept", func() bool {
		_, err := os.Stat(filepath.Join(queueDir, id+".jsonl"))
		return os.IsNotExist(err)
	})
	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, fd.delivered(), "a replacement task must not replay its predecessor's backlog")
}

// Adoption finishes a move a crash interrupted, and never pairs a legacy cursor
// with a different log.
func TestAdoptLegacyEventQueueResumesAndRefusesConflicts(t *testing.T) {
	t.Run("resumes after the log moved", func(t *testing.T) {
		dir := t.TempDir()
		owned := eventQueueStem("resume01", backfilledGenerationForTest)
		require.NoError(t, os.WriteFile(filepath.Join(dir, owned+".jsonl"), []byte("{}\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "resume01.cursor"), []byte("3"), 0o644))

		require.NoError(t, adoptLegacyEventQueue(dir, "resume01", backfilledGenerationForTest))
		raw, err := os.ReadFile(filepath.Join(dir, owned+".cursor"))
		require.NoError(t, err)
		assert.Equal(t, "3", string(raw))
		_, err = os.Stat(filepath.Join(dir, "resume01.cursor"))
		assert.True(t, os.IsNotExist(err))
	})
	t.Run("a conflict moves neither file", func(t *testing.T) {
		dir := t.TempDir()
		owned := eventQueueStem("clash001", backfilledGenerationForTest)
		require.NoError(t, os.WriteFile(filepath.Join(dir, owned+".jsonl"), []byte("new\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "clash001.jsonl"), []byte("old\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "clash001.cursor"), []byte("4"), 0o644))

		err := adoptLegacyEventQueue(dir, "clash001", backfilledGenerationForTest)
		require.ErrorIs(t, err, errLegacyEventQueueConflict)
		_, statErr := os.Stat(filepath.Join(dir, owned+".cursor"))
		assert.True(t, os.IsNotExist(statErr), "the legacy cursor must not be paired with the generation's log")
		_, statErr = os.Stat(filepath.Join(dir, "clash001.cursor"))
		assert.NoError(t, statErr)
	})
	t.Run("a minted generation adopts nothing", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "minted01.jsonl"), []byte("x\n"), 0o644))
		require.NoError(t, adoptLegacyEventQueue(dir, "minted01", "0123456789abcdef0123456789abcdef"))
		_, err := os.Stat(filepath.Join(dir, "minted01.jsonl"))
		assert.NoError(t, err)
	})
}

// A delivery that read a hand-edited, pre-field row just before a load
// backfilled it must still be admitted, and it must run under the generation
// the row now stores. This is the integration failure on 28a6f0fa1:
// `af tasks trigger` on a freshly hand-written tasks.json was refused with
// "task task-one was replaced before its run was admitted", because the create
// path's target-relationship load backfilled the row between RunTask's read
// and admission. At 28a6f0fa1 the first require below fails with that error.
func TestAdmissionAcceptsReadFromBeforeBackfill(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	writePreFieldTasks(t, enabledCronTask("inflight", repoPath))
	read, err := task.GetTask("inflight")
	require.NoError(t, err)
	require.Empty(t, read.GenerationID, "precondition: the caller read the pre-field row")
	_, _, err = task.LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	stored, err := task.GetTask("inflight")
	require.NoError(t, err)
	require.True(t, task.IsBackfilledGeneration(stored.GenerationID), "precondition: backfilled meanwhile")

	admission, err := manager.nextTaskRunAdmission("inflight", read.GenerationID)
	require.NoError(t, err, "the backfill is not a replacement")
	assert.Equal(t, stored.GenerationID, admission.generationID,
		"the admitted run carries the generation the row stores, so on_complete applies to it")

	minted := addStatusTestTask(t, enabledCronTask("minted03", repoPath))
	_, err = manager.nextTaskRunAdmission(minted.ID, "")
	require.Error(t, err, "an empty generation never names a row an add minted")
	assert.Contains(t, err.Error(), "was replaced before its run was admitted")
}
