package daemon

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

func routeTaskDeliveryToManager(t *testing.T, manager *Manager) {
	t.Helper()
	original := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		return manager.DeliverPrompt(req)
	}
	t.Cleanup(func() { deliverPromptForTask = original })
}

func addTargetedCronTask(t *testing.T, id, repoPath, target, prompt string) {
	t.Helper()
	if err := task.AddTask(task.Task{
		ID: id, Name: id, Prompt: prompt, CronExpr: "0 3 * * *",
		TargetSession: target, ProjectPath: repoPath, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
}

// TestTaskDeliveryParksLimitReachedTargetWithoutTyping is the #4223 regression:
// a cron delivery into a known limit wall is a skipped occurrence, not a send.
// It must reach neither the backend nor the failure recorder, and tasks list must
// say what happened instead of publishing the old false "sent" status.
func TestTaskDeliveryParksLimitReachedTargetWithoutTyping(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, recorder}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.SetLimitReached(time.Now().Add(6 * 24 * time.Hour))
	routeTaskDeliveryToManager(t, manager)
	addTargetedCronTask(t, "a4223001", repoPath, "limited", "scheduled monitor")

	if err := RunTask("a4223001", task.ProjectExpectation{}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("limit-parked task typed into the target: %v", got)
	}
	stored, err := task.GetTask("a4223001")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.LastRunStatus != TaskStatusLimitParked || stored.LastRunAt == nil {
		t.Fatalf("parked run recorded status=%q at=%v, want %q with a timestamp", stored.LastRunStatus, stored.LastRunAt, TaskStatusLimitParked)
	}
}

// TestManualPromptStillDeliversIntoLimitReachedTarget pins the intentional
// asymmetry: an operator may need to answer a credits picker or limit prompt,
// so the manual send path must continue typing even when AF marks the lane.
func TestManualPromptStillDeliversIntoLimitReachedTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, recorder}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.SetLimitReached(time.Now().Add(6 * 24 * time.Hour))

	status, err := manager.SendPromptWithStatus(SendPromptRequest{
		Title: "limited", RepoID: repoID, Prompt: "choose credits",
	})
	if err != nil {
		t.Fatalf("manual SendPrompt: %v", err)
	}
	if status != session.PromptCouldNotConfirm {
		t.Fatalf("manual delivery status = %v, want the recording backend's unchanged could-not-confirm", status)
	}
	if got := recorder.snapshot(); len(got) != 1 || got[0] != "choose credits" {
		t.Fatalf("manual prompt did not land exactly once: %v", got)
	}
}

// TestTaskOriginFinalSendCheckParksLimitReachedTarget pins the last check under
// the session operation lock. DeliverPrompt also checks before entering the
// send path, but liveness can change after that observation; the conclusion
// that no automated keystroke reaches a limited pane must hold at the final
// boundary too.
func TestTaskOriginFinalSendCheckParksLimitReachedTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, recorder}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.SetLimitReached(time.Now().Add(6 * 24 * time.Hour))

	_, err := manager.SendPromptWithStatus(SendPromptRequest{
		Title: "limited", RepoID: repoID, Prompt: "scheduled monitor", TaskOrigin: true,
	})
	if !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("task-origin final send error = %v, want usage-limit sentinel", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("task-origin final send typed into the limited target: %v", got)
	}
}

// TestTaskDeliveryIntoHealthyTargetIsUnchanged is the other preserve half: task
// provenance alone does not defer a healthy target or alter its recorded status.
func TestTaskDeliveryIntoHealthyTargetIsUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, recorder}
	registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, session.Running)
	routeTaskDeliveryToManager(t, manager)
	addTargetedCronTask(t, "a4223002", repoPath, "healthy", "scheduled monitor")

	if err := RunTask("a4223002", task.ProjectExpectation{}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if got := recorder.snapshot(); len(got) != 1 || got[0] != "scheduled monitor" {
		t.Fatalf("healthy task delivery changed: %v", got)
	}
	stored, err := task.GetTask("a4223002")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.LastRunStatus != "sent" || stored.LastRunAt == nil {
		t.Fatalf("healthy run recorded status=%q at=%v, want sent", stored.LastRunStatus, stored.LastRunAt)
	}
}

