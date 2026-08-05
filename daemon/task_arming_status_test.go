package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// A refused task must not be indistinguishable from a healthy one. Before this,
// a task excluded from arming kept the LastRunStatus its last successful run
// wrote, stayed enabled on disk, and was absent from both cron and watch — with
// armed-ness exposed on no surface at all (#2929).
func TestRecordArmingStatus_WritesTheRefusalWhereUsersLook(t *testing.T) {
	withArmingTaskFile(t)
	seeded := seedArmingTask(t, "refused-task", "started")

	m := &Manager{}
	m.recordArmingStatus(seeded, notArmedStatus(errors.New("target session is archived")))

	after := reloadArmingTask(t, "refused-task")
	require.True(t, strings.HasPrefix(after.LastRunStatus, "errored:"),
		"the refusal must carry the prefix the TUI renders as failed, got %q", after.LastRunStatus)
	require.Contains(t, after.LastRunStatus, "target session is archived",
		"the status must say why, not just that something is wrong")
}

// Arming is a supervision decision, not a run, so the timestamp of the last real
// delivery must survive it.
func TestRecordArmingStatus_PreservesLastRunAt(t *testing.T) {
	withArmingTaskFile(t)
	seeded := seedArmingTask(t, "refused-task", "started")
	require.NotNil(t, seeded.LastRunAt)
	before := *seeded.LastRunAt

	m := &Manager{}
	m.recordArmingStatus(seeded, notArmedStatus(errors.New("unsafe")))

	after := reloadArmingTask(t, "refused-task")
	require.NotNil(t, after.LastRunAt)
	require.True(t, after.LastRunAt.Equal(before),
		"a supervision-status change must not move LastRunAt")
}

// Arming runs on every task CRUD as well as at startup. A persistently refused
// task must not rewrite tasks.json each time.
func TestRecordArmingStatus_IsIdempotent(t *testing.T) {
	withArmingTaskFile(t)
	seeded := seedArmingTask(t, "refused-task", "started")
	status := notArmedStatus(errors.New("unsafe"))

	m := &Manager{}
	m.recordArmingStatus(seeded, status)
	first := reloadArmingTask(t, "refused-task")

	// Feed the already-updated record back in, as the next reload would.
	writes := armingTaskFileWrites(t)
	m.recordArmingStatus(first, status)
	require.Equal(t, writes, armingTaskFileWrites(t),
		"an unchanged arming status must not rewrite the task file")
}

// A refusal must not outlive the condition that caused it: a task whose target
// came back would otherwise keep claiming it is unarmed until its next run — up
// to a day for a nightly schedule, indefinitely for a watch task.
func TestClearStaleNotArmedStatus_ClearsOnlyItsOwnStatus(t *testing.T) {
	withArmingTaskFile(t)
	m := &Manager{}

	t.Run("clears a not-armed status", func(t *testing.T) {
		seeded := seedArmingTask(t, "recovered-task", notArmedStatus(errors.New("was unsafe")))
		m.clearStaleNotArmedStatus(seeded)
		require.Empty(t, reloadArmingTask(t, "recovered-task").LastRunStatus)
	})

	t.Run("leaves a real run failure alone", func(t *testing.T) {
		seeded := seedArmingTask(t, "failed-task", "errored: project path is not a git repository")
		m.clearStaleNotArmedStatus(seeded)
		require.Equal(t, "errored: project path is not a git repository",
			reloadArmingTask(t, "failed-task").LastRunStatus,
			"a genuine run failure must survive arming")
	})

	t.Run("leaves a successful run alone", func(t *testing.T) {
		seeded := seedArmingTask(t, "ok-task", "started")
		m.clearStaleNotArmedStatus(seeded)
		require.Equal(t, "started", reloadArmingTask(t, "ok-task").LastRunStatus)
	})
}

// withArmingTaskFile points the task store at a private home for this test.
func withArmingTaskFile(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
}

// seedArmingTask persists an enabled cron task carrying an existing run status
// and LastRunAt, the shape a task has after a successful run.
func seedArmingTask(t *testing.T, id, status string) task.Task {
	t.Helper()
	tsk := task.Task{
		ID:        id,
		Name:      id,
		Prompt:    "do the thing",
		CronExpr:  "0 3 * * *",
		Program:   "claude",
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(tsk))
	ran := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	require.NoError(t, task.UpdateTaskStatus(id, &ran, status))
	return reloadArmingTask(t, id)
}

// armingTaskFileWrites fingerprints the task file so a test can prove a
// no-op arming pass did not rewrite it.
func armingTaskFileWrites(t *testing.T) string {
	t.Helper()
	path, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return fmt.Sprintf("%d:%d:%x", info.Size(), info.ModTime().UnixNano(), sha256.Sum256(data))
}

func reloadArmingTask(t *testing.T, id string) task.Task {
	t.Helper()
	loaded, err := task.GetTask(id)
	require.NoError(t, err)
	return *loaded
}

// The repair of DROPPING target_session turns a refused task into an ordinary
// cron/watch task, and that path never reaches the target validation — so the
// clear has to happen for everything that lands in the safe set, not only for
// tasks that passed validation. Otherwise the fix leaves behind exactly the
// stale status it exists to eliminate.
func TestClearStaleNotArmedStatus_ClearsWhenTheTargetIsDropped(t *testing.T) {
	withArmingTaskFile(t)
	seeded := seedArmingTask(t, "repaired-task", notArmedStatus(errors.New("target was archived")))
	require.Empty(t, seeded.TargetSession, "seeded without a target: the dropped-target repair shape")

	m := &Manager{}
	snapshot, err := m.persistedTasksForArming()
	require.NoError(t, err)
	require.Empty(t, snapshot.refused)

	require.Empty(t, reloadArmingTask(t, "repaired-task").LastRunStatus,
		"a task armed as an ordinary cron task must not keep claiming it is unarmed")
}
