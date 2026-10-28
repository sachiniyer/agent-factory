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

// tasks.json FOLLOWS a symlink (#3672), unlike af's own managed files.
//
// It is the one USER-AUTHORED store in the af home — people edit it by hand — so
// a user may reasonably keep it in dotfiles the way they keep config.toml, and
// #3660's answer for the global config is the right one here: rewrite the file
// the link names and leave the link in place.
//
// Before this change the write replaced the link with a regular file, and the
// dotfiles copy silently stopped being the one af read.
func TestWriteTasksFollowsASymlinkedStore(t *testing.T) {
	dotfiles := t.TempDir()
	real := filepath.Join(dotfiles, "af-tasks.json")
	require.NoError(t, os.WriteFile(real, []byte("{\"schema_version\":1,\"tasks\":[]}\n"), 0644))

	home := t.TempDir()
	link := filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(real, link))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return link, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	require.NoError(t, AddTask(Task{
		ID:          "a1b2",
		Name:        "Linked store",
		Prompt:      "do stuff",
		CronExpr:    "0 9 * * *",
		ProjectPath: t.TempDir(),
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now().Truncate(time.Second),
	}))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the user's dotfiles arrangement survives the write")

	// The row landed in the file the link NAMES, read without going through the
	// link — otherwise a replaced link would read identically.
	raw, err := os.ReadFile(real)
	require.NoError(t, err)
	var envelope tasksEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope))
	var stored []Task
	require.NoError(t, json.Unmarshal(envelope.Tasks, &stored))
	require.Len(t, stored, 1)
	assert.Equal(t, "a1b2", stored[0].ID)

	// And af still reads it back through the link, so the two ends agree.
	loaded, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "a1b2", loaded[0].ID)

	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".tmp.",
			"no temp file is stranded in the af home when the write goes elsewhere")
	}
}

// A BROKEN link is an error naming both ends, not a silent create — the same
// answer resolveWriteTarget gives the global config (#3660). af cannot tell
// whether the user meant to create the missing target or to have a real file at
// the link's own path, so it stops and lets them decide.
func TestWriteTasksRefusesADanglingLinkedStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.json")
	home := t.TempDir()
	link := filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(missing, link))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return link, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	err := AddTask(Task{
		ID:          "c3d4",
		Name:        "Dangling",
		Prompt:      "do stuff",
		CronExpr:    "0 9 * * *",
		ProjectPath: t.TempDir(),
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now().Truncate(time.Second),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), missing, "and the target it points at")
	assert.NoFileExists(t, missing, "and creates nothing at the far end")
}

// --- Read-path dangling-link contract (#3672 review) -------------------------
//
// The three read paths in store.go — LoadTasks, loadTasksLocked, and
// ensureTasksSchemaMigrated — once treated a dangling tasks.json symlink as an
// empty store (os.Stat/ReadFile follow the link, return ENOENT, the IsNotExist
// branch swallowed it). The write path already refused the same link and named
// both ends (TestWriteTasksRefusesADanglingLinkedStore above). The config package
// closed this read/write asymmetry for config.toml and config.json with
// RefuseDanglingConfigLink (#3660 review); these tests pin the same guard on
// the task store's read paths, and the non-regressions that keep the ordinary
// first-run and resolving-link paths ordinary.

// danglingStoreLink stages a tasks.json that is a symlink to a file that does
// not exist anywhere — the dotfiles shape with a moved, renamed, or deleted
// target. It returns the link path (inside an af-home temp dir) and the missing
// target path (outside it).
func danglingStoreLink(t *testing.T) (link, missing string) {
	t.Helper()
	missing = filepath.Join(t.TempDir(), "gone.json")
	home := t.TempDir()
	link = filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(missing, link))
	return link, missing
}

// pointGetTasksPathAt overrides the store-path seam to return path, restoring
// the original on cleanup. LoadTasks and the mutations that resolve their own
// path load through getTasksPathFn, so this is how a test pins where they read.
func pointGetTasksPathAt(t *testing.T, path string) {
	t.Helper()
	orig := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = orig })
}

// TestLoadTasksRefusesDanglingSymlink pins the headline read path: a dangling
// tasks.json symlink must NOT read as an empty store. Before the guard it
// returned 0 tasks with nil error, so the daemon's scheduler wiped every
// schedule and re-armed against the empty list, the TUI showed an empty rail,
// and af doctor reported "no enabled tasks" — all silently. It must refuse,
// naming both ends of the link the way the write path does.
func TestLoadTasksRefusesDanglingSymlink(t *testing.T) {
	link, missing := danglingStoreLink(t)
	pointGetTasksPathAt(t, link)

	tasks, err := LoadTasks()
	require.Error(t, err, "a dangling tasks.json link must not read as an empty store")
	assert.Empty(t, tasks, "no tasks are returned for a broken link")
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), missing, "and the target it points at")

	// A read must not repair or replace the link; it is left for the operator.
	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the broken link is left for the user to repair")
}

// TestLoadTasksLockedRefusesDanglingSymlink pins the write path's own loader.
// loadTasksLocked is what AddTask, UpdateTaskStatus, and RemoveTask load through
// to find the row they change; a dangling link made it return an empty list, so
// every mutation then reported "task with id X not found" for a task that does
// exist in the unreachable target — and the write-side refusal was never reached.
// It must refuse with the same both-ends error instead.
func TestLoadTasksLockedRefusesDanglingSymlink(t *testing.T) {
	link, missing := danglingStoreLink(t)

	tasks, err := loadTasksLocked(link)
	require.Error(t, err, "a dangling link must not load as an empty store")
	assert.Empty(t, tasks)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)
}

