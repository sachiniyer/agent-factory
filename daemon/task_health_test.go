package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The live half of schedule health (#3623): the arming state and next-run time
// that only the process holding the scheduler can observe, plus the audit actor
// that only the client can declare.

// TestListTasks_NextRunComesFromTheArmedEntry: the number a user reads has to be
// what the scheduler will actually fire, not a re-evaluation of the expression.
// Both were the same value before this change, which is exactly why an unarmed
// task could render a confident "next".
func TestListTasks_NextRunComesFromTheArmedEntry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1001", "")))

	srv := &controlServer{scheduler: newTaskScheduler()}
	require.NoError(t, srv.scheduler.Reload())
	srv.scheduler.Start()
	t.Cleanup(srv.scheduler.Stop)

	var resp ListTasksResponse
	require.NoError(t, srv.ListTasks(ListTasksRequest{}, &resp))
	require.Len(t, resp.Tasks, 1)

	assert.Equal(t, task.ArmingArmed, resp.Tasks[0].Arming)
	require.NotNil(t, resp.Tasks[0].NextRunAt, "an armed task reports when it will fire")
	entry := srv.scheduler.cron.Entry(srv.scheduler.entries["aaaa1001"])
	assert.True(t, entry.Next.Equal(*resp.Tasks[0].NextRunAt),
		"the reported time must be the entry's own, read off the scheduler")

	// And the snapshot is what the response was built from, keyed by task ID.
	snapshot, observed := srv.scheduler.armingSnapshot()
	require.True(t, observed)
	assert.True(t, entry.Next.Equal(snapshot["aaaa1001"]))
}

// TestListTasks_UnarmedEnabledTaskHasNoNextRun generalizes #2929: whatever the
// reason the daemon is not holding an enabled task, its absence from the
// scheduler is reported instead of a fire time it will never reach.
func TestListTasks_UnarmedEnabledTaskHasNoNextRun(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1002", "")))

	// A scheduler that has completed a reload, from a snapshot that did not
	// include this task — the shape of every arming refusal.
	srv := &controlServer{scheduler: newTaskScheduler()}
	require.NoError(t, srv.scheduler.reloadTasks(nil))

	var resp ListTasksResponse
	require.NoError(t, srv.ListTasks(ListTasksRequest{}, &resp))
	require.Len(t, resp.Tasks, 1)

	assert.Equal(t, task.ArmingNotArmed, resp.Tasks[0].Arming)
	assert.Nil(t, resp.Tasks[0].NextRunAt,
		"absence is the signal: a task nothing is holding has no next run")
	assert.True(t, resp.Tasks[0].Enabled, "and it is still enabled on disk, which is the whole problem")
}

// TestListTasks_ArmingIsUnknownBeforeTheFirstReload is the fabricated-negative
// guard. The control socket accepts reads while the daemon is still warming up,
// and an empty entry map in that window means "arming has not run yet", not
// "nothing is armed" — reporting the latter would call every task on a starting
// daemon broken.
func TestListTasks_ArmingIsUnknownBeforeTheFirstReload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1003", "")))

	srv := &controlServer{scheduler: newTaskScheduler()}

	var resp ListTasksResponse
	require.NoError(t, srv.ListTasks(ListTasksRequest{}, &resp))
	require.Len(t, resp.Tasks, 1)
	assert.Equal(t, task.ArmingUnknown, resp.Tasks[0].Arming)
	assert.Nil(t, resp.Tasks[0].NextRunAt)
}

// TestListTasks_CarriesTheOverdueDerivation: the daemon's read is the one every
// client goes through, so the pure derivation has to survive it too.
func TestListTasks_CarriesTheOverdueDerivation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dark := enabledCronTask("aaaa1004", "")
	dark.CronExpr = "20 * * * *"
	require.NoError(t, task.AddTask(dark))
	// Through the scheduler-owned writer: a create supplies the task's
	// definition and the store supplies its history (task.resetStoreOwnedFields).
	last := time.Now().Add(-18 * 24 * time.Hour)
	require.NoError(t, task.UpdateTaskStatus("aaaa1004", &last, "started"))

	srv := &controlServer{scheduler: newTaskScheduler()}
	var resp ListTasksResponse
	require.NoError(t, srv.ListTasks(ListTasksRequest{}, &resp))
	require.Len(t, resp.Tasks, 1)
	assert.True(t, resp.Tasks[0].Overdue)
	assert.Positive(t, resp.Tasks[0].MissedOccurrences)
}

