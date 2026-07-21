package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/require"
)

func TestControlRestartTaskStartsEnabledWatchTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	tsk := watchTask("feed0002", "sleep 60", t.TempDir())
	require.NoError(t, task.AddTask(tsk))

	watchers, _ := newTestSupervisor(t, task.LoadTasks)
	server := &controlServer{scheduler: newTaskScheduler(), watchers: watchers}
	var resp RestartTaskResponse
	require.NoError(t, server.RestartTask(RestartTaskRequest{ID: tsk.ID}, &resp))
	require.True(t, resp.OK)
	require.Equal(t, []string{tsk.ID}, watchers.watchingTaskIDs())
}

func TestControlRestartTaskRefusesWrongScopeDisabledAndCronTasks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	disabled := watchTask("feed0003", "sleep 60", "/repos/alpha")
	disabled.Enabled = false
	cron := task.Task{
		ID: "feed0004", Name: "cron", Prompt: "run", CronExpr: "0 3 * * *",
		ProjectPath: "/repos/alpha", Enabled: true, CreatedAt: time.Now(),
	}
	enabledWatch := watchTask("feed0005", "sleep 60", "/repos/beta")
	for _, tsk := range []task.Task{disabled, cron, enabledWatch} {
		require.NoError(t, task.AddTask(tsk))
	}

	watchers, _ := newTestSupervisor(t, task.LoadTasks)
	server := &controlServer{scheduler: newTaskScheduler(), watchers: watchers}
	for _, tc := range []struct {
		name   string
		req    RestartTaskRequest
		needle string
	}{
		{name: "disabled", req: RestartTaskRequest{ID: disabled.ID}, needle: "disabled"},
		{name: "cron", req: RestartTaskRequest{ID: cron.ID}, needle: "not a watch task"},
		{
			name: "rebound",
			req: RestartTaskRequest{
				ID:     enabledWatch.ID,
				Expect: task.ProjectExpectation{Enforce: true, ProjectPath: "/repos/alpha"},
			},
			needle: "re-bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := server.RestartTask(tc.req, &RestartTaskResponse{})
			require.ErrorContains(t, err, tc.needle)
			require.Empty(t, watchers.watchingTaskIDs())
		})
	}
}