func TestDeliverWatchEventRecordsLimitParkAndRequestsReplay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	if err := task.AddTask(task.Task{
		ID: "a4223003", Name: "watch-limit", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	original := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (string, error) {
		return TaskStatusLimitParked, nil
	}
	t.Cleanup(func() { deliverPromptForTask = original })

	err := deliverWatchEvent("a4223003", "issue 4223")
	if !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("watch delivery error = %v, want usage-limit replay sentinel", err)
	}
	stored, getErr := task.GetTask("a4223003")
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if stored.LastRunStatus != TaskStatusLimitParked || stored.LastRunAt == nil {
		t.Fatalf("watch park recorded status=%q at=%v, want %q", stored.LastRunStatus, stored.LastRunAt, TaskStatusLimitParked)
	}
}

// TestLimitParkedWatchQueueSurvivesSixDaysAndRestart proves the existing queue's
// ordinary 72-hour expiry cannot discard a distinct event held by a six-day
// usage-limit window. The marker is on disk, a new queue instance recovers it,
// and the old event is delivered rather than expired once the target is healthy.
func TestLimitParkedWatchQueueSurvivesSixDaysAndRestart(t *testing.T) {
	const taskID = "a4223004"
	queueDir := t.TempDir()
	seed := newEventQueue(queueDir, taskID)
	seed.now = func() time.Time { return time.Now().Add(-6 * 24 * time.Hour) }
	if err := seed.enqueue("distinct-old-event", true); err != nil {
		t.Fatalf("enqueue parked event: %v", err)
	}

	s, recorder := newTestSupervisor(t, staticTasks(watchTask(taskID, "sleep 60", t.TempDir())))
	s.queueDir = func() (string, error) { return queueDir, nil }
	s.queueMaxAge = 72 * time.Hour
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	want := taskID + ":distinct-old-event"
	waitUntil(t, 10*time.Second, "six-day parked event to replay after restart", func() bool {
		got := recorder.eventsSnapshot()
		return len(got) == 1 && got[0] == want
	})
	waitUntil(t, 10*time.Second, "drained usage-limit marker removal", func() bool {
		_, err := os.Stat(seed.limitPath)
		return os.IsNotExist(err)
	})
}

// TestLimitParkedWatchQueueBackpressuresAtCapacity verifies the volume half of
// the audit. A protected backlog may cross the ordinary cap by one bounded
// record, but no oldest event is evicted; the reader then waits until replay
// makes room, which backpressures the subprocess pipe instead of growing disk.
func TestLimitParkedWatchQueueBackpressuresAtCapacity(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223005")
	for i := 0; i <= watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("event-%03d", i), i == 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if got := queue.pendingCount(); got != watcherQueueMaxEvents+1 {
		t.Fatalf("protected queue dropped an event at its ordinary cap: pending=%d", got)
	}

	originalPoll := watcherLimitBackpressurePoll
	watcherLimitBackpressurePoll = time.Millisecond
	t.Cleanup(func() { watcherLimitBackpressurePoll = originalPoll })
	w := &taskWatcher{queue: queue, stopCh: make(chan struct{})}
	t.Cleanup(func() { close(w.stopCh) })
	waitDone := make(chan bool, 1)
	go func() { waitDone <- w.waitForLimitQueueCapacity() }()
	select {
	case <-waitDone:
		t.Fatal("watch reader did not backpressure at the protected queue cap")
	case <-time.After(20 * time.Millisecond):
	}

	for i := 0; i < 2; i++ {
		_, cursor, ok, err := queue.peek()
		if err != nil || !ok {
			t.Fatalf("peek %d while making room: ok=%v err=%v", i, ok, err)
		}
		advanceEventQueue(t, queue, cursor)
	}
	select {
	case ok := <-waitDone:
		if !ok {
			t.Fatal("capacity wait reported a stop after replay made room")
		}
	case <-time.After(time.Second):
		t.Fatal("watch reader stayed blocked after replay made room")
	}
}

func TestCleanOrphanQueuesRemovesLimitMarker(t *testing.T) {
	dir := t.TempDir()
	s := newWatcherSupervisor()
	s.queueDir = func() (string, error) { return dir, nil }
	marker := newEventQueue(dir, "a4223006").limitPath
	if err := os.WriteFile(marker, []byte("parked\n"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	s.cleanOrphanQueues(nil, everyWatchTask())
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("orphan usage-limit marker survived cleanup: %v", err)
	}
}
