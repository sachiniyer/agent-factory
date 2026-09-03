package app

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The snapshot poll's arming half (#3626).
//
// The automations rail reads tasks.json every 750ms (#1168), and a disk read can
// observe nothing about the running scheduler — WithScheduleHealth clears
// Arming/NextRunAt precisely because it would be inventing them. So the rail fell
// back to evaluating the cron expression, which renders a confident "next Mar 04
// 09:00" for a task no daemon is holding: the reading that let two hourly tasks
// go dark for 18 days while every surface called them healthy (#3623).
//
// The fix is here rather than in the rail: the poll that already reads the tasks
// also asks the daemon what it has armed, so the rail keeps its single input.

func armingPollHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	root := t.TempDir()
	repo := &config.RepoContext{Root: root, ID: config.RepoIDFromRoot(root)}
	require.NoError(t, task.AddTask(task.Task{
		ID: "t1", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}))
	h.switchProject(repo)
	return h
}

// observedFromStore models what controlServer.ListTasks returns: the store's own
// rows, read through the same loader the rail reads through and NUMBERED as that
// loader numbers them (task.Task.Ordinal), with the live arming layered on top
// exactly as daemon/task_health.go's withLiveArming layers it.
//
// Built from the store rather than by hand on purpose. A hand-written
// observation is free to omit the row number, and an observation with no row
// identifies nothing — every answer would come back UNKNOWN and the assertions
// below would pass or fail for reasons that have nothing to do with what they
// claim to pin (#3680). Returning what the real read returns is what keeps this
// fake honest.
func observedFromStore(t *testing.T, layer func(row int, o *task.Task)) []task.Task {
	t.Helper()
	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.NotEmpty(t, tasks, "the fake must answer about a store that actually holds rows")
	for i := range tasks {
		layer(i+1, &tasks[i])
	}
	return tasks
}

func TestFetchSnapshotCmdCarriesLiveArmingOntoTheDiskRecord(t *testing.T) {
	h := armingPollHome(t)
	next := time.Date(2026, time.March, 4, 9, 0, 0, 0, time.UTC)
	t.Cleanup(SetLiveTaskArmingFetcherForTest(func() ([]task.Task, error) {
		return observedFromStore(t, func(_ int, o *task.Task) {
			o.Arming, o.NextRunAt = task.ArmingArmed, &next
		}), nil
	}))

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.NoError(t, msg.tasksErr)
	require.Len(t, msg.tasks, 1)
	assert.Equal(t, task.ArmingArmed, msg.tasks[0].Arming,
		"the poll must carry what the daemon is actually holding")
	require.NotNil(t, msg.tasks[0].NextRunAt, "and the armed entry's own next fire")
	assert.Equal(t, next, *msg.tasks[0].NextRunAt)
}

func TestFetchSnapshotCmdLeavesArmingUnknownWhenTheDaemonCannotAnswer(t *testing.T) {
	// A FAILED READ IS NOT AN EMPTY RESULT. Reporting not-armed here would accuse
	// every task on the box of being broken because one poll missed — and the rail
	// renders not-armed as "will not fire", which is the alarm this whole feature
	// exists to make trustworthy.
	h := armingPollHome(t)
	t.Cleanup(SetLiveTaskArmingFetcherForTest(func() ([]task.Task, error) {
		return nil, errors.New("dial unix daemon-http.sock: connect: no such file or directory")
	}))

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.NoError(t, msg.tasksErr, "a failed arming read must not fail the task read beside it")
	require.Len(t, msg.tasks, 1, "nor blank the rail")
	assert.Equal(t, task.ArmingUnknown, msg.tasks[0].Arming)
	assert.Nil(t, msg.tasks[0].NextRunAt)
}

