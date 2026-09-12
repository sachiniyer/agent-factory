package task

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordWatchRateDropsOwnsCountAndPreservesNewerDelivery(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	require.NoError(t, AddTask(Task{
		ID: "d4357002", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
		Enabled: true, CreatedAt: created, DroppedEvents: 99,
	}))

	loaded, err := GetTask("d4357002")
	require.NoError(t, err)
	assert.Zero(t, loaded.DroppedEvents, "create must discard client-supplied daemon history")

	deliveredAt := created.Add(time.Hour)
	_, err = UpdateTaskStatus(loaded.ID, &deliveredAt, "sent")
	require.NoError(t, err)
	droppedAt := deliveredAt.Add(time.Minute)
	updated, err := RecordWatchRateDrops(loaded.ID, 3, droppedAt)
	require.NoError(t, err)
	assert.Equal(t, 3, updated.DroppedEvents)
	assert.Equal(t, WatchRateDropStatus, updated.LastRunStatus)
	assert.Equal(t, deliveredAt, *updated.LastRunAt, "a discarded event is not a delivered run")

	newerDelivery := droppedAt.Add(time.Minute)
	_, err = UpdateTaskStatus(loaded.ID, &newerDelivery, "sent")
	require.NoError(t, err)
	updated, err = RecordWatchRateDrops(loaded.ID, 5, droppedAt)
	require.NoError(t, err)
	assert.Equal(t, 5, updated.DroppedEvents, "a delayed checkpoint must still persist the exact count")
	assert.Equal(t, "sent", updated.LastRunStatus,
		"a delayed checkpoint must not overwrite a newer successful delivery")

	updated, err = RecordWatchRateDrops(loaded.ID, 4, newerDelivery.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 5, updated.DroppedEvents, "a stale absolute checkpoint must never reduce the count")
	assert.Equal(t, "sent", updated.LastRunStatus)
}
