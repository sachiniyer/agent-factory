package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestUpdateTaskPreservesSchedulerOwnedFields is the regression test for #731.
// UpdateTask is a user-edit path; its caller may hold a stale copy of the
// scheduler-owned status fields (read before a concurrent scheduler run or
// manual trigger bumped them via UpdateTaskStatus). UpdateTask must NOT
// clobber the fresher on-disk LastRunAt/LastRunStatus (nor the immutable
// CreatedAt) when applying a user edit.
func TestUpdateTaskPreservesSchedulerOwnedFields(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	tasks := []Task{
		{ID: "u1", Name: "Old Name", Prompt: "old prompt", CronExpr: "0 * * * *", Enabled: true, CreatedAt: created, LastRunAt: &t1, LastRunStatus: "started"},
	}
	setupTestTasks(t, tasks)

	// Scheduler bumps the status to a fresher value via the canonical path.
	t2 := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	require.NoError(t, UpdateTaskStatus("u1", &t2, "completed"))

	// User edit carries a STALE copy: LastRunAt=t1, LastRunStatus="started",
	// plus an attempt to mutate CreatedAt. None of these should win.
	stale := Task{ID: "u1", Name: "New Name", Prompt: "new prompt", CronExpr: "0 0 * * *", Enabled: false, CreatedAt: time.Time{}, LastRunAt: &t1, LastRunStatus: "started"}
	require.NoError(t, UpdateTask(stale))

	s, err := GetTask("u1")
	require.NoError(t, err)
	// User-editable fields applied.
	assert.Equal(t, "New Name", s.Name)
	assert.Equal(t, "new prompt", s.Prompt)
	assert.Equal(t, "0 0 * * *", s.CronExpr)
	assert.False(t, s.Enabled)
	// Scheduler-owned fields preserved at the fresher disk values.
	require.NotNil(t, s.LastRunAt)
	assert.True(t, s.LastRunAt.Equal(t2), "LastRunAt must retain the fresher scheduler value t2, not regress to stale t1")
	assert.Equal(t, "completed", s.LastRunStatus, "LastRunStatus must retain the fresher scheduler value, not regress to stale")
	assert.True(t, s.CreatedAt.Equal(created), "CreatedAt is immutable and must be preserved from disk")
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

// TestValidateTaskID_PathTraversalRejected covers the CLI path-traversal
// exploit class from #575. Every input that could break out of the locks
// directory (or any other path segment built from the task ID) must be
// rejected. Mirrors config.TestValidateRepoID_PathTraversalRejected.
func TestValidateTaskID_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"dotdot-slash", "../"},
		{"deep-traversal", "../../../etc/passwd"},
		{"embedded-traversal", "foo/../bar"},
		{"trailing-traversal", "abc/.."},
		{"absolute-path", "/etc/passwd"},
		{"windows-absolute", "C:\\windows\\system32"},
		{"forward-slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"null-byte", "foo\x00bar"},
		{"newline", "foo\nbar"},
		{"tilde", "~/secrets"},
		{"hidden", ".hidden"},
		{"glob", "foo*"},
		{"space", "foo bar"},
		{"issue-575-payload", "foo/../../rogue/pwned"},
		{"too-long", strings.Repeat("a", maxTaskIDLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTaskID(tc.input)
			assert.Error(t, err, "expected %q to be rejected", tc.input)
		})
	}
}

// TestValidateTaskID_LegitimateAccepted ensures real-world task IDs from
// GenerateID and the fixture IDs already used elsewhere in the test suite
// continue to validate.
func TestValidateTaskID_LegitimateAccepted(t *testing.T) {
	cases := []string{
		GenerateID(), // 8 hex chars from production helper
		"a1b2",
		"abc1",
		"x1",
		"u1",
		"underscore_id",
		"dashed-id",
		"A",
		strings.Repeat("a", maxTaskIDLength),
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			assert.NoError(t, ValidateTaskID(id))
		})
	}
}

// TestGetTask_RejectsInvalidID verifies that GetTask rejects path-traversal
// IDs without touching the on-disk tasks file. This is the defense-in-depth
// layer that protects callers that bypass the CLI validation.
func TestGetTask_RejectsInvalidID(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "real"}})

	_, err := GetTask("../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid task id")
}

// TestRemoveTask_RejectsInvalidID mirrors TestGetTask_RejectsInvalidID for
// RemoveTask, which is reachable from the API and TUI.
func TestRemoveTask_RejectsInvalidID(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "real"}})

	err := RemoveTask("../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid task id")
}

// TestUpdateTask_RejectsInvalidID mirrors TestGetTask_RejectsInvalidID for
// UpdateTask.
func TestUpdateTask_RejectsInvalidID(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "real"}})

	err := UpdateTask(Task{ID: "../etc/passwd"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid task id")
}

// TestUpdateTaskStatus_BypassesProgramValidation is the regression guard for
// #664: scheduler/TUI status bumps must succeed on tasks whose stored Program
// value would now fail enum validation (e.g. a legacy absolute path created
// before #658 introduced the enum check).
func TestUpdateTaskStatus_BypassesProgramValidation(t *testing.T) {
	stored := []Task{
		{ID: "legacy1", Name: "Pre-#658", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/tmp", Program: "/home/foo/bin/claude", Enabled: true},
	}
	setupTestTasks(t, stored)

	now := time.Now().Truncate(time.Second)
	require.NoError(t, UpdateTaskStatus("legacy1", &now, "started"))

	got, err := GetTask("legacy1")
	require.NoError(t, err)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(now))
	assert.Equal(t, "started", got.LastRunStatus)
	assert.Equal(t, "/home/foo/bin/claude", got.Program, "Program must not be touched by status updates")
}

// TestUpdateTaskStatus_NotFound verifies the not-found error path that the
// runner / TUI rely on to log a meaningful failure when a task is deleted
// mid-run.
func TestUpdateTaskStatus_NotFound(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "exists"}})

	now := time.Now()
	err := UpdateTaskStatus("missing", &now, "started")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestUpdateTask_RejectsBadProgram is the regression guard that #664's fix
// does NOT loosen validation on the user-edit path: UpdateTask must still
// reject a non-enum Program so the TUI/CLI editor flows fail fast.
func TestUpdateTask_RejectsBadProgram(t *testing.T) {
	stored := []Task{
		{ID: "edit1", Name: "Editable", Program: "claude", Enabled: true},
	}
	setupTestTasks(t, stored)

	bad := stored[0]
	bad.Program = "/home/foo/bin/claude"
	err := UpdateTask(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task program")
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
