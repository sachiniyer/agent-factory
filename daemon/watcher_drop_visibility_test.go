package daemon

import (
	"encoding/json"
	"errors"
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

// TestRecordedParkedHeadResumeRetiresTerminalOverlay is the recovery half of
// the unverifiable-exit contract: persistTerminalStatus legitimately latches
// "stopped" — in memory AND on the durable row — while the queue cannot
// confirm a recorded parked head, but once storage heals the retry skips
// commitParkedStatus — the head's status is already durable — so resuming the
// recorded head is what must republish the parked row and retire the latch.
// Without it listings show the stale terminal status over the actionable park
// and past the eventual replay outcome (#4226 review).
func TestRecordedParkedHeadResumeRetiresTerminalOverlay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir := t.TempDir()
	tsk := watchTask("d4357010", `printf 'x\n'`, dir)
	require.NoError(t, task.AddTask(tsk))
	when := time.Now()
	_, err := task.UpdateTaskStatus(tsk.ID, &when, TaskStatusLimitParked)
	require.NoError(t, err)

	seed := newEventQueue(dir, "d4357010")
	if _, err := seed.enqueueWithParkedStatus("held occurrence", true, true); err != nil {
		t.Fatalf("seed recorded parked head: %v", err)
	}
	denyAccess(t, seed.path, seed.path, 0o644)

	statusWrites := 0
	originalUpdate := updateWatchTaskStatus
	updateWatchTaskStatus = func(taskID string, at *time.Time, status string) (task.Task, error) {
		statusWrites++
		return originalUpdate(taskID, at, status)
	}
	t.Cleanup(func() { updateWatchTaskStatus = originalUpdate })

	queue := newEventQueue(dir, "d4357010")
	s := &watcherSupervisor{
		setStatus: func(taskID, status string) {
			persistWatcherStatus(taskID, status)
		},
	}
	delivered := ""
	s.deliver = func(_, line string, _ watchDeliveryOptions) error {
		delivered = line
		return nil
	}
	w := &taskWatcher{taskID: "d4357010", queue: queue, sup: s}
	s.watchers = map[string]*taskWatcher{w.taskID: w}

	w.persistTerminalStatus("stopped")
	stored, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", stored.LastRunStatus,
		"the terminal publication reaches the durable row, not just the overlay")
	s.applyLiveDropState(stored)
	require.Equal(t, "stopped", stored.LastRunStatus,
		"the latched overlay wins while the parked head cannot be verified")

	require.NoError(t, os.Chmod(seed.path, 0o644))
	// Production resumes through the drainer's peek (watcher_drain.go), whose
	// load retry is throttled to one disk attempt per
	// eventQueueLoadRetryInterval. The outage above just consumed that attempt,
	// so peek would answer with the latched error for up to five more seconds
	// and the drainer would observe the heal on a later round. loadFailedFresh
	// is only this test's shortcut to that same recovery without sleeping out
	// the interval; production calls it solely at stop time.
	require.False(t, queue.loadFailedFresh(),
		"storage healed: the fresh check must observe the readable queue")
	ev, cursor, ok, err := queue.peek()
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, w.deliverQueuedEvent(ev, cursor))
	require.Equal(t, "held occurrence", delivered)

	w.mu.Lock()
	latch := w.terminalStatus
	w.mu.Unlock()
	require.Empty(t, latch,
		"a recorded parked head resuming must retire the stale terminal overlay")
	stored, err = task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusLimitParked, stored.LastRunStatus,
		"the reconcile must republish the durable parked status the terminal overwrote")
	s.applyLiveDropState(stored)
	require.Equal(t, TaskStatusLimitParked, stored.LastRunStatus,
		"listings must show the actionable park again, not the terminal it replaced")

	// The reconcile is bounded: retrying the still-recorded head must not
	// rewrite the store every cadence.
	writes := statusWrites
	require.NoError(t, w.deliverQueuedEvent(ev, cursor))
	require.Equal(t, writes, statusWrites,
		"the recorded head's later retries must not rewrite the store")
}

