package daemon

import (
	"encoding/json"
	"os"
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
	s.deliver = func(_, _ string, _ watchDeliveryOptions) error { return nil }
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

func TestLiveDropOverlayPreservesTerminalWatcherStatus(t *testing.T) {
	droppedAt := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	for _, terminal := range []string{"stopped", "errored: exit status 1"} {
		t.Run(terminal, func(t *testing.T) {
			var persisted string
			w := &taskWatcher{taskID: "d4357004", dropped: 4, lastDroppedAt: droppedAt}
			s := &watcherSupervisor{
				watchers:  map[string]*taskWatcher{w.taskID: w},
				setStatus: func(_, status string) { persisted = status },
			}
			w.sup = s
			w.persistTerminalStatus(terminal)
			record := task.Task{ID: "d4357004", LastRunStatus: terminal}

			s.applyLiveDropState(&record)

			require.Equal(t, 4, record.DroppedEvents)
			require.Equal(t, terminal, record.LastRunStatus)
			require.Equal(t, terminal, persisted)
		})
	}
}

func TestLiveDropOverlayPreservesRecordedParkedHeadOverTerminalStatus(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "d4357005")
	_, err := queue.enqueueWithParkedStatus("held occurrence", true, true)
	require.NoError(t, err)
	var persisted string
	w := &taskWatcher{taskID: "d4357005", queue: queue}
	s := &watcherSupervisor{
		watchers:  map[string]*taskWatcher{w.taskID: w},
		setStatus: func(_, status string) { persisted = status },
	}
	w.sup = s
	w.persistTerminalStatus("stopped")
	record := task.Task{ID: w.taskID, LastRunStatus: TaskStatusLimitParked}

	s.applyLiveDropState(&record)

	require.Equal(t, TaskStatusLimitParked, record.LastRunStatus)
	require.Empty(t, persisted, "terminal persistence must not hide a recorded parked occurrence")
	w.mu.Lock()
	terminalStatus := w.terminalStatus
	w.mu.Unlock()
	require.Empty(t, terminalStatus, "live overlay must not latch a terminal status over a parked head")
}

// TestEnqueueAppendFailureCountsTheUnretainedEvent is the finding's core: a
// queue that exists but cannot retain the record loses the event exactly as a
// missing queue does, so the loss must reach the same drop accounting rather
// than vanish behind a log line.
func TestEnqueueAppendFailureCountsTheUnretainedEvent(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "d4357007")
	require.NoError(t, queue.enqueue("seed backlog"))
	persisted := -1
	s := &watcherSupervisor{
		recordDrops: func(_ string, total int, _ time.Time) error { persisted = total; return nil },
	}
	w := &taskWatcher{taskID: "d4357007", queue: queue, sup: s}
	denyAccess(t, queue.path, queue.path, 0o644)

	tail := &tailBuffer{}
	w.enqueueEvent("limit-held occurrence", tail, true)

	w.mu.Lock()
	dropped := w.dropped
	w.mu.Unlock()
	require.Equal(t, 1, dropped, "an event the queue could not retain must be counted")
	require.Equal(t, 1, persisted, "the loss must reach the durable drop checkpoint")
}

// TestTerminalStatusPublishesWithoutEvidenceOfRecordedParkedHead pins both
// halves of the parked-head contract: an unverifiable queue is not evidence of
// a parked head, so a real terminal outcome publishes rather than leaving an
// ordinary backlog displaying stale status past a permanently stopped watcher;
// and once storage heals the same check reads fresh state rather than
// deferring to a stale cached outage.
func TestTerminalStatusPublishesWithoutEvidenceOfRecordedParkedHead(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "d4357008")
	require.NoError(t, seed.enqueue("ordinary backlog"))
	denyAccess(t, seed.path, seed.path, 0o644)

	queue := newEventQueue(dir, "d4357008")
	var persisted string
	s := &watcherSupervisor{setStatus: func(_, status string) { persisted = status }}
	w := &taskWatcher{taskID: "d4357008", queue: queue, sup: s}

	w.persistTerminalStatus("stopped")
	require.Equal(t, "stopped", persisted,
		"unverifiable queue state is not evidence of a parked head; the terminal outcome must publish")

	persisted = ""
	require.NoError(t, os.Chmod(seed.path, 0o644))
	w.persistTerminalStatus("stopped")
	require.Equal(t, "stopped", persisted,
		"once queue state is readable and shows no parked head, the terminal outcome publishes")
}

func TestNewerParkRetiresEarlierLiveTerminalOverlay(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "d4357006")
	require.NoError(t, queue.enqueue("held occurrence"))
	_, cursor, ok, err := queue.peek()
	require.NoError(t, err)
	require.True(t, ok)
	persisted := ""
	w := &taskWatcher{taskID: "d4357006", queue: queue}
	s := &watcherSupervisor{
		watchers:  map[string]*taskWatcher{w.taskID: w},
		setStatus: func(_, status string) { persisted = status },
	}
	w.sup = s
	w.persistTerminalStatus("stopped")
	recorded, err := w.commitParkedStatus(cursor, func() error {
		persisted = TaskStatusLimitParked
		return nil
	})
	require.NoError(t, err)
	require.True(t, recorded)
	record := task.Task{ID: w.taskID, LastRunStatus: persisted}

	s.applyLiveDropState(&record)

	require.Equal(t, TaskStatusLimitParked, record.LastRunStatus)
	w.mu.Lock()
	terminalStatus := w.terminalStatus
	w.mu.Unlock()
	require.Empty(t, terminalStatus, "newer parked publication must retire an older terminal overlay")
}