// TestWatchTaskArming: a watch task has no schedule, so arming is its whole
// signal — and a supervisor that has not reloaded yet says unknown, like the
// scheduler.
func TestWatchTaskArming(t *testing.T) {
	supervisor := newWatcherSupervisor()
	assert.Equal(t, task.ArmingUnknown, supervisor.armingFor("w1"),
		"before the first reload nothing has been observed")

	require.NoError(t, supervisor.reloadSnapshot(nil, nil))
	assert.Equal(t, task.ArmingNotArmed, supervisor.armingFor("w1"),
		"after a reload that did not include it, the task is genuinely not armed")
}

// TestTaskActor_DeclaredSurfaceWins: the TUI reaches the daemon over the same
// HTTP routes the web UI uses, so the transport cannot tell them apart and the
// declared label is what makes the audit trail useful.
func TestTaskActor_DeclaredSurfaceWins(t *testing.T) {
	httpCtx := context.WithValue(context.Background(), rpcRequesterContextKey{}, "HTTP operator peer 127.0.0.1:1234")

	assert.Equal(t, task.ActorTUI, taskActor(httpCtx, string(task.ActorTUI)))
	assert.Equal(t, task.ActorAPI, taskActor(httpCtx, ""),
		"an HTTP client that declares nothing is the API surface")
	assert.Equal(t, task.ActorUnknown, taskActor(context.Background(), ""),
		"a control-socket client that declares nothing is unknown, never assumed to be the CLI")
	assert.Equal(t, task.ActorCLI, taskActor(context.Background(), string(task.ActorCLI)))
	assert.Equal(t, task.ActorAPI, taskActor(httpCtx, "not-a-surface"),
		"an unrecognized label falls back rather than being stored verbatim")
}

// TestControlAddTask_RecordsTheAuditActor and its update sibling pin the trail
// end to end through the RPCs every client actually uses.
func TestControlAddTask_RecordsTheAuditActor(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	srv := &controlServer{scheduler: newTaskScheduler()}

	var resp AddTaskResponse
	require.NoError(t, srv.AddTask(
		AddTaskRequest{Task: enabledCronTask("aaaa1005", ""), Actor: string(task.ActorCLI)}, &resp))

	stored, err := task.GetTask("aaaa1005")
	require.NoError(t, err)
	require.Len(t, stored.Audit, 1)
	assert.Equal(t, task.AuditCreated, stored.Audit[0].Action)
	assert.Equal(t, task.ActorCLI, stored.Audit[0].Actor)
}

func TestControlUpdateTask_RecordsTheDisableAndItsSurface(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa1006", "")))
	srv := &controlServer{scheduler: newTaskScheduler()}

	off := false
	var resp UpdateTaskResponse
	require.NoError(t, srv.UpdateTask(UpdateTaskRequest{
		ID: "aaaa1006", Update: task.TaskUpdate{Enabled: &off}, Actor: string(task.ActorTUI),
	}, &resp))

	stored, err := task.GetTask("aaaa1006")
	require.NoError(t, err)
	// The seed's own create is entry 0, recorded as unknown because task.AddTask
	// declares no surface; the disable is what this test is about.
	require.Len(t, stored.Audit, 2)
	assert.Equal(t, task.AuditDisabled, stored.Audit[1].Action)
	assert.Equal(t, task.ActorTUI, stored.Audit[1].Actor)
	assert.Equal(t, []string{"enabled"}, stored.Audit[1].Fields)

	// And the response proves the disarm actually took effect rather than
	// asserting that it should have.
	assert.Equal(t, task.ArmingNotArmed, resp.Task.Arming)
	assert.Nil(t, resp.Task.NextRunAt)
}

// TestArmingSnapshot_SurvivesAConcurrentReload reproduces the mechanism rather
// than hoping for the race: reloadTasks REPLACES every cron entry on each task
// write, so a snapshot that reads s.entries and then queries the cron without
// holding the lock across both sees IDs that no longer exist and reports an
// armed task as not-armed. That false alarm reaches `af doctor` and the task
// list on nothing worse than a concurrent `af tasks update`.
//
// With both halves under one lock the invariant is absolute, so this asserts it
// absolutely: across a hundred snapshots taken while reloads run continuously,
// the task is armed in every single one.
func TestArmingSnapshot_SurvivesAConcurrentReload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	tasks := []task.Task{enabledCronTask("aaaa1007", "")}

	scheduler := newTaskScheduler()
	require.NoError(t, scheduler.reloadTasks(tasks))
	scheduler.Start()
	t.Cleanup(scheduler.Stop)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if err := scheduler.reloadTasks(tasks); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	for i := 0; i < 100; i++ {
		snapshot, observed := scheduler.armingSnapshot()
		require.True(t, observed)
		if _, armed := snapshot["aaaa1007"]; !armed {
			t.Fatalf("snapshot %d reported an armed task as not armed; a reload landed between reading the entry map and the cron", i)
		}
	}
}

