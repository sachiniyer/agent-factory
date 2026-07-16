package task

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
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

func TestLoadTasksMigratesLegacyArrayFileToEnvelope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tasksFileName)
	legacyJSON := []byte(`[
  {"id":"legacy","name":"Old Task","prompt":"do it","cron_expr":"0 9 * * *","project_path":"/tmp","enabled":true,"created_at":"2025-01-01T00:00:00Z","unknown":"preserved"}
]`)
	require.NoError(t, os.WriteFile(path, legacyJSON, 0644))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	tasks, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "legacy", tasks[0].ID)
	assert.Equal(t, "", tasks[0].Program, "legacy task without program key must keep loading with Program=\"\"")

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	var envelope struct {
		SchemaVersion int               `json:"schema_version"`
		Tasks         []json.RawMessage `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(onDisk, &envelope))
	assert.Equal(t, TasksSchemaVersion, envelope.SchemaVersion)
	require.Len(t, envelope.Tasks, 1)
	assert.JSONEq(t, `{"id":"legacy","name":"Old Task","prompt":"do it","cron_expr":"0 9 * * *","project_path":"/tmp","enabled":true,"created_at":"2025-01-01T00:00:00Z","unknown":"preserved"}`, string(envelope.Tasks[0]))

	backup, err := os.ReadFile(path + ".bak.schema-v0")
	require.NoError(t, err)
	assert.Equal(t, legacyJSON, backup)
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

	err := AddTask(s)
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

func TestLoadTasksForRepo(t *testing.T) {
	setupTestTasks(t, []Task{
		{ID: "a", Name: "A", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/repos/one", Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "b", Name: "B", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/repos/two", Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "c", Name: "C", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/repos/one", Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	one, err := LoadTasksForRepo("/repos/one")
	require.NoError(t, err)
	require.Len(t, one, 2)
	for _, tk := range one {
		assert.Equal(t, "/repos/one", tk.ProjectPath)
	}

	none, err := LoadTasksForRepo("/repos/absent")
	require.NoError(t, err)
	assert.Empty(t, none)
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

// ptr returns the address of v, for building the pointer fields of a TaskUpdate
// patch in tests.
func ptr[T any](v T) *T { return &v }

func TestUpdateTask(t *testing.T) {
	tasks := []Task{
		{ID: "u1", Name: "Old Name", Prompt: "old prompt", CronExpr: "0 * * * *", Enabled: true},
	}
	setupTestTasks(t, tasks)

	merged, err := UpdateTask("u1", TaskUpdate{
		Name:     ptr("New Name"),
		Prompt:   ptr("new prompt"),
		CronExpr: ptr("0 0 * * *"),
		Enabled:  ptr(false),
	})
	assert.NoError(t, err)
	// UpdateTask returns the authoritative merged record.
	assert.Equal(t, "New Name", merged.Name)
	assert.Equal(t, "0 0 * * *", merged.CronExpr)

	s, err := GetTask("u1")
	assert.NoError(t, err)
	assert.Equal(t, "New Name", s.Name)
	assert.Equal(t, "new prompt", s.Prompt)
	assert.Equal(t, "0 0 * * *", s.CronExpr)
	assert.False(t, s.Enabled)
}

// TestUpdateTaskFieldLevelDoesNotClobber is the core regression guard for #1700:
// a single-field patch (here disabling the task) must leave every field the
// patch does NOT mention exactly as-stored — including a value a concurrent
// client committed after this caller last read the record. The old full-struct
// read-modify-write reverted those fields; the field-level merge cannot.
func TestUpdateTaskFieldLevelDoesNotClobber(t *testing.T) {
	setupTestTasks(t, []Task{
		{ID: "t1", Name: "orig", Prompt: "orig prompt", CronExpr: "0 * * * *", TargetSession: "sess-a", Program: "claude", Enabled: true},
	})

	// Client B changes the cron and target session out-of-band.
	_, err := UpdateTask("t1", TaskUpdate{CronExpr: ptr("30 6 * * 1"), TargetSession: ptr("sess-b")})
	require.NoError(t, err)

	// Client A, holding a copy from BEFORE B's edit, only toggles Enabled off.
	// It ships just that one field.
	merged, err := UpdateTask("t1", TaskUpdate{Enabled: ptr(false)})
	require.NoError(t, err)
	assert.False(t, merged.Enabled)

	got, err := GetTask("t1")
	require.NoError(t, err)
	assert.False(t, got.Enabled, "A's toggle must apply")
	// B's concurrent edits survive — A's toggle carried no cron/target field.
	assert.Equal(t, "30 6 * * 1", got.CronExpr, "B's concurrent cron edit must NOT be clobbered by A's toggle")
	assert.Equal(t, "sess-b", got.TargetSession, "B's concurrent target-session edit must survive")
	// Fields neither client touched are untouched.
	assert.Equal(t, "orig prompt", got.Prompt)
	assert.Equal(t, "orig", got.Name)
}

// TestDiffTaskAndUpdatePersistProjectPath is the regression guard for #1836.
// The TUI edit form lets a user retarget a task at another repo, and its save
// path ships a DiffTask patch. While ProjectPath was missing from TaskUpdate the
// diff came back empty, so the edit was dropped with no error and the old path
// reappeared on reload — data loss the user had no way to see fail.
func TestDiffTaskAndUpdatePersistProjectPath(t *testing.T) {
	setupTestTasks(t, []Task{
		{ID: "p1", Name: "orig", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: "/repos/old", Program: "claude", Enabled: true},
	})

	loaded, err := GetTask("p1")
	require.NoError(t, err)
	old := *loaded
	cur := old
	cur.ProjectPath = "/repos/new"

	// A ProjectPath-only edit must diff to a real patch; an empty one is the bug.
	patch := DiffTask(old, cur)
	require.False(t, patch.IsEmpty(), "a ProjectPath-only edit must produce a non-empty patch")
	require.NotNil(t, patch.ProjectPath, "the patch must carry the retargeted path")
	assert.Equal(t, "/repos/new", *patch.ProjectPath)

	merged, err := UpdateTask("p1", patch)
	require.NoError(t, err)
	assert.Equal(t, "/repos/new", merged.ProjectPath)

	// What the user sees after saving and reloading.
	got, err := GetTask("p1")
	require.NoError(t, err)
	assert.Equal(t, "/repos/new", got.ProjectPath, "the retargeted path must survive a reload")
	// Fields the patch never mentioned stay as-stored.
	assert.Equal(t, "orig", got.Name)
	assert.Equal(t, "0 9 * * *", got.CronExpr)
}

// TestTaskUpdateGobRoundTripPreservesZeroPointers guards the net/rpc control
// socket (the CLI transport): gob elides a struct field at its zero value and
// follows a *bool→false / *string→"" down to that zero and drops it, decoding the
// pointer back as nil. TaskUpdate's JSON-backed gob codec must defeat that, or
// `af tasks update --enabled false` and the trigger-clearing "" patches would
// silently become no-ops over the socket (#1700).
func TestTaskUpdateGobRoundTripPreservesZeroPointers(t *testing.T) {
	in := TaskUpdate{Enabled: ptr(false), WatchCmd: ptr(""), CronExpr: ptr("0 9 * * *")}
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(in))
	var out TaskUpdate
	require.NoError(t, gob.NewDecoder(&buf).Decode(&out))

	require.NotNil(t, out.Enabled, "a disable patch (&false) must survive gob, not decode to nil")
	assert.False(t, *out.Enabled)
	require.NotNil(t, out.WatchCmd, "a trigger-clearing (&\"\") patch must survive gob, not decode to nil")
	assert.Equal(t, "", *out.WatchCmd)
	require.NotNil(t, out.CronExpr)
	assert.Equal(t, "0 9 * * *", *out.CronExpr)
	// An unset field stays nil (absent), never a spurious zero.
	assert.Nil(t, out.Name)
	assert.Nil(t, out.Prompt)
}

func TestUpdateTaskNotFound(t *testing.T) {
	setupTestTasks(t, []Task{})

	_, err := UpdateTask("missing", TaskUpdate{Name: ptr("x")})
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

	// A user edit patches only user-editable fields; the scheduler-owned status
	// fields and immutable CreatedAt are not part of TaskUpdate, so they can
	// never regress to a stale value — preservation is inherent to the merge.
	_, err := UpdateTask("u1", TaskUpdate{
		Name:     ptr("New Name"),
		Prompt:   ptr("new prompt"),
		CronExpr: ptr("0 0 * * *"),
		Enabled:  ptr(false),
	})
	require.NoError(t, err)

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
	id1, err := GenerateID()
	require.NoError(t, err)
	id2, err := GenerateID()
	require.NoError(t, err)
	assert.Len(t, id1, 8) // 4 bytes = 8 hex chars
	assert.Len(t, id2, 8)
	assert.NotEqual(t, id1, id2)
	assert.NotEqual(t, "00000000", id1, "a successful generation must never produce the all-zero ID")
}

// failingReader is an io.Reader that always fails, used to simulate an
// unavailable system entropy source.
type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

// TestGenerateIDEntropyFailure is the regression test for #897: when the
// entropy source fails, GenerateID must return an error rather than silently
// emit the predictable, collision-prone "00000000" ID that breaks first-match
// get/remove/update semantics.
func TestGenerateIDEntropyFailure(t *testing.T) {
	orig := randReader
	randReader = failingReader{}
	t.Cleanup(func() { randReader = orig })

	id, err := GenerateID()
	require.Error(t, err, "GenerateID must surface the entropy failure")
	assert.Empty(t, id, "no ID may be returned when entropy is unavailable")
	assert.NotEqual(t, "00000000", id, "GenerateID must never emit the all-zero ID")
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
		{ID: "p1", Name: "Original", Prompt: "do the thing", Program: "claude", CronExpr: "0 3 * * *", Enabled: true},
	}
	setupTestTasks(t, tasks)

	_, err := UpdateTask("p1", TaskUpdate{Program: ptr("amp")})
	require.NoError(t, err)

	got, err := GetTask("p1")
	require.NoError(t, err)
	assert.Equal(t, "amp", got.Program)
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
	genID, err := GenerateID()
	require.NoError(t, err)
	cases := []string{
		genID, // 8 hex chars from production helper
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

	_, err := UpdateTask("../etc/passwd", TaskUpdate{Name: ptr("x")})
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

// TestUpdateTaskStatus_NilLastRunAtPreservesTimestamp is the regression guard
// for #1215: a status-only update (lastRunAt == nil) must write LastRunStatus
// without touching the on-disk LastRunAt. persistWatcherStatus relies on this
// so a supervision-status write can't revert a newer event-delivery timestamp
// committed by a concurrent deliverWatchEvent.
func TestUpdateTaskStatus_NilLastRunAtPreservesTimestamp(t *testing.T) {
	existing := time.Now().Truncate(time.Second)
	stored := []Task{
		{ID: "w1", Name: "Watcher", Prompt: "p", WatchCmd: "tail -f x", ProjectPath: "/tmp", Enabled: true, LastRunAt: &existing, LastRunStatus: "completed"},
	}
	setupTestTasks(t, stored)

	// A newer event delivery lands first (this is the value we must not lose).
	newer := existing.Add(90 * time.Second)
	require.NoError(t, UpdateTaskStatus("w1", &newer, "completed"))

	// A supervision-status write races in with a nil timestamp.
	require.NoError(t, UpdateTaskStatus("w1", nil, "stopped"))

	got, err := GetTask("w1")
	require.NoError(t, err)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(newer),
		"nil lastRunAt must preserve the newer on-disk timestamp, not revert it")
	assert.Equal(t, "stopped", got.LastRunStatus, "LastRunStatus must still update")
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
		{ID: "edit1", Name: "Editable", Prompt: "do the thing", Program: "claude", CronExpr: "0 3 * * *", Enabled: true},
	}
	setupTestTasks(t, stored)

	_, err := UpdateTask("edit1", TaskUpdate{Program: ptr("/home/foo/bin/claude")})
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

// TestValidateTrigger pins the #782 trigger contract: both triggers set is
// always invalid, an enabled task needs exactly one, and a disabled task with
// neither is tolerated as a draft. It also pins the #1000 rule: an enabled cron
// task must carry a non-empty prompt, while watch tasks and disabled drafts may
// have an empty prompt.
func TestValidateTrigger(t *testing.T) {
	cases := []struct {
		name    string
		cron    string
		watch   string
		prompt  string
		enabled bool
		wantErr bool
	}{
		{"enabled cron only", "0 3 * * *", "", "do the thing", true, false},
		{"enabled watch only", "", "tail -f log", "", true, false},
		{"enabled both", "0 3 * * *", "tail -f log", "p", true, true},
		{"enabled neither", "", "", "p", true, true},
		{"enabled whitespace counts as unset", "   ", "  ", "p", true, true},
		{"disabled cron only", "0 3 * * *", "", "", false, false},
		{"disabled watch only", "", "tail -f log", "", false, false},
		{"disabled both", "0 3 * * *", "tail -f log", "p", false, true},
		{"disabled neither (draft)", "", "", "", false, false},
		// #1000: enabling a cron task with an empty/whitespace prompt is a
		// silent no-op at run time, so the model layer rejects it.
		{"enabled cron empty prompt", "0 3 * * *", "", "", true, true},
		{"enabled cron whitespace prompt", "0 3 * * *", "", "   ", true, true},
		// Watch tasks legitimately default an empty prompt to the emitted line.
		{"enabled watch empty prompt ok", "", "tail -f log", "", true, false},
		// Disabled cron drafts with an empty prompt are tolerated.
		{"disabled cron empty prompt (draft)", "0 3 * * *", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tsk := Task{ID: "aaaa0001", CronExpr: tc.cron, WatchCmd: tc.watch, Prompt: tc.prompt, Enabled: tc.enabled}
			err := tsk.ValidateTrigger()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestAddTask_EnforcesTriggerContract verifies the store-level chokepoint:
// AddTask and UpdateTask refuse tasks that violate the trigger rule, whatever
// surface produced them.
func TestAddTask_EnforcesTriggerContract(t *testing.T) {
	setupTestTasks(t, []Task{})

	bad := Task{ID: "bbbb0001", Prompt: "p", CronExpr: "0 3 * * *", WatchCmd: "tail -f x", Enabled: true, CreatedAt: time.Now()}
	require.Error(t, AddTask(bad))

	noTrigger := Task{ID: "bbbb0002", Prompt: "p", Enabled: true, CreatedAt: time.Now()}
	require.Error(t, AddTask(noTrigger))

	watch := Task{ID: "bbbb0003", WatchCmd: "tail -f x", Enabled: true, CreatedAt: time.Now()}
	require.NoError(t, AddTask(watch))

	// Patching the watch task to ALSO carry a cron (without clearing watch) must
	// be refused — the merged record would set both triggers.
	_, err := UpdateTask("bbbb0003", TaskUpdate{CronExpr: ptr("0 3 * * *")})
	require.Error(t, err)
}

// TestWatchFieldsJSONRoundTrip pins the wire contract: the new fields are
// omitted when empty (backward-compatible JSON for existing cron tasks) and
// round-trip when set.
func TestWatchFieldsJSONRoundTrip(t *testing.T) {
	cronTask := Task{ID: "cccc0001", Prompt: "p", CronExpr: "0 3 * * *", Enabled: true}
	data, err := json.Marshal(cronTask)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "watch_cmd")
	assert.NotContains(t, string(data), "target_session")

	watchTask := Task{ID: "cccc0002", WatchCmd: "tail -f x", TargetSession: "captain", Enabled: true}
	data, err = json.Marshal(watchTask)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "cron_expr")

	var back Task
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, "tail -f x", back.WatchCmd)
	assert.Equal(t, "captain", back.TargetSession)
}

// TestRenderWatchPrompt pins the {{line}} templating contract for watch
// events: empty prompt defaults to the raw line; otherwise every {{line}}
// occurrence is substituted.
func TestRenderWatchPrompt(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		line   string
		want   string
	}{
		{"empty prompt defaults to line", "", "new issue #9", "new issue #9"},
		{"whitespace prompt defaults to line", "   ", "new issue #9", "new issue #9"},
		{"substitutes line", "Triage: {{line}}", "new issue #9", "Triage: new issue #9"},
		{"substitutes all occurrences", "{{line}} and {{line}}", "x", "x and x"},
		{"no placeholder leaves prompt as-is", "fixed prompt", "ignored", "fixed prompt"},
		{"empty line with placeholder", "Triage: {{line}}", "", "Triage: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RenderWatchPrompt(tc.prompt, tc.line))
		})
	}
}

// TestValidateTriggerMaxConcurrentRuns covers the watch-task concurrency cap
// (#1892). The cap is only enforceable where sessions can actually pile up — a
// watch task that creates one per event — so it is rejected everywhere else
// rather than silently ignored: a stored setting that reads as enforced but does
// nothing is worse than an error at the point of configuration.
func TestValidateTriggerMaxConcurrentRuns(t *testing.T) {
	cases := []struct {
		name    string
		task    Task
		wantErr string
	}{
		{
			name: "watch task with a cap",
			task: Task{ID: "t1", WatchCmd: "tail -f log", Enabled: true, MaxConcurrentRuns: 3},
		},
		{
			name: "unset cap is unlimited and always valid",
			task: Task{ID: "t2", WatchCmd: "tail -f log", Enabled: true},
		},
		{
			name: "a cap on a cron task is refused",
			task: Task{ID: "t3", CronExpr: "0 3 * * *", Prompt: "p", Enabled: true, MaxConcurrentRuns: 3},
			// Overlapping cron fires already coalesce on RunTask's lock.
			wantErr: "not a watch task",
		},
		{
			name:    "a cap with a target session is refused",
			task:    Task{ID: "t4", WatchCmd: "tail -f log", TargetSession: "shared", Enabled: true, MaxConcurrentRuns: 3},
			wantErr: "target_session",
		},
		{
			name:    "a negative cap is refused",
			task:    Task{ID: "t5", WatchCmd: "tail -f log", Enabled: true, MaxConcurrentRuns: -1},
			wantErr: "negative max_concurrent_runs",
		},
		{
			name: "a disabled watch draft may still carry a cap",
			task: Task{ID: "t6", WatchCmd: "tail -f log", MaxConcurrentRuns: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.task.ValidateTrigger()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestTaskUpdateMaxConcurrentRunsRoundTrip pins the patch semantics for the cap.
// Zero is a meaningful value ("revert to unlimited"), not "unchanged", so the
// field is a pointer — and it has to survive the CLI's gob control socket, which
// elides a pointer to a zero value. TaskUpdate's JSON codec is what saves it, and
// this is the test that fails if that codec is ever dropped.
func TestTaskUpdateMaxConcurrentRunsRoundTrip(t *testing.T) {
	zero := 0
	three := 3

	// A patch setting the cap to 0 must survive gob as a NON-nil pointer, or
	// `af tasks update --max-concurrent-runs 0` would silently no-op.
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(TaskUpdate{MaxConcurrentRuns: &zero}))
	var back TaskUpdate
	require.NoError(t, gob.NewDecoder(&buf).Decode(&back))
	require.NotNil(t, back.MaxConcurrentRuns, "a *int at 0 must not decode back as nil (gob elides zero values)")
	assert.Equal(t, 0, *back.MaxConcurrentRuns)

	// apply: a nil field leaves the stored cap alone, a non-nil one overwrites it.
	base := Task{ID: "t1", WatchCmd: "tail -f log", MaxConcurrentRuns: 5}
	assert.Equal(t, 5, TaskUpdate{}.apply(base).MaxConcurrentRuns, "an absent patch field must not change the cap")
	assert.Equal(t, 3, TaskUpdate{MaxConcurrentRuns: &three}.apply(base).MaxConcurrentRuns)
	assert.Equal(t, 0, TaskUpdate{MaxConcurrentRuns: &zero}.apply(base).MaxConcurrentRuns, "0 must revert the cap to unlimited")

	// IsEmpty must see the field, or a cap-only patch would be treated as a no-op.
	assert.False(t, TaskUpdate{MaxConcurrentRuns: &zero}.IsEmpty())

	// DiffTask must emit it when the user edits it in the TUI.
	diff := DiffTask(base, Task{ID: "t1", WatchCmd: "tail -f log", MaxConcurrentRuns: 3})
	require.NotNil(t, diff.MaxConcurrentRuns)
	assert.Equal(t, 3, *diff.MaxConcurrentRuns)
}

// TestUpdateTaskClearsStaleCapForNonCLIWriters is the regression for the
// stale-cap rejection (#1892): the cap-clearing rule has to live in the SHARED
// merge, because every non-CLI writer (TUI task pane, API, daemon) sends a
// partial patch built by DiffTask — which carries only the fields that changed,
// and never max_concurrent_runs, since those surfaces have no cap control. With
// the rule CLI-side only, those clients could not switch a capped watch task to
// cron or give it a target session at all: the merged record kept the positive
// cap and ValidateTrigger rejected the whole save.
func TestUpdateTaskClearsStaleCapForNonCLIWriters(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Exactly the patch a TUI/API edit produces: trigger fields only.
	seed := Task{ID: "cap1892e", Name: "capped", WatchCmd: "tail -f x", MaxConcurrentRuns: 3, Enabled: true}
	require.NoError(t, AddTask(seed))

	edited := seed
	edited.CronExpr = "0 9 * * *"
	edited.WatchCmd = ""
	edited.Prompt = "run it"
	patch := DiffTask(seed, edited)
	require.Nil(t, patch.MaxConcurrentRuns, "a TUI/API edit never patches the cap; that is the point of this test")

	merged, err := UpdateTask("cap1892e", patch)
	require.NoError(t, err, "a non-CLI watch→cron edit must succeed, not be rejected for a cap the client never touched")
	assert.Equal(t, "0 9 * * *", merged.CronExpr)
	assert.Equal(t, 0, merged.MaxConcurrentRuns, "the now-inapplicable cap must be cleared by the shared merge")
}

// TestUpdateTaskClearsStaleCapOnDeliveryModeChange: the same rule for the other
// direction a cap becomes invalid — a target session is added, so deliveries
// serialize into one session and there is nothing to bound.
func TestUpdateTaskClearsStaleCapOnDeliveryModeChange(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := Task{ID: "cap1892f", Name: "capped", WatchCmd: "tail -f x", MaxConcurrentRuns: 3, Enabled: true}
	require.NoError(t, AddTask(seed))

	target := "shared"
	merged, err := UpdateTask("cap1892f", TaskUpdate{TargetSession: &target})
	require.NoError(t, err, "adding a target session must not be rejected for a cap the client never touched")
	assert.Equal(t, "shared", merged.TargetSession)
	assert.Equal(t, 0, merged.MaxConcurrentRuns)
}

// TestUpdateTaskKeepsExplicitCapContradiction: an explicitly-patched cap is NOT
// auto-cleared. Silently dropping a value the caller actually sent would hide
// their mistake; the contradiction surfaces as a validation error instead.
func TestUpdateTaskKeepsExplicitCapContradiction(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, AddTask(Task{ID: "cap1892g", Name: "w", WatchCmd: "tail -f x", Enabled: true}))

	cron := "0 9 * * *"
	empty := ""
	prompt := "p"
	three := 3
	_, err := UpdateTask("cap1892g", TaskUpdate{
		CronExpr: &cron, WatchCmd: &empty, Prompt: &prompt, MaxConcurrentRuns: &three,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a watch task")
}

// TestUpdateTaskKeepsCapWhenStillApplicable: the clearing rule must not be
// overzealous — a watch task that stays a watch task keeps its cap through an
// unrelated edit, and reverting a target session to per-run is compatible with
// one.
func TestUpdateTaskKeepsCapWhenStillApplicable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, AddTask(Task{ID: "cap1892h", Name: "w", WatchCmd: "tail -f x", MaxConcurrentRuns: 3, Enabled: true}))

	prompt := "new prompt"
	merged, err := UpdateTask("cap1892h", TaskUpdate{Prompt: &prompt})
	require.NoError(t, err)
	assert.Equal(t, 3, merged.MaxConcurrentRuns, "an unrelated edit must not drop the cap")

	empty := ""
	merged, err = UpdateTask("cap1892h", TaskUpdate{TargetSession: &empty})
	require.NoError(t, err)
	assert.Equal(t, 3, merged.MaxConcurrentRuns, "reverting to a session per run keeps the cap applicable")
}

// TestTargetSessionNormalizedOnWrite is the regression for the whitespace bypass
// (#1892): validation asked TrimSpace(TargetSession) == "" while deliverTaskPrompt
// asks the raw TargetSession == "", so a whitespace-only target validated as
// "creates a session per event" — storing a cap — and then took the
// target-session path at delivery, silently ignoring that cap. Normalizing on the
// write path collapses both questions onto one value.
func TestTargetSessionNormalizedOnWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, AddTask(Task{
		ID: "cap1892i", Name: "w", WatchCmd: "tail -f x",
		TargetSession: "   ", MaxConcurrentRuns: 3, Enabled: true,
	}))
	got, err := GetTask("cap1892i")
	require.NoError(t, err)
	assert.Equal(t, "", got.TargetSession, "a whitespace-only target session must normalize to empty on write")
	assert.Equal(t, 3, got.MaxConcurrentRuns, "with no real target session the cap is applicable and enforced")

	// And through the update path.
	ws := "  \t "
	merged, err := UpdateTask("cap1892i", TaskUpdate{TargetSession: &ws})
	require.NoError(t, err)
	assert.Equal(t, "", merged.TargetSession)
	assert.Equal(t, 3, merged.MaxConcurrentRuns, "normalizing to empty keeps the cap applicable rather than silently voiding it")
}
