package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the fix for the silent verdict-drop reported for a targeted
// task delivery whose target_session is absent at fire time: the daemon
// auto-creates the target through DeliverPrompt, but the targeted branch of
// deliverTaskPromptRun never reported the created run's identity back through
// its run out-parameter (so the status write landed unkeyed and advanced the
// row's clock past the session's creation time, refusing every later keyed
// failure verdict as stale), and createMissingPromptTarget never bound the
// created session to the task (so stampTaskRunFailed/stampTaskRunInterrupted
// bailed on TaskID == "" and dropped the verdict with no log). Together that
// left a failed/lost/interrupted auto-created run reading "started" on every
// task surface. The fix reports the created session's identity through the
// DeliverPrompt response and binds the created session to the task.

// fixedCreatedAt is a stable creation time for the auto-created run, so the
// run key the delivery reports is deterministic and a later failure verdict
// can be shown to land against it (not be refused as stale).
var fixedCreatedAt = time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)

// stubDeliverCreated swaps the daemon RPC seam for a delivery that reports a
// freshly auto-created target session, mirroring the fixed daemon's
// createMissingPromptTarget tail: status "started", the created session's
// identity, and (optionally) its initial-prompt verdict.
func stubDeliverCreated(t *testing.T, sessionID string, created time.Time, undelivered bool) {
	t.Helper()
	orig := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		res := taskPromptDeliveryResult{
			status:           "started",
			sessionID:        sessionID,
			sessionCreatedAt: created,
		}
		if undelivered {
			res.lastPromptDeliveryStatus = session.PromptNotDelivered
		}
		return res, nil
	}
	t.Cleanup(func() { deliverPromptForTask = orig })
}

// TestRepro_TargetedAutoCreateKeysRunStatus is the end-to-end pin for the
// run-keying half of the fix. A cron task with target_session fires, the
// target is absent, and the daemon auto-creates it. The run-status write must
// be KEYED to the created session (#5661): LastRunID is the session id and
// LastRunStartedAt is the session's creation time. Before the fix the targeted
// branch left the run out-parameter zero, so the write was unkeyed — it
// cleared LastRunID and advanced the row's clock past the session's creation
// time, which made the failure verdict for that run refuse as stale. The
// second half of the test proves that refusal no longer happens: a later
// failure verdict for the same run is applied.
func TestRepro_TargetedAutoCreateKeysRunStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	stubDeliverCreated(t, "sess-keyed", fixedCreatedAt, false)

	require.NoError(t, task.AddTask(task.Task{
		ID:            "reproA0001",
		Name:          "cap",
		Prompt:        "do it",
		CronExpr:      "0 3 * * *",
		TargetSession: "captain",
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now(),
	}))

	require.NoError(t, RunTask("reproA0001", task.ProjectExpectation{}))

	got, err := task.GetTask("reproA0001")
	require.NoError(t, err)
	assert.Equal(t, "started", got.LastRunStatus,
		"the auto-created run is recorded started")
	assert.Equal(t, "sess-keyed", got.LastRunID,
		"the run-status write must be keyed to the created session, not unkeyed (which cleared LastRunID)")
	require.NotNil(t, got.LastRunStartedAt)
	assert.True(t, got.LastRunStartedAt.Equal(fixedCreatedAt),
		"the run's start time is the session's creation time, so later keyed writers for this run are not refused as stale")

	// The failure verdict the session lifecycle would produce once the
	// auto-created run dies — keyed to the same run — must LAND. Before the
	// fix the unkeyed write advanced the row clock past fixedCreatedAt, so
	// runOrder({sess-keyed, fixedCreatedAt}, unkeyed@later) < 0 refused this.
	later := fixedCreatedAt.Add(time.Minute)
	_, applied, err := task.RecordRunStatus("reproA0001", task.RunKey{ID: "sess-keyed", StartedAt: fixedCreatedAt}, &later, task.RunFailedUndeliveredStatus)
	require.NoError(t, err)
	assert.True(t, applied, "the failure verdict for the auto-created run must land (the row is keyed to it, so it is not refused as stale)")
	assert.Equal(t, task.RunFailedUndeliveredStatus, lastRunStatus(t, "reproA0001"))
}