// TestWithLiveArming_DuplicateIDsAreArmedAtMostOnce: a hand-edited store can
// hold two rows with the same ID, and both subsystems arm only the FIRST. An
// ID-keyed lookup alone handed the surviving entry to every matching row, so a
// skipped duplicate — a different expression that will never execute — reported
// armed, carrying the other row's next_run_at. Exactly the "looks healthy, never
// runs" shape this whole change exists to remove.
func TestWithLiveArming_DuplicateIDsAreArmedAtMostOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	first := enabledCronTask("dupe0001", "")
	second := enabledCronTask("dupe0001", "")
	second.CronExpr = "45 4 * * *"

	srv := &controlServer{scheduler: newTaskScheduler()}
	require.NoError(t, srv.scheduler.reloadTasks([]task.Task{first, second}))
	srv.scheduler.Start()
	t.Cleanup(srv.scheduler.Stop)

	got := srv.withLiveArming([]task.Task{first, second})
	assert.Equal(t, task.ArmingArmed, got[0].Arming, "the scheduler armed the first occurrence")
	assert.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, task.ArmingNotArmed, got[1].Arming,
		"and the row it skipped must not borrow that entry")
	assert.Nil(t, got[1].NextRunAt)
}

// TestWatcherSupervisor_DuplicateIDsWatchTheFirst pins the rule the annotation
// above relies on. A map assignment quietly kept the LAST occurrence, so the
// watch supervisor and the cron scheduler disagreed about which row a duplicated
// ID meant, and nothing anywhere said a row had been skipped.
func TestWatcherSupervisor_DuplicateIDsWatchTheFirst(t *testing.T) {
	dir := t.TempDir()
	supervisor := newWatcherSupervisor()
	supervisor.queueDir = func() (string, error) { return dir, nil }
	supervisor.logPath = func(string) (string, error) { return filepath.Join(dir, "w.log"), nil }
	supervisor.deliver = func(string, string) error { return nil }
	supervisor.setStatus = func(string, string) {}
	t.Cleanup(supervisor.Stop)

	first := watchTask("dupe0002", "printf 'first\\n'; sleep 30", dir)
	second := watchTask("dupe0002", "printf 'second\\n'; sleep 30", dir)
	require.NoError(t, supervisor.reloadSnapshot([]task.Task{first, second}, []task.Task{first, second}))

	supervisor.mu.Lock()
	w := supervisor.watchers["dupe0002"]
	supervisor.mu.Unlock()
	require.NotNil(t, w)
	assert.Equal(t, watcherSignature(first), w.sig,
		"the first occurrence is the one watched, matching the cron scheduler's rule")
}

// TestFirstOccurrencePerID_ResolvesMixedTriggerDuplicates is the case the
// per-subsystem duplicate checks structurally cannot see. The cron scheduler
// skips watch rows before its own duplicate check and the watch supervisor skips
// cron rows before its own, so an enabled cron row followed by an enabled watch
// row sharing an ID passes BOTH: the scheduler arms the cron entry, the
// supervisor starts the watch process, and each event that process emits is
// delivered by ID — resolving to the first record and running the cron task's
// configuration on someone else's trigger.
//
// Resolving it over the whole ordered list, before anything filters by kind, is
// the only place that sees the collision.
func TestFirstOccurrencePerID_ResolvesMixedTriggerDuplicates(t *testing.T) {
	cronRow := enabledCronTask("mixed001", "")
	watchRow := watchTask("mixed001", "printf 'line\\n'; sleep 30", "")
	other := enabledCronTask("other001", "")

	kept := firstOccurrencePerID([]task.Task{cronRow, watchRow, other})

	require.Len(t, kept, 2, "the duplicate is gone before either subsystem filters by kind")
	assert.Equal(t, "mixed001", kept[0].ID)
	assert.False(t, kept[0].IsWatch(), "first wins, matching the scheduler's own rule")
	assert.Equal(t, "other001", kept[1].ID, "and unrelated tasks keep their order")
}

// TestFirstOccurrencePerID_LeavesDistinctIDsAlone keeps the dedupe from being a
// filter on anything but a genuine collision.
func TestFirstOccurrencePerID_LeavesDistinctIDsAlone(t *testing.T) {
	tasks := []task.Task{enabledCronTask("aaaa2001", ""), enabledCronTask("aaaa2002", "")}
	assert.Equal(t, tasks, firstOccurrencePerID(tasks))
}
