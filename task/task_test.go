package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestTasks writes a tasks file to a temp dir and overrides
// getTasksPath for the duration of the test.
func setupTestTasks(t *testing.T, tasks []Task) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, tasksFileName)

	if tasks != nil {
		data, err := json.MarshalIndent(tasks, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0644))
	}

	// Override the path resolver for the test
	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	return path
}

func TestLoadTasksEmpty(t *testing.T) {
	setupTestTasks(t, nil) // no file on disk
	tasks, err := LoadTasks()
	assert.NoError(t, err)
	assert.Empty(t, tasks)
}

func TestLoadTasks(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	input := []Task{
		{ID: "a1b2", Name: "Test Task", Prompt: "do stuff", CronExpr: "0 9 * * *", ProjectPath: "/tmp", Program: "claude", Enabled: true, CreatedAt: now},
	}
	setupTestTasks(t, input)

	tasks, err := LoadTasks()
	assert.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "a1b2", tasks[0].ID)
	assert.Equal(t, "Test Task", tasks[0].Name)
	assert.Equal(t, "do stuff", tasks[0].Prompt)
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	setupTestTasks(t, []Task{})

	s := Task{
		ID:          "abc1",
		Name:        "Nightly Build",
		Prompt:      "run build",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now().Truncate(time.Second),
	}

	err := SaveTasks([]Task{s})
	require.NoError(t, err)

	loaded, err := LoadTasks()
	assert.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, s.ID, loaded[0].ID)
	assert.Equal(t, s.Name, loaded[0].Name)
	assert.Equal(t, s.CronExpr, loaded[0].CronExpr)
}

func TestAddTask(t *testing.T) {
	setupTestTasks(t, []Task{})

	s1 := Task{ID: "s1", Name: "First", Prompt: "p1", CronExpr: "0 * * * *", ProjectPath: "/tmp", Program: "claude", Enabled: true, CreatedAt: time.Now()}
	s2 := Task{ID: "s2", Name: "Second", Prompt: "p2", CronExpr: "0 0 * * *", ProjectPath: "/tmp", Program: "claude", Enabled: true, CreatedAt: time.Now()}

	require.NoError(t, AddTask(s1))
	require.NoError(t, AddTask(s2))

	loaded, err := LoadTasks()
	assert.NoError(t, err)
	assert.Len(t, loaded, 2)
	assert.Equal(t, "s1", loaded[0].ID)
	assert.Equal(t, "s2", loaded[1].ID)
}

func TestRemoveTask(t *testing.T) {
	tasks := []Task{
		{ID: "keep", Name: "Keep", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/tmp", Program: "claude", Enabled: true},
		{ID: "remove", Name: "Remove", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/tmp", Program: "claude", Enabled: true},
	}
	setupTestTasks(t, tasks)

	err := RemoveTask("remove")
	assert.NoError(t, err)

	loaded, err := LoadTasks()
	assert.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "keep", loaded[0].ID)
}

func TestRemoveTaskNotFound(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "exists"}})

	err := RemoveTask("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetTask(t *testing.T) {
	tasks := []Task{
		{ID: "x1", Name: "First"},
		{ID: "x2", Name: "Second"},
	}
	setupTestTasks(t, tasks)

	s, err := GetTask("x2")
	assert.NoError(t, err)
	assert.Equal(t, "Second", s.Name)
}

func TestGetTaskNotFound(t *testing.T) {
	setupTestTasks(t, []Task{})

	_, err := GetTask("missing")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateTask(t *testing.T) {
	tasks := []Task{
		{ID: "u1", Name: "Old Name", Prompt: "old prompt", CronExpr: "0 * * * *", Enabled: true},
	}
	setupTestTasks(t, tasks)

	updated := Task{ID: "u1", Name: "New Name", Prompt: "new prompt", CronExpr: "0 0 * * *", Enabled: false}
	err := UpdateTask(updated)
	assert.NoError(t, err)

	s, err := GetTask("u1")
	assert.NoError(t, err)
	assert.Equal(t, "New Name", s.Name)
	assert.Equal(t, "new prompt", s.Prompt)
	assert.Equal(t, "0 0 * * *", s.CronExpr)
	assert.False(t, s.Enabled)
}

func TestUpdateTaskNotFound(t *testing.T) {
	setupTestTasks(t, []Task{})

	err := UpdateTask(Task{ID: "missing"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGenerateID(t *testing.T) {
	id1 := GenerateID()
	id2 := GenerateID()
	assert.Len(t, id1, 8) // 4 bytes = 8 hex chars
	assert.Len(t, id2, 8)
	assert.NotEqual(t, id1, id2)
}

// TestLoadTaskWithoutProgramFieldFallsBackToEmpty verifies that a task record
// persisted before the per-task program feature (i.e. without a "program"
// key) loads cleanly with Program == "". The runner / daemon path treats an
// empty Program as "use the config default", so this is the backwards-compat
// path users on stale tasks.json files depend on. Regression test for #453.
func TestLoadTaskWithoutProgramFieldFallsBackToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tasksFileName)

	legacyJSON := `[{"id":"legacy","name":"Old Task","prompt":"do it","cron_expr":"0 9 * * *","project_path":"/tmp","enabled":true,"created_at":"2025-01-01T00:00:00Z"}]`
	require.NoError(t, os.WriteFile(path, []byte(legacyJSON), 0644))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	tasks, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "legacy", tasks[0].ID)
	assert.Equal(t, "", tasks[0].Program, "legacy task without program key must load with Program=\"\" so the runner falls back to the config default")
}

// TestUpdateTaskPersistsProgram verifies that the Program field is persisted
// through UpdateTask. Regression test for #453: editing a task's program
// through the TUI must survive a SaveTasks -> LoadTasks round-trip.
func TestUpdateTaskPersistsProgram(t *testing.T) {
	tasks := []Task{
		{ID: "p1", Name: "Original", Program: "claude", Enabled: true},
	}
	setupTestTasks(t, tasks)

	updated := tasks[0]
	updated.Program = "aider"
	require.NoError(t, UpdateTask(updated))

	got, err := GetTask("p1")
	require.NoError(t, err)
	assert.Equal(t, "aider", got.Program)
}

func TestTaskNameInJSON(t *testing.T) {
	s := Task{ID: "n1", Name: "My Task", Prompt: "do things"}
	data, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"name":"My Task"`)

	// Name omitted when empty
	s2 := Task{ID: "n2", Prompt: "do things"}
	data2, err := json.Marshal(s2)
	require.NoError(t, err)
	assert.NotContains(t, string(data2), `"name"`)
}