// TestRecordedParkedHeadResumeKeepsDeliveredSentRow is the cursor-loss half of
// the reconcile gate: a recorded parked head that delivered "sent" but whose
// cursor-advance persist failed is still the durable queue head after a
// restart, and redelivering it under at-least-once must not first republish
// "parked: usage limit" over the real sent outcome — if that retry then
// defers or fails, the false limit status would sit indefinitely (Codex on
// #4226). Only terminal publications (stopped/errored) owe the reconcile.
func TestRecordedParkedHeadResumeKeepsDeliveredSentRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir := t.TempDir()
	tsk := watchTask("d4357012", `printf 'x\n'`, dir)
	require.NoError(t, task.AddTask(tsk))
	when := time.Now()
	_, err := task.UpdateTaskStatus(tsk.ID, &when, "sent")
	require.NoError(t, err)

	seed := newEventQueue(dir, "d4357012")
	if _, err := seed.enqueueWithParkedStatus("held occurrence", true, true); err != nil {
		t.Fatalf("seed recorded parked head: %v", err)
	}

	statusWrites := 0
	originalUpdate := updateWatchTaskStatus
	updateWatchTaskStatus = func(taskID string, at *time.Time, status string) (task.Task, error) {
		statusWrites++
		require.NotEqual(t, TaskStatusLimitParked, status,
			"a delivered head must never be republished as parked")
		return originalUpdate(taskID, at, status)
	}
	t.Cleanup(func() { updateWatchTaskStatus = originalUpdate })

	queue := newEventQueue(dir, "d4357012")
	delivered := ""
	s := &watcherSupervisor{
		setStatus: func(taskID, status string) { persistWatcherStatus(taskID, status) },
	}
	s.deliver = func(_, line string, _ watchDeliveryOptions) error {
		delivered = line
		return nil
	}
	w := &taskWatcher{taskID: "d4357012", queue: queue, sup: s}
	s.watchers = map[string]*taskWatcher{w.taskID: w}

	ev, cursor, ok, err := queue.peek()
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, w.deliverQueuedEvent(ev, cursor))
	require.Equal(t, "held occurrence", delivered)
	require.Zero(t, statusWrites,
		"a sent row is not a terminal overwrite — the reconcile owes nothing")

	stored, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"the real delivered outcome must survive the parked head's redelivery")
}

// TestRecordedParkedHeadResumeKeepsArmingRefusal pins the one "errored:" row
// that is not a terminal publication. Arming writes its refusal before
// watchers.reconcile stops the watcher, and a drainer retry in that window must
// not republish the park over it, even with an earlier terminal overlay still
// latched (#2929, #4226 review).
func TestRecordedParkedHeadResumeKeepsArmingRefusal(t *testing.T) {
	for name, overlay := range map[string]string{"row only": "", "overlay latched": "stopped"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			dir := t.TempDir()
			tsk := watchTask("d4357013", `printf 'x\n'`, dir)
			require.NoError(t, task.AddTask(tsk))
			refusal := notArmedStatus(errors.New("target session is archived"))
			_, err := task.UpdateTaskStatus(tsk.ID, nil, refusal)
			require.NoError(t, err)

			seed := newEventQueue(dir, tsk.ID)
			_, err = seed.enqueueWithParkedStatus("held occurrence", true, true)
			require.NoError(t, err)

			queue := newEventQueue(dir, tsk.ID)
			s := &watcherSupervisor{deliver: func(string, string, watchDeliveryOptions) error { return nil }}
			w := &taskWatcher{taskID: tsk.ID, queue: queue, sup: s, terminalStatus: overlay}
			s.watchers = map[string]*taskWatcher{w.taskID: w}

			ev, cursor, ok, err := queue.peek()
			require.NoError(t, err)
			require.True(t, ok)
			require.True(t, queue.parkedStatusRecorded(cursor),
				"precondition: the head must be recorded, or the reconcile is never reached")
			require.NoError(t, w.deliverQueuedEvent(ev, cursor))

			stored, err := task.GetTask(tsk.ID)
			require.NoError(t, err)
			require.Equal(t, refusal, stored.LastRunStatus,
				"a drainer retry must not republish the park over the arming refusal")
		})
	}
}
