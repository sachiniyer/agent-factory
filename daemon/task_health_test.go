package daemon

import (
	"context"
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
	last := time.Now().Add(-18 * 24 * time.Hour)
	dark.LastRunAt = &last
	require.NoError(t, task.AddTask(dark))

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
