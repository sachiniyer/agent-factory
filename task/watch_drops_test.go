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

func TestRecordWatchRateDropsForGenerationRefusesAReboundID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	require.NoError(t, AddTask(Task{
		ID: "d4357006", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
		Enabled: true, CreatedAt: created,
	}))
	loaded, err := GetTask("d4357006")
	require.NoError(t, err)
	require.NotEmpty(t, loaded.GenerationID)
	droppedAt := created.Add(time.Hour)

	// A flush from a watcher that supervised a REMOVED incarnation must not
	// stamp its count — or the dropped status — onto the namesake replacement.
	updated, applied, err := RecordWatchRateDropsForGeneration(loaded.ID, "removed-generation", 7, droppedAt)
	require.NoError(t, err, "a rebound ID is a clean refusal, not a storage fault")
	assert.False(t, applied)
	assert.Zero(t, updated)
	stored, err := GetTask(loaded.ID)
	require.NoError(t, err)
	assert.Zero(t, stored.DroppedEvents)
	assert.Empty(t, stored.LastRunStatus)

	updated, applied, err = RecordWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID, 7, droppedAt)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, 7, updated.DroppedEvents)
	assert.Equal(t, WatchRateDropStatus, updated.LastRunStatus)

	_, applied, err = RecordWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID, 5, droppedAt)
	require.NoError(t, err)
	assert.False(t, applied, "a stale absolute checkpoint writes nothing for the right generation either")

	require.NoError(t, RemoveTask(loaded.ID, ProjectExpectation{}))
	_, _, err = RecordWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID, 9, droppedAt)
	require.Error(t, err, "a flush for a fully removed task still surfaces as an error")
}