// TestRepro_TargetedAutoCreateUndeliveredRecordsFailure pins the #5278 half of
// the fix on the targeted branch. A create whose initial prompt provably
// never landed must surface as a failed run — the same verdict the
// create-per-run branch already reports — rather than the healthy "started".
// Before the fix the targeted branch never inspected the created session's
// LastPromptDeliveryStatus, so a provably-undelivered initial prompt was
// recorded as "started".
func TestRepro_TargetedAutoCreateUndeliveredRecordsFailure(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	stubDeliverCreated(t, "sess-undeliv", fixedCreatedAt, true)

	require.NoError(t, task.AddTask(task.Task{
		ID:            "reproB0001",
		Name:          "cap",
		Prompt:        "do it",
		CronExpr:      "0 3 * * *",
		TargetSession: "captain",
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now(),
	}))

	require.NoError(t, RunTask("reproB0001", task.ProjectExpectation{}))

	got, err := task.GetTask("reproB0001")
	require.NoError(t, err)
	assert.Equal(t, taskStatusErroredPromptUndelivered, got.LastRunStatus,
		"a provably-undelivered initial prompt must be recorded as a failed run, not healthy \"started\"")
	assert.Equal(t, "sess-undeliv", got.LastRunID,
		"the failed run is still keyed to the created session")
}

// TestRepro_StampTaskRunFailedRecordsBoundSession pins the binding half of the
// fix. stampTaskRunFailed bails by early return when the session has no TaskID
// (#5661): before the fix the auto-created target had TaskID == "" because
// createMissingPromptTarget never bound it, so a failed/lost auto-created run
// was silently dropped with no log and no event. With the fix the auto-created
// target is bound to the task, so the stamper records the failure on a row
// keyed to the run. The negative sub-test documents the bail that the unbound
// shape produced.
func TestRepro_StampTaskRunFailedRecordsBoundSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	manager, repoID, repoPath := newStatusTestManager(t)

	// A bound, task-spawned session whose run key matches the row the delivery
	// would have keyed — the shape createMissingPromptTarget now produces.
	seedCronTask(t, "reproC0001", repoPath)
	bound, err := session.NewInstance(session.InstanceOptions{
		ID: "sess-bound", CreatedAt: fixedCreatedAt, Title: "captain",
		Path: repoPath, Program: "claude", TaskID: "reproC0001",
	})
	require.NoError(t, err)
	bound.SetBackend(session.NewFakeBackend())

	// Seed the row the delivery would have written: keyed "started" for this run.
	_, _, err = task.RecordRunStatus("reproC0001", task.RunKey{ID: "sess-bound", StartedAt: fixedCreatedAt}, &fixedCreatedAt, task.RunStartedStatus)
	require.NoError(t, err)
	assert.Equal(t, task.RunStartedStatus, lastRunStatus(t, "reproC0001"))

	// The poll's run-failure stamp lands: the row is keyed to the run, so the
	// keyed failure write is not refused as stale, and the session is bound, so
	// the stamper does not bail.
	manager.stampTaskRunFailed(bound)
	assert.Equal(t, taskStatusErroredPromptUndelivered, lastRunStatus(t, "reproC0001"),
		"a bound session's failed run must be recorded on the task")

	// Negative control: an UNBOUND session (TaskID == "") — the shape the
	// auto-created target had before the fix — must NOT have its verdict
	// recorded. This documents the bail that silently dropped the verdict.
	seedCronTask(t, "reproC0002", repoPath)
	unbound, err := session.NewInstance(session.InstanceOptions{
		ID: "sess-unbound", CreatedAt: fixedCreatedAt, Title: "drift",
		Path: repoPath, Program: "claude", // TaskID intentionally empty
	})
	require.NoError(t, err)
	unbound.SetBackend(session.NewFakeBackend())
	_, _, err = task.RecordRunStatus("reproC0002", task.RunKey{ID: "sess-unbound", StartedAt: fixedCreatedAt}, &fixedCreatedAt, task.RunStartedStatus)
	require.NoError(t, err)
	manager.stampTaskRunFailed(unbound)
	assert.Equal(t, task.RunStartedStatus, lastRunStatus(t, "reproC0002"),
		"an unbound session's failed run is dropped by the stamper's early return — the bug the binding fixes")

	_ = repoID // repoID is part of newStatusTestManager's contract; kept for clarity.
}

