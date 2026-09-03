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