// TestEnsureTasksSchemaMigratedRefusesDanglingSymlink pins the daemon-load
// schema check. A dangling link made os.Stat return ENOENT, so migration was
// skipped with nil error — the check passed a broken link as a migrated store.
// It must refuse so the daemon load fails loudly, the same way LoadTasks does.
func TestEnsureTasksSchemaMigratedRefusesDanglingSymlink(t *testing.T) {
	link, missing := danglingStoreLink(t)

	err := ensureTasksSchemaMigrated(link)
	require.Error(t, err, "a dangling link must not skip migration as if the store were absent")
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)
}

// TestMutationsRefuseDanglingSymlinkNotNotFound pins the impact most visible to
// a user: a mutation against a dangling link used to report "task with id X not
// found" for a task that still exists in the unreachable target file, because
// the mutation's own load already saw the phantom-empty store. ensureTasksSchemaMigrated
// now fails first, so the broken link — the real cause — surfaces instead of
// the misleading "not found".
func TestMutationsRefuseDanglingSymlinkNotNotFound(t *testing.T) {
	link, missing := danglingStoreLink(t)
	pointGetTasksPathAt(t, link)

	t.Run("UpdateTaskStatus", func(t *testing.T) {
		_, err := UpdateTaskStatus("abcd", nil, "ok")
		require.Error(t, err)
		assert.Contains(t, err.Error(), link, "the error names the link")
		assert.Contains(t, err.Error(), missing, "and its target")
		assert.NotContains(t, err.Error(), "not found",
			"a dangling link surfaces the broken link, not a misleading 'not found'")
	})

	t.Run("RemoveTask", func(t *testing.T) {
		err := RemoveTask("abcd", ProjectExpectation{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), link)
		assert.Contains(t, err.Error(), missing)
		assert.NotContains(t, err.Error(), "not found")
	})
}

// --- Non-regressions: the guard must not turn ordinary paths into errors. ---

// TestLoadTasksRegularMissingStillReturnsEmpty is the key non-regression for the
// headline read path. RefuseDanglingConfigLink returns nil for a simply-absent
// path (an ordinary first run), so the guard must NOT turn "no store yet" into
// an error. LoadTasks still returns an empty list with nil error.
func TestLoadTasksRegularMissingStillReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	pointGetTasksPathAt(t, filepath.Join(home, tasksFileName))

	tasks, err := LoadTasks()
	require.NoError(t, err, "a regular absent tasks.json is an ordinary first run, not a broken link")
	assert.Empty(t, tasks)
}

// TestLoadTasksLockedRegularMissingStillReturnsEmpty is the write-path loader
// non-regression: a regular absent path still returns an empty list so the
// create path (AddTask on a fresh home) proceeds.
func TestLoadTasksLockedRegularMissingStillReturnsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), tasksFileName)

	tasks, err := loadTasksLocked(missing)
	require.NoError(t, err)
	assert.Empty(t, tasks)
}

// TestEnsureTasksSchemaMigratedRegularMissingStillReturnsNil is the schema-check
// non-regression: a regular absent path still returns nil so a first run does
// not error before the store is created.
func TestEnsureTasksSchemaMigratedRegularMissingStillReturnsNil(t *testing.T) {
	missing := filepath.Join(t.TempDir(), tasksFileName)

	err := ensureTasksSchemaMigrated(missing)
	require.NoError(t, err, "a regular absent path does not error on a schema check")
}

// TestLoadTasksResolvingSymlinkStillReads is the resolving-link non-regression
// for the headline read path: a tasks.json symlink whose target exists is NOT
// refused — the store loads through the link exactly as before.
// TestWriteTasksFollowsASymlinkedStore above already exercises the write+read
// round-trip; this isolates the read guard against a false positive.
func TestLoadTasksResolvingSymlinkStillReads(t *testing.T) {
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "af-tasks.json")
	require.NoError(t, os.WriteFile(target, []byte("{\"schema_version\":1,\"tasks\":[]}\n"), 0644))
	home := t.TempDir()
	link := filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(target, link))
	pointGetTasksPathAt(t, link)

	tasks, err := LoadTasks()
	require.NoError(t, err, "a resolving symlink must still read, not be refused")
	assert.Empty(t, tasks)
}

// TestLoadTasksLockedResolvingSymlinkStillReads is the resolving-link
// non-regression for the write-path loader: a resolving symlink still loads.
func TestLoadTasksLockedResolvingSymlinkStillReads(t *testing.T) {
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "af-tasks.json")
	require.NoError(t, os.WriteFile(target, []byte("{\"schema_version\":1,\"tasks\":[]}\n"), 0644))
	home := t.TempDir()
	link := filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(target, link))

	tasks, err := loadTasksLocked(link)
	require.NoError(t, err, "a resolving symlink must still load, not be refused")
	assert.Empty(t, tasks)
}

// TestEnsureTasksSchemaMigratedResolvingSymlinkStillNil is the resolving-link
// non-regression for the schema check: a resolving symlink does not error.
func TestEnsureTasksSchemaMigratedResolvingSymlinkStillNil(t *testing.T) {
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "af-tasks.json")
	require.NoError(t, os.WriteFile(target, []byte("{\"schema_version\":1,\"tasks\":[]}\n"), 0644))
	home := t.TempDir()
	link := filepath.Join(home, tasksFileName)
	require.NoError(t, os.Symlink(target, link))

	err := ensureTasksSchemaMigrated(link)
	require.NoError(t, err, "a resolving symlink must not error on a schema check")
}
