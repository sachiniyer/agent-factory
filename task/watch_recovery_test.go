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
// earns "started" back ONLY as a compare-and-swap over the exact terminal
// status the watcher was armed with. A newer value on disk — a fresh arming
// refusal, a delivered or parked outcome — is newer evidence and is never
// clobbered, and LastRunAt is never touched.
func TestRecoverStaleWatcherStatusRetiresTerminalOnly(t *testing.T) {
	created := time.Date(2026, time.October, 22, 11, 0, 0, 0, time.UTC)
	deliveredAt := created.Add(time.Hour)
	const giveUp = "errored: gave up after 5 failures in 10m0s (last: exit status 1): boom"
	cases := []struct {
		name      string
		stored    string
		expected  string
		want      string
		recovered bool
	}{
		{"clean stop", "stopped", "stopped", "started", true},
		{"breaker give-up", giveUp, giveUp, "started", true},
		{"one-off error", "errored: exit status 1: boom", "errored: exit status 1: boom", "started", true},
		{"arming refusal", "errored: not armed — target session is archived", "errored: not armed — target session is archived", "started", true},
		{"legacy bare errored", "errored", "errored", "started", true},
		{"fresh refusal over armed give-up", "errored: not armed — target session is archived", giveUp, "errored: not armed — target session is archived", false},
		{"delivered over armed give-up", "sent", giveUp, "sent", false},
		{"rate drop over armed give-up", WatchRateDropStatus, giveUp, WatchRateDropStatus, false},
		{"usage-limit park over armed give-up", "parked: usage limit", giveUp, "parked: usage limit", false},
		{"delivered is terminal-ineligible", "sent", "sent", "sent", false},
		{"empty is terminal-ineligible", "", "", "", false},
		{"already recovered", "started", "started", "started", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			require.NoError(t, AddTask(Task{
				ID: "f9a0b1c2", Name: "watcher", WatchCmd: "watch.sh",
				Enabled: true, CreatedAt: created,
			}))
			_, err := UpdateTaskStatus("f9a0b1c2", &deliveredAt, c.stored)
			require.NoError(t, err)

			recovered, err := RecoverStaleWatcherStatus("f9a0b1c2", c.expected, "started")
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
	if _, err := RecoverStaleWatcherStatus("not a task id", "stopped", "started"); err == nil {
		t.Fatal("expected validation error for malformed task id")
	}
	recovered, err := RecoverStaleWatcherStatus("f9a0b1c3", "stopped", "started")
	require.Error(t, err)
	assert.False(t, recovered)
}
