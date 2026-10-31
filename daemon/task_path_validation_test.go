package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// #5859: with ValidatePath the TARGET daemon refuses a task whose project path
// is not a git repository on ITS host, at create and at edit; without it the
// store behaves as before.
func TestControlAddTask_ValidatePathRefusesBadPathsOnTheDaemonHost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupControlRepo(t)
	notRepo := t.TempDir()
	file := filepath.Join(notRepo, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	srv := &controlServer{scheduler: newTaskScheduler()}

	withPath := func(id, path string) task.Task {
		tk := enabledCronTask(id, "")
		tk.ProjectPath = path
		return tk
	}
	for name, bad := range map[string]string{
		"nonexistent": filepath.Join(notRepo, "nope"),
		"not a git":   notRepo,
		"a file":      file,
		"relative":    "relative/repo",
	} {
		var resp AddTaskResponse
		err := srv.AddTask(AddTaskRequest{Task: withPath("badp0001", bad), ValidatePath: true}, &resp)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "project path", name)
		assert.False(t, resp.OK, name)
	}
	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Empty(t, tasks, "a refused create stores nothing")

	var ok AddTaskResponse
	require.NoError(t, srv.AddTask(AddTaskRequest{Task: withPath("goodp001", repo), ValidatePath: true}, &ok))

	// Control: the legacy behaviour (no flag) still stores an unresolved path.
	var legacy AddTaskResponse
	require.NoError(t, srv.AddTask(AddTaskRequest{Task: withPath("legcy001", filepath.Join(notRepo, "nope"))}, &legacy))

	// Edit: a patch that moves the task to a bad path is refused; to a repo is fine.
	bad := filepath.Join(notRepo, "nope")
	var up UpdateTaskResponse
	require.Error(t, srv.UpdateTask(UpdateTaskRequest{ID: "goodp001", Update: task.TaskUpdate{ProjectPath: &bad}, ValidatePath: true}, &up))
	got, err := task.GetTask("goodp001")
	require.NoError(t, err)
	assert.Equal(t, repo, got.ProjectPath, "a refused edit changes nothing")
	require.NoError(t, srv.UpdateTask(UpdateTaskRequest{ID: "goodp001", Update: task.TaskUpdate{ProjectPath: &repo}, ValidatePath: true}, &up))
}