// TestRepro_TargetedAutoCreateBindsTaskID is the end-to-end pin for the binding
// half: a targeted delivery that auto-creates its absent target must bind the
// created session to the task (TaskID on the stored row), so the failure/
// interrupted stampers and the on_complete lifecycle can operate on it. It also
// asserts the created session's identity rides the DeliverPrompt response, which
// is what lets deliverTaskPromptRun key the run-status write.
func TestRepro_TargetedAutoCreateBindsTaskID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		Title:      "captain",
		RepoPath:   repoPath,
		Program:    "claude",
		Prompt:     "do it",
		TaskRepoID: repo.ID,
		TaskOrigin: true,
		TaskID:     "reproD0001",
	}, &resp)
	require.NoError(t, err)
	assert.Equal(t, "started", resp.Status, "an absent target auto-creates and reports started")
	assert.NotEmpty(t, resp.SessionID,
		"the auto-created session's identity must ride the response so the run-status write can be keyed")
	assert.False(t, resp.SessionCreatedAt.IsZero(),
		"the auto-created session's creation time must ride the response to order the run")

	// The stored session is bound to the task — the provenance the failure/
	// interrupted stampers and the on_complete lifecycle key on. Before the fix
	// createMissingPromptTarget omitted TaskID, leaving the session unbound for
	// life and every stamper bailing by early return.
	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1, "the named repo received the auto-created session")
	assert.Equal(t, "reproD0001", stored[0].TaskID,
		"the auto-created target session must be bound to the task (TaskID set) so the stampers can function")
	assert.Equal(t, resp.SessionID, stored[0].ID,
		"the response's SessionID is the stored session's stable identity")
}

// TestRepro_WatchAutoCreateKeysRunStatus pins the Codex P1 on taskrun.go:252
// (#7880): a watch task that auto-creates its absent target_session must key its
// run-status write to the created run, not land it as an unkeyed event. Before
// the fix deliverTaskPromptOutcome discarded the run key and the watch path wrote
// through UpdateTaskStatusForTarget (unkeyed), which advanced the row's clock
// past the session's creation time and refused every later keyed failure or
// interruption stamp for that run as stale — the same invisible-failure
// behavior the cron fix addressed. The watch path now threads the RunKey through
// RecordRunStatusForTarget, so the write is keyed and a later failure stamp
// lands.
func TestRepro_WatchAutoCreateKeysRunStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	stubDeliverCreated(t, "sess-watch", fixedCreatedAt, false)

	require.NoError(t, task.AddTask(task.Task{
		ID:            "reproW0001",
		Name:          "watch",
		Prompt:        "event: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: "captain",
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now(),
	}))

	require.NoError(t, deliverWatchEventWithOptions("reproW0001", "line1", watchDeliveryOptions{}))

	got, err := task.GetTask("reproW0001")
	require.NoError(t, err)
	assert.Equal(t, "started", got.LastRunStatus,
		"the watch auto-create is recorded started")
	assert.Equal(t, "sess-watch", got.LastRunID,
		"the watch run-status write must be keyed to the created session, not unkeyed (which cleared LastRunID)")
	require.NotNil(t, got.LastRunStartedAt)
	assert.True(t, got.LastRunStartedAt.Equal(fixedCreatedAt),
		"the run's start time is the session's creation time, so later keyed writers for this run are not refused as stale")

	// The failure verdict the session lifecycle would produce once the
	// auto-created run dies — keyed to the same run — must LAND. Before the fix
	// the unkeyed write advanced the row clock past fixedCreatedAt, so
	// runOrder({sess-watch, fixedCreatedAt}, unkeyed@later) < 0 refused this.
	later := fixedCreatedAt.Add(time.Minute)
	_, applied, err := task.RecordRunStatus("reproW0001", task.RunKey{ID: "sess-watch", StartedAt: fixedCreatedAt}, &later, task.RunFailedUndeliveredStatus)
	require.NoError(t, err)
	assert.True(t, applied, "the failure verdict for the watch auto-created run must land (the row is keyed to it, so it is not refused as stale)")
	assert.Equal(t, task.RunFailedUndeliveredStatus, lastRunStatus(t, "reproW0001"))
}
