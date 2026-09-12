package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// TestWatcherRateDropIsVisibleOnTaskAndListAPI drives the real watcher rate
// limiter, then asserts only on public task surfaces. The private count is used
// solely to wait until the asynchronous source reader has observed both drops;
// a passing test must prove a user can see that loss without that private state.
func TestWatcherRateDropIsVisibleOnTaskAndListAPI(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir := t.TempDir()
	tsk := watchTask("d4357001", `printf 'one\ntwo\nthree\n'; sleep 60`, dir)
	require.NoError(t, task.AddTask(tsk))
	when := time.Now()
	_, err := task.UpdateTaskStatus(tsk.ID, &when, "sent")
	require.NoError(t, err)

	s := newWatcherSupervisor()
	s.eventsPerMinute = 1
	s.deliver = func(_, _ string) error { return nil }
	logDir := t.TempDir()
	s.logPath = func(taskID string) (string, error) {
		return filepath.Join(logDir, "task-"+taskID+".log"), nil
	}
	queueDir := t.TempDir()
	s.queueDir = func() (string, error) { return queueDir, nil }
	t.Cleanup(s.Stop)
	require.NoError(t, s.Reload())
	waitUntil(t, 5*time.Second, "two rate-dropped events", func() bool {
		return s.droppedEvents(tsk.ID) == 2
	})

	server := &controlServer{watchers: s}
	var response ListTasksResponse
	require.NoError(t, server.ListTasks(ListTasksRequest{}, &response))
	require.Len(t, response.Tasks, 1)
	require.Contains(t, response.Tasks[0].LastRunStatus, "dropped",
		"a run that lost events must not retain the same sent status as a lossless run")
	assertDroppedEventCount(t, response.Tasks[0], 2)

	// A graceful stop flushes the coalesced counter so the no-daemon CLI disk
	// fallback remains exact without turning every excess source line into a
	// tasks.json rewrite.
	s.Stop()
	stored, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Contains(t, stored.LastRunStatus, "dropped")
	assertDroppedEventCount(t, stored, 2)
}

func assertDroppedEventCount(t *testing.T, record any, want float64) {
	t.Helper()
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	got, ok := fields["dropped_events"]
	require.True(t, ok, "task JSON has no dropped_events field: %s", strings.TrimSpace(string(encoded)))
	require.Equal(t, want, got)
}

func TestLiveDropOverlayPreservesANewerSuccessfulDelivery(t *testing.T) {
	droppedAt := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	deliveredAt := droppedAt.Add(time.Second)
	w := &taskWatcher{dropped: 4, lastDroppedAt: droppedAt, lastDeliveredAt: deliveredAt}
	s := &watcherSupervisor{watchers: map[string]*taskWatcher{"d4357003": w}}
	record := task.Task{ID: "d4357003", LastRunStatus: "sent"}

	s.applyLiveDropState(&record)

	require.Equal(t, 4, record.DroppedEvents)
	require.Equal(t, "sent", record.LastRunStatus)
}
