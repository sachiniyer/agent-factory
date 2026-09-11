package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

func TestCreatePerRunWatchLimitParkDoesNotRequestQueueReplay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	if err := task.AddTask(task.Task{
		ID: "a4223101", Name: "watch-create-limit", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", ProjectPath: repoPath, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	original := createSessionForTask
	creates := 0
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		creates++
		return &session.InstanceData{Title: "parked-run", Liveness: session.LiveLimitReached}, nil
	}
	t.Cleanup(func() { createSessionForTask = original })

	if err := deliverWatchEvent("a4223101", "issue 4223"); err != nil {
		t.Fatalf("create-per-run park requested duplicate queue replay: %v", err)
	}
	if creates != 1 {
		t.Fatalf("created %d parked sessions, want exactly 1", creates)
	}
	stored, err := task.GetTask("a4223101")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.LastRunStatus != TaskStatusLimitParked {
		t.Fatalf("create-per-run status = %q, want %q", stored.LastRunStatus, TaskStatusLimitParked)
	}
}

func TestTaskPromptLimitTransitionIsSerializedWithFinalSend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, recorder}
	inst := registerStarted(t, manager, repoID, repoPath, "limited-race", backend, true, session.Running)

	manager.accountLimitMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			manager.accountLimitMu.Unlock()
		}
	})
	reachedFence := make(chan struct{})
	originalHook := testHookTaskPromptBeforeLimitFence
	var once sync.Once
	testHookTaskPromptBeforeLimitFence = func() { once.Do(func() { close(reachedFence) }) }
	t.Cleanup(func() { testHookTaskPromptBeforeLimitFence = originalHook })

	done := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "limited-race", RepoID: repoID, Prompt: "scheduled", TaskOrigin: true,
		})
		done <- err
	}()
	select {
	case <-reachedFence:
	case <-time.After(time.Second):
		t.Fatal("task send did not reach final limit fence")
	}
	// This is the poll's publication operation, performed while holding the same
	// manager fence setLimitReachedAtEpoch uses in production.
	inst.SetLimitReached(time.Now().Add(time.Hour))
	manager.accountLimitMu.Unlock()
	locked = false

	select {
	case err := <-done:
		if !errors.Is(err, errTargetLimitReached) {
			t.Fatalf("send raced past limit publication: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("task send stayed blocked after limit publication")
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("racing task prompt reached pane: %v", got)
	}
}

func TestExistingBacklogIsProtectedBeforeLimitBurstCanEvict(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "limited-backlog", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	manager.setLimitReached(inst, time.Now().Add(time.Hour))
	if err := task.AddTask(task.Task{
		ID: "a4223102", Name: "watch-existing-backlog", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited-backlog", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	queue := newEventQueue(t.TempDir(), "a4223102")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i)); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: "a4223102", sup: s, queue: queue, stopCh: stopCh}
	w.handleEvent("new-after-limit", &tailBuffer{})

	if got := queue.pendingCount(); got != watcherQueueMaxEvents+1 {
		t.Fatalf("limit transition allowed cap eviction: pending=%d", got)
	}
	ev, _, ok, err := queue.peek()
	if err != nil || !ok || ev.Line != "old-000" {
		t.Fatalf("oldest distinct event was evicted: event=%+v ok=%v err=%v", ev, ok, err)
	}
}

func TestStopPersistsCompleteEventsPrefetchedBeforeLimitBackpressure(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223103")
	if err := queue.enqueue("already-parked", true); err != nil {
		t.Fatalf("seed parked queue: %v", err)
	}
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{
		taskID: "a4223103", sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh,
	}
	br := bufio.NewReaderSize(strings.NewReader("prefetched-one\nprefetched-two\npartial"), maxWatchLineBytes)
	if _, err := br.Peek(len("prefetched-one\nprefetched-two\npartial")); err != nil {
		t.Fatalf("prefetch fixture: %v", err)
	}
	w.persistBufferedLimitEvents(br, &tailBuffer{})

	var got []string
	for {
		ev, cursor, ok, err := queue.peek()
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, ev.Line)
		if advanced, err := queue.advance(cursor); err != nil || !advanced {
			t.Fatalf("advance: advanced=%v err=%v", advanced, err)
		}
	}
	want := []string{"already-parked", "prefetched-one", "prefetched-two"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("persisted events = %v, want %v", got, want)
	}
}
