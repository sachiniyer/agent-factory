package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

func TestWatchWriterExitBreaksLimitCapacityWait(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "writer-exit-at-capacity")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("event-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	t.Cleanup(stop)
	writersStopped := make(chan struct{})
	close(writersStopped)
	w := &taskWatcher{
		taskID: "writer-exit-at-capacity", sup: newWatcherSupervisor(),
		queue: queue, stopCh: stopCh,
	}
	done := make(chan struct{})
	go func() {
		w.consumeLines(strings.NewReader(""), &tailBuffer{}, writersStopped)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		stop()
		<-done
		t.Fatal("stdout reader stayed capacity-blocked after its writer exited")
	}
}

func TestParkedStatusPublicationExcludesSupervisorStatus(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	const taskID = "a4226001"
	if err := task.AddTask(task.Task{
		ID: taskID, Name: "watch-park-publication", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	originalDeliver := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		return taskPromptDeliveryResult{status: TaskStatusLimitParked}, nil
	}
	t.Cleanup(func() { deliverPromptForTask = originalDeliver })

	originalUpdate := updateWatchTaskStatus
	parkCommitted := make(chan struct{})
	releasePark := make(chan struct{})
	var parkOnce sync.Once
	updateWatchTaskStatus = func(id string, at *time.Time, status string) (task.Task, error) {
		stored, err := originalUpdate(id, at, status)
		if status == TaskStatusLimitParked {
			parkOnce.Do(func() { close(parkCommitted) })
			<-releasePark
		}
		return stored, err
	}
	t.Cleanup(func() {
		select {
		case <-releasePark:
		default:
			close(releasePark)
		}
		updateWatchTaskStatus = originalUpdate
	})

	queue := newEventQueue(t.TempDir(), taskID)
	if err := queue.enqueue("one occurrence"); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	ev, cursor, ok, err := queue.peek()
	if err != nil || !ok {
		t.Fatalf("peek seeded event: ok=%v err=%v", ok, err)
	}
	w := &taskWatcher{taskID: taskID, sup: newWatcherSupervisor(), queue: queue}
	deliveryDone := make(chan error, 1)
	go func() { deliveryDone <- w.deliverQueuedEvent(ev, cursor) }()
	select {
	case <-parkCommitted:
	case <-time.After(time.Second):
		t.Fatal("parked status did not reach the task store")
	}

	supervisorDone := make(chan struct{})
	go func() {
		w.persistSupervisorStatus("stopped")
		close(supervisorDone)
	}()
	select {
	case <-supervisorDone:
	case <-time.After(100 * time.Millisecond):
	}
	close(releasePark)
	if err := <-deliveryDone; !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("first park = %v, want replay request", err)
	}
	select {
	case <-supervisorDone:
	case <-time.After(time.Second):
		t.Fatal("supervisor status remained blocked after parked publication")
	}
	if err := w.deliverQueuedEvent(ev, cursor); !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("parked retry = %v, want replay request", err)
	}
	stored, err := task.GetTask(taskID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if stored.LastRunStatus != TaskStatusLimitParked {
		t.Fatalf("parked occurrence hidden after retry: status=%q, want %q", stored.LastRunStatus, TaskStatusLimitParked)
	}
}
