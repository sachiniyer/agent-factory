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
// earns "started" back ONLY as a compare-and-swap over the exact store row
// the watcher was armed with — status text AND every other persisted field.
// A row rewritten inside the window — a fresh arming refusal, a delivered or
// parked outcome, an unrelated field edit — is newer evidence and is never
// clobbered, while a rewrite that changes nothing persisted cannot carry new
// evidence either (Codex #5457). LastRunAt is never touched.
func TestRecoverStaleWatcherStatusRetiresTerminalOnly(t *testing.T) {
	created := time.Date(2026, time.October, 22, 11, 0, 0, 0, time.UTC)
	deliveredAt := created.Add(time.Hour)
	const giveUp = "errored: gave up after 5 failures in 10m0s (last: exit status 1): boom"
	const refusal = "errored: not armed — target session is archived"
	sameRefusal := refusal
	refusalV := refusal
	sent := "sent"
	drop := WatchRateDropStatus
	park := "parked: usage limit"
	cases := []struct {
		name      string
		armed     string  // status the watcher armed over
		rewrite   *string // a status write that lands inside the window, if any
		want      string  // persisted status after the recovery attempt
		recovered bool
	}{
		{"clean stop", "stopped", nil, "started", true},
		{"breaker give-up", giveUp, nil, "started", true},
		{"one-off error", "errored: exit status 1: boom", nil, "started", true},
		{"arming refusal", refusal, nil, "started", true},
		{"legacy bare errored", "errored", nil, "started", true},
		{"fresh refusal over armed give-up", giveUp, &refusalV, refusal, false},
		{"identical status rewritten — row unchanged", refusal, &sameRefusal, "started", true},
		{"delivered over armed give-up", giveUp, &sent, sent, false},
		{"rate drop over armed give-up", giveUp, &drop, drop, false},
		{"usage-limit park over armed give-up", giveUp, &park, park, false},
		{"delivered is terminal-ineligible", sent, nil, sent, false},
		{"empty is terminal-ineligible", "", nil, "", false},
		{"already recovered", "started", nil, "started", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			require.NoError(t, AddTask(Task{
				ID: "f9a0b1c2", Name: "watcher", WatchCmd: "watch.sh",
				Enabled: true, CreatedAt: created,
			}))
			_, err := UpdateTaskStatus("f9a0b1c2", &deliveredAt, c.armed)
			require.NoError(t, err)

			// The watcher's arming read: the store row it armed over.
			armed, err := GetTask("f9a0b1c2")
			require.NoError(t, err)

			if c.rewrite != nil {
				_, err := UpdateTaskStatus("f9a0b1c2", &deliveredAt, *c.rewrite)
				require.NoError(t, err)
			}

			recovered, err := RecoverStaleWatcherStatus("f9a0b1c2", *armed, "started")
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

// TestRecoverStaleWatcherStatusRowDriftRefuses pins the row half of the CAS:
// the comparison is over persisted content, not the status string alone. An
// arming record whose other fields no longer match the stored row — whatever
// wrote them — cannot claim the swap even when the status text is identical.
func TestRecoverStaleWatcherStatusRowDriftRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, AddTask(Task{
		ID: "f9a0b1c2", Name: "watcher", WatchCmd: "watch.sh", Enabled: true,
	}))
	_, err := UpdateTaskStatus("f9a0b1c2", nil, "stopped")
	require.NoError(t, err)

	armed, err := GetTask("f9a0b1c2")
	require.NoError(t, err)
	armed.Name = "a different record than the one on disk"

	recovered, err := RecoverStaleWatcherStatus("f9a0b1c2", *armed, "started")
	require.NoError(t, err)
	assert.False(t, recovered)

	got, err := GetTask("f9a0b1c2")
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.LastRunStatus)
}

// TestRecoverStaleWatcherStatusRejectsMissingAndInvalid covers the error
// paths: a malformed ID is refused before touching the store, and a missing
// task reports not-found rather than fabricating a row.
func TestRecoverStaleWatcherStatusRejectsMissingAndInvalid(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	armed := Task{ID: "f9a0b1c3", LastRunStatus: "stopped"}
	if _, err := RecoverStaleWatcherStatus("not a task id", armed, "started"); err == nil {
		t.Fatal("expected validation error for malformed task id")
	}
	recovered, err := RecoverStaleWatcherStatus("f9a0b1c3", armed, "started")
	require.Error(t, err)
	assert.False(t, recovered)
}
