package task

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTerminalWatcherStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"stopped", true},
		{"errored: exit status 1: boom", true},
		{"errored: not armed — target session is archived", true},
		{"", false},
		{"sent", false},
		{"deferred: target attached", false},
		{WatchRateDropStatus, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.status, func(t *testing.T) {
			assert.Equal(t, c.want, isTerminalWatcherStatus(c.status))
		})
	}
}

// TestRecordWatchRateDropsPreservesNewerTerminalStatus is the durable-write
// counterpart of the live-overlay latch (applyLiveDropState): a stop-time drop
// flush passes lastDroppedAt (the time of the most recent drop) which can post-
// date the last successful delivery, so the LastRunAt guard alone would let the
// stale "dropped" outcome clobber the newer "stopped"/"errored:" terminal status
// the watcher persisted (terminal writes deliberately do not advance LastRunAt).
// The status must survive while the cumulative drop count still advances.
func TestRecordWatchRateDropsPreservesNewerTerminalStatus(t *testing.T) {
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	deliveredAt := created.Add(time.Hour)
	droppedAt := deliveredAt.Add(time.Minute) // post-dates the delivery → LastRunAt guard passes
	for _, terminal := range []string{
		"stopped",
		"errored: exit status 1: boom",
		"errored: not armed — target session is archived",
	} {
		terminal := terminal
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			require.NoError(t, AddTask(Task{
				ID: "d4357100", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
				Enabled: true, CreatedAt: created,
			}))
			// A prior delivery, then a terminal outcome recorded without advancing
			// LastRunAt (the production persistWatcherStatus path passes nil).
			_, err := UpdateTaskStatus("d4357100", &deliveredAt, "sent")
			require.NoError(t, err)
			_, err = UpdateTaskStatus("d4357100", nil, terminal)
			require.NoError(t, err)

			updated, err := RecordWatchRateDrops("d4357100", 3, droppedAt)
			require.NoError(t, err)
			assert.Equal(t, 3, updated.DroppedEvents, "cumulative count must still advance")
			assert.Equal(t, terminal, updated.LastRunStatus,
				"a stale drop flush must not overwrite the newer terminal watcher status")

			reloaded, err := GetTask("d4357100")
			require.NoError(t, err)
			assert.Equal(t, 3, reloaded.DroppedEvents)
			assert.Equal(t, terminal, reloaded.LastRunStatus)
		})
	}
}

// TestRecordWatchRateDropsPreservesTerminalStatusWithNoPriorDelivery covers the
// LastRunAt == nil branch: when no delivery was ever recorded, the buggy guard
// treated nil as "no newer evidence" and let the drop flush overwrite the
// terminal status even though the terminal outcome is newer than the drop.
func TestRecordWatchRateDropsPreservesTerminalStatusWithNoPriorDelivery(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	require.NoError(t, AddTask(Task{
		ID: "d4357101", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
		Enabled: true, CreatedAt: created,
	}))
	_, err := UpdateTaskStatus("d4357101", nil, "stopped")
	require.NoError(t, err)

	updated, err := RecordWatchRateDrops("d4357101", 2, created.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, updated.DroppedEvents, "cumulative count must still advance")
	assert.Equal(t, "stopped", updated.LastRunStatus,
		"no prior delivery must not let a drop flush clobber the terminal status")
}

// TestRecordWatchRateDropsStillOverwritesNonTerminalStatus confirms the
// original behavior is preserved: when the on-disk status is not terminal, a
// drop whose timestamp is not older than the last delivery still records the
// dropped outcome (no regression from the added terminal guard).
func TestRecordWatchRateDropsStillOverwritesNonTerminalStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	created := time.Date(2026, time.September, 11, 11, 0, 0, 0, time.UTC)
	require.NoError(t, AddTask(Task{
		ID: "d4357102", Name: "watcher", WatchCmd: "watch.sh", Program: "claude",
		Enabled: true, CreatedAt: created,
	}))
	deliveredAt := created.Add(time.Hour)
	droppedAt := deliveredAt.Add(time.Minute)
	_, err := UpdateTaskStatus("d4357102", &deliveredAt, "sent")
	require.NoError(t, err)

	updated, err := RecordWatchRateDrops("d4357102", 4, droppedAt)
	require.NoError(t, err)
	assert.Equal(t, 4, updated.DroppedEvents)
	assert.Equal(t, WatchRateDropStatus, updated.LastRunStatus,
		"a drop not older than the last delivery must still record the dropped outcome")
	assert.Equal(t, deliveredAt, *updated.LastRunAt)
}

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
