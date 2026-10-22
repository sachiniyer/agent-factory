package task

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoverStaleWatcherStatusRetiresTerminalOnly pins the crashWindow
// recovery rule (#5441): a watch that proves healthy for a full crash window
// earns "started" back ONLY when the persisted status is still a terminal
// watcher outcome. A delivered, dropped, parked, or empty status is newer
// evidence and is never clobbered — and LastRunAt is never touched.
func TestRecoverStaleWatcherStatusRetiresTerminalOnly(t *testing.T) {
	created := time.Date(2026, time.October, 22, 11, 0, 0, 0, time.UTC)
	deliveredAt := created.Add(time.Hour)
	cases := []struct {
		name      string
		status    string
		want      string
		recovered bool
	}{
		{"clean stop", "stopped", "started", true},
		{"breaker give-up", "errored: gave up after 5 failures in 10m0s (last: exit status 1): boom", "started", true},
		{"one-off error", "errored: exit status 1: boom", "started", true},
		{"arming refusal", "errored: not armed — target session is archived", "started", true},
		{"delivered", "sent", "sent", false},
		{"rate drop", WatchRateDropStatus, WatchRateDropStatus, false},
		{"usage-limit park", "parked: usage limit", "parked: usage limit", false},
		{"never ran", "", "", false},
		{"already recovered", "started", "started", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			require.NoError(t, AddTask(Task{
				ID: "f9a0b1c2", Name: "watcher", WatchCmd: "watch.sh",
				Enabled: true, CreatedAt: created,
			}))
			_, err := UpdateTaskStatus("f9a0b1c2", &deliveredAt, c.status)
			require.NoError(t, err)

			recovered, err := RecoverStaleWatcherStatus("f9a0b1c2", "started")
			require.NoError(t, err)
			assert.Equal(t, c.recovered, recovered)

			got, err := GetTask("f9a0b1c2")
			require.NoError(t, err)
			assert.Equal(t, c.want, got.LastRunStatus)
			require.NotNil(t, got.LastRunAt)
			assert.Equal(t, deliveredAt, *got.LastRunAt, "recovery must never touch LastRunAt")
		})
	}
}

// TestRecoverStaleWatcherStatusRejectsMissingAndInvalid covers the error
// paths: a malformed ID is refused before touching the store, and a missing
// task reports not-found rather than fabricating a row.
func TestRecoverStaleWatcherStatusRejectsMissingAndInvalid(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	if _, err := RecoverStaleWatcherStatus("not a task id", "started"); err == nil {
		t.Fatal("expected validation error for malformed task id")
	}
	recovered, err := RecoverStaleWatcherStatus("f9a0b1c3", "started")
	require.Error(t, err)
	assert.False(t, recovered)
}
