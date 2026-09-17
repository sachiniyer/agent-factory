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
	setRunStatus(t, loaded.ID, &deliveredAt, "sent")
	droppedAt := deliveredAt.Add(time.Minute)
	updated, err := RecordWatchRateDrops(loaded.ID, 3, droppedAt)
	require.NoError(t, err)
	assert.Equal(t, 3, updated.DroppedEvents)
	assert.Equal(t, WatchRateDropStatus, updated.LastRunStatus)
	assert.Equal(t, deliveredAt, *updated.LastRunAt, "a discarded event is not a delivered run")

	newerDelivery := droppedAt.Add(time.Minute)
	setRunStatus(t, loaded.ID, &newerDelivery, "sent")
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

func TestResetWatchRateDropsForGenerationClearsReboundEvidence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	require.NoError(t, AddTask(Task{
		ID: "d4357009", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
		Enabled: true, CreatedAt: created,
	}))
	loaded, err := GetTask("d4357009")
	require.NoError(t, err)
	droppedAt := created.Add(time.Hour)

	// Stage the stale state a rebound can find on the row: a count and the
	// status a drop record leaves behind.
	_, applied, err := RecordWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID, 12, droppedAt)
	require.NoError(t, err)
	require.True(t, applied)

	// A reset naming a different incarnation is the same clean refusal the
	// checkpoint path gives — the row moved on and is not this caller's to
	// edit.
	updated, applied, err := ResetWatchRateDropsForGeneration(loaded.ID, "other-generation")
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Zero(t, updated)
	stored, err := GetTask(loaded.ID)
	require.NoError(t, err)
	assert.Equal(t, 12, stored.DroppedEvents)
	assert.Equal(t, WatchRateDropStatus, stored.LastRunStatus)

	updated, applied, err = ResetWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Zero(t, updated.DroppedEvents)
	assert.Empty(t, updated.LastRunStatus)
	stored, err = GetTask(loaded.ID)
	require.NoError(t, err)
	assert.Zero(t, stored.DroppedEvents)
	assert.Empty(t, stored.LastRunStatus,
		"the drop status is the predecessor's evidence and clears with its count")

	// An already-clean row is a no-op, and a foreign status survives: the
	// reset owns drop evidence, nothing else.
	_, applied, err = ResetWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID)
	require.NoError(t, err)
	assert.False(t, applied)

	deliveredAt := droppedAt.Add(time.Minute)
	setRunStatus(t, loaded.ID, &deliveredAt, "sent")
	_, applied, err = ResetWatchRateDropsForGeneration(loaded.ID, loaded.GenerationID)
	require.NoError(t, err)
	assert.False(t, applied)
	stored, err = GetTask(loaded.ID)
	require.NoError(t, err)
	assert.Equal(t, "sent", stored.LastRunStatus)

	_, _, err = ResetWatchRateDropsForGeneration("d43570ff", "any")
	require.Error(t, err, "a reset for a missing task still surfaces as an error")
}
