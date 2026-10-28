package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store refuses an add that names an existing task's ID, atomically under the
// tasks-file lock, and leaves the existing row byte-for-byte as it was.
func TestAddTaskRefusesADuplicateID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	first := Task{ID: "dup-id", Prompt: "first", CronExpr: "0 * * * *", ProjectPath: t.TempDir()}
	require.NoError(t, AddTask(first))

	err := AddTask(Task{ID: "dup-id", Prompt: "second", WatchCmd: "./w.sh", ProjectPath: t.TempDir()})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskIDExists)

	got, err := GetTask("dup-id")
	require.NoError(t, err)
	assert.Equal(t, "first", got.Prompt)
	assert.Empty(t, got.WatchCmd)
	all, err := LoadTasks()
	require.NoError(t, err)
	assert.Len(t, all, 1)
}