func TestFetchSnapshotCmdRefusesArmingForAStaleDefinition(t *testing.T) {
	// The two reads straddle an edit: the daemon answered about the expression it
	// armed, the disk already holds the new one. Adopting the observation would
	// promise a fire time for a schedule that is no longer the schedule.
	h := armingPollHome(t)
	next := time.Date(2026, time.March, 4, 9, 0, 0, 0, time.UTC)
	t.Cleanup(SetLiveTaskArmingFetcherForTest(func() ([]task.Task, error) {
		return observedFromStore(t, func(_ int, o *task.Task) {
			o.CronExpr = "0 21 * * *" // what the daemon armed; the disk already moved on
			o.Arming, o.NextRunAt = task.ArmingArmed, &next
		}), nil
	}))

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.Len(t, msg.tasks, 1)
	assert.Equal(t, task.ArmingUnknown, msg.tasks[0].Arming)
	assert.Nil(t, msg.tasks[0].NextRunAt)
}

func TestFetchSnapshotCmdCarriesNotArmed(t *testing.T) {
	// The negative observation is the whole of #2929: an enabled task the daemon
	// refused to arm looks identical to a healthy one from disk alone.
	h := armingPollHome(t)
	t.Cleanup(SetLiveTaskArmingFetcherForTest(func() ([]task.Task, error) {
		return observedFromStore(t, func(_ int, o *task.Task) {
			o.Arming = task.ArmingNotArmed
		}), nil
	}))

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.Len(t, msg.tasks, 1)
	assert.Equal(t, task.ArmingNotArmed, msg.tasks[0].Arming)
}

func TestFetchSnapshotCmdPairsAScopedRowWithItsOwnRowInTheAnswer(t *testing.T) {
	// The production shape, end to end. The rail loads ONE project's rows; the
	// daemon answers about every project. The two lists therefore have different
	// lengths and different indexes, so what pairs them has to travel ON the
	// record — the row each one occupies in tasks.json (#3680).
	//
	// A duplicated id across two projects is the case that makes it matter. The
	// daemon skips a duplicate GLOBALLY, first row wins (#855), so it arms row 1
	// and answers not-armed for row 2 — and row 2 is the only row this rail
	// displays, first in its own list and second in the store. Its warning is
	// exactly the one worth keeping: that task will never fire.
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, root := range []string{rootA, rootB} {
		require.NoError(t, task.AddTask(task.Task{
			ID: "dup00001", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *",
			ProjectPath: root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
		}))
	}
	h.switchProject(&config.RepoContext{Root: rootB, ID: config.RepoIDFromRoot(rootB)})

	next := time.Date(2026, time.March, 4, 9, 0, 0, 0, time.UTC)
	t.Cleanup(SetLiveTaskArmingFetcherForTest(func() ([]task.Task, error) {
		return observedFromStore(t, func(row int, o *task.Task) {
			if row == 1 {
				o.Arming, o.NextRunAt = task.ArmingArmed, &next
				return
			}
			o.Arming = task.ArmingNotArmed
		}), nil
	}))

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.NoError(t, msg.tasksErr)
	require.Len(t, msg.tasks, 1, "the rail holds project B's row and nothing else")
	assert.Equal(t, 2, msg.tasks[0].Ordinal,
		"and it is still row 2 of the store, though it is item 0 of this list")
	assert.Equal(t, task.ArmingNotArmed, msg.tasks[0].Arming,
		"so it takes the answer about ITS row, not the armed one about the other project's")
	assert.Nil(t, msg.tasks[0].NextRunAt, "with no fire time, because nothing is holding it")
}

func TestLiveTaskArmingFetcherSkipsARemoteTarget(t *testing.T) {
	// The rail's task DEFINITIONS come from the local tasks.json, so an
	// observation from the daemon named by --daemon-url / AF_DAEMON_URL is about a
	// different machine's tasks. Mostly that would just read UNKNOWN — but two
	// stores sharing a task id and a cron expression would copy a remote task's
	// arming onto an unrelated local one, and a fabricated "armed" is the false
	// clean bill this whole feature exists to remove (#3626 review).
	//
	// The address is deliberately one nothing is listening on: if the guard were
	// removed, this would attempt a dial and come back with a transport error
	// rather than the clean "nothing observed" below.
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:9")
	require.True(t, apiclient.IsRemoteTarget(), "the fixture must actually select a remote target")

	tasks, err := liveTaskArmingFetcher()
	require.NoError(t, err, "a remote target is not an error, it is a question this fetch does not answer")
	assert.Nil(t, tasks, "and no observation, which leaves every record ArmingUnknown")
}
