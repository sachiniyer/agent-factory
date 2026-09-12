package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestTargetedAutoCreateLimitParkDoesNotRequestQueueReplay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	if err := task.AddTask(task.Task{
		ID: "a4223104", Name: "watch-target-create-limit", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "missing-target", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	original := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		return taskPromptDeliveryResult{status: TaskStatusLimitParked, promptRetained: true}, nil
	}
	t.Cleanup(func() { deliverPromptForTask = original })

	if err := deliverWatchEvent("a4223104", "issue 4223"); err != nil {
		t.Fatalf("targeted auto-create park requested duplicate queue replay: %v", err)
	}
	stored, err := task.GetTask("a4223104")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.LastRunStatus != TaskStatusLimitParked {
		t.Fatalf("targeted auto-create status = %q, want %q", stored.LastRunStatus, TaskStatusLimitParked)
	}
}

type blockingLimitSnapshotBackend struct {
	readyFakeBackend
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	recorder *promptRecorder
}

func (b *blockingLimitSnapshotBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return false, false, claudeLimitBanner
}

func (b *blockingLimitSnapshotBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.recorder.add(prompt)
	return nil
}

func TestTaskPromptWaitsForInFlightLimitSnapshotSettlement(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := &blockingLimitSnapshotBackend{
		readyFakeBackend: readyFakeBackend{session.NewFakeBackend()},
		started:          make(chan struct{}), release: make(chan struct{}), recorder: recorder,
	}
	inst := registerStarted(t, manager, repoID, repoPath, "snapshot-race", backend, true, session.Running)

	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("status poll did not enter pane snapshot")
	}

	reachedObservationFence := make(chan struct{})
	originalHook := testHookTaskPromptBeforeObservationFence
	var once sync.Once
	testHookTaskPromptBeforeObservationFence = func() {
		once.Do(func() { close(reachedObservationFence) })
	}
	t.Cleanup(func() { testHookTaskPromptBeforeObservationFence = originalHook })
	sendDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "snapshot-race", RepoID: repoID, Prompt: "scheduled", TaskOrigin: true,
		})
		sendDone <- err
	}()
	select {
	case <-reachedObservationFence:
	case <-time.After(time.Second):
		t.Fatal("task send did not reach observation-settlement fence")
	}
	close(backend.release)

	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("status poll did not publish captured usage limit")
	}
	select {
	case err := <-sendDone:
		if !errors.Is(err, errTargetLimitReached) {
			t.Fatalf("send overtook captured limit observation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("task send stayed blocked after observation settlement")
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("task prompt overtook captured limit observation: %v", got)
	}
}

func TestWatchQueueAdmissionWaitsForInFlightLimitSnapshotSettlement(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &blockingLimitSnapshotBackend{
		readyFakeBackend: readyFakeBackend{session.NewFakeBackend()},
		started:          make(chan struct{}), release: make(chan struct{}), recorder: &promptRecorder{},
	}
	inst := registerStarted(t, manager, repoID, repoPath, "queue-snapshot-race", backend, true, session.Running)
	if err := task.AddTask(task.Task{
		ID: "a4223107", Name: "watch-queue-snapshot-race", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "queue-snapshot-race", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	queue := newEventQueue(t.TempDir(), "a4223107")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i)); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: "a4223107", sup: s, queue: queue, stopCh: stopCh}

	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("status poll did not enter pane snapshot")
	}

	reachedObservationFence := make(chan struct{})
	originalHook := testHookTaskLimitObserveBeforeObservationFence
	var once sync.Once
	testHookTaskLimitObserveBeforeObservationFence = func() {
		once.Do(func() { close(reachedObservationFence) })
	}
	t.Cleanup(func() { testHookTaskLimitObserveBeforeObservationFence = originalHook })
	handleDone := make(chan struct{})
	go func() {
		w.handleEvent("new-after-captured-limit", &tailBuffer{})
		close(handleDone)
	}()
	select {
	case <-reachedObservationFence:
	case <-time.After(time.Second):
		t.Fatal("queue admission did not reach observation-settlement fence")
	}
	select {
	case <-handleDone:
		t.Fatal("queue admission overtook an in-flight limit snapshot")
	case <-time.After(20 * time.Millisecond):
	}
	close(backend.release)
	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("status poll did not publish captured usage limit")
	}
	select {
	case <-handleDone:
	case <-time.After(time.Second):
		t.Fatal("queue admission stayed blocked after observation settlement")
	}

	if got := queue.pendingCount(); got != watcherQueueMaxEvents+1 {
		t.Fatalf("captured limit allowed cap eviction: pending=%d", got)
	}
	ev, _, ok, err := queue.peek()
	if err != nil || !ok || ev.Line != "old-000" {
		t.Fatalf("oldest distinct event was evicted: event=%+v ok=%v err=%v", ev, ok, err)
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

func TestSlowTaskPromptDoesNotBlockAnUnrelatedTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	firstRecorder := &promptRecorder{}
	firstBackend := &blockingSendKillBackend{
		readyFakeBackend: readyFakeBackend{session.NewFakeBackend()},
		rec:              firstRecorder,
		sendStarted:      make(chan struct{}),
		releaseSend:      make(chan struct{}),
		killStarted:      make(chan struct{}),
		killBlock:        make(chan struct{}),
	}
	registerStarted(t, manager, repoID, repoPath, "slow-target", firstBackend, true, session.Running)
	secondRecorder := &promptRecorder{}
	registerStarted(t, manager, repoID, repoPath, "independent-target", recordingBackend{
		readyFakeBackend{session.NewFakeBackend()}, secondRecorder,
	}, true, session.Running)

	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "slow-target", RepoID: repoID, Prompt: "first", TaskOrigin: true,
		})
		firstDone <- err
	}()
	select {
	case <-firstBackend.sendStarted:
	case <-time.After(time.Second):
		t.Fatal("first prompt did not reach its blocking transport")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "independent-target", RepoID: repoID, Prompt: "second", TaskOrigin: true,
		})
		secondDone <- err
	}()
	secondBlocked := false
	select {
	case err := <-secondDone:
		if err != nil {
			close(firstBackend.releaseSend)
			<-firstDone
			t.Fatalf("unrelated prompt failed: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		secondBlocked = true
	}
	close(firstBackend.releaseSend)
	if err := <-firstDone; err != nil {
		t.Fatalf("first prompt failed after release: %v", err)
	}
	if secondBlocked {
		if err := <-secondDone; err != nil {
			t.Fatalf("unrelated prompt failed after slow transport released: %v", err)
		}
		t.Fatal("unrelated task prompt was serialized behind another target's transport I/O")
	}
	if got := secondRecorder.snapshot(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("unrelated target received prompts %v, want [second]", got)
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

func TestLimitedTargetQueuesBeforeLiveRateDrop(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "limited-rate", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	manager.setLimitReached(inst, time.Now().Add(time.Hour))
	if err := task.AddTask(task.Task{
		ID: "a4223110", Name: "watch-limited-rate", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited-rate", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{
		taskID: "a4223110", sup: s, queue: newEventQueue(t.TempDir(), "a4223110"), stopCh: stopCh,
	}
	for i := 0; i < s.eventsPerMinute; i++ {
		w.eventTimes = append(w.eventTimes, time.Now())
	}

	w.handleEvent("must-survive-rate-window", &tailBuffer{})
	if got := w.queue.pendingCount(); got != 1 {
		t.Fatalf("limited target event was rate-dropped: pending=%d", got)
	}
	if !w.queue.retainLimitParked() {
		t.Fatal("limited target event did not establish protected backlog")
	}
	if w.dropped != 0 {
		t.Fatalf("limited target event incremented rate-drop counter: %d", w.dropped)
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
	w.persistRemainingLimitEvents(br, &tailBuffer{})

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

func TestStopDrainsCompleteEventsAlreadyAcceptedByKernelPipe(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223108")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("kernel-one\nkernel-two\n"); err != nil {
		t.Fatalf("write kernel-buffered events: %v", err)
	}

	stopCh := make(chan struct{})
	close(stopCh)
	writersStopped := make(chan struct{})
	w := &taskWatcher{taskID: "a4223108", sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh}
	done := make(chan struct{})
	go func() {
		w.consumeLines(reader, &tailBuffer{}, writersStopped)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("reader closed before the stopped writer released its kernel pipe")
	case <-time.After(20 * time.Millisecond):
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	close(writersStopped)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish draining the closed kernel pipe")
	}
	if got := queue.pendingCount(); got != watcherQueueMaxEvents+2 {
		t.Fatalf("kernel-buffered events were lost: pending=%d", got)
	}
}

func TestOrdinaryStopDoesNotPromotePrefetchedLinesToLimitBacklog(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223105")
	if err := queue.enqueue("ordinary-backlog"); err != nil {
		t.Fatalf("seed ordinary queue: %v", err)
	}
	stopCh := make(chan struct{})
	s := newWatcherSupervisor()
	var stopOnce sync.Once
	s.observeTargetLimit = func(string) (bool, error) {
		stopOnce.Do(func() { close(stopCh) })
		return false, nil
	}
	w := &taskWatcher{taskID: "a4223105", sup: s, queue: queue, stopCh: stopCh}
	w.consumeLines(strings.NewReader("first\nsecond\nthird\n"), &tailBuffer{}, nil)

	if got := queue.pendingCount(); got != 2 {
		t.Fatalf("ordinary stop persisted prefetched events as limit backlog: pending=%d, want seed + first only", got)
	}
	if queue.retainLimitParked() {
		t.Fatal("ordinary stop created a usage-limit retention marker")
	}
}

func TestAgedBacklogObservesTargetLimitBeforeExpiringHead(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "limited-aged", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	manager.setLimitReached(inst, time.Now().Add(time.Hour))
	if err := task.AddTask(task.Task{
		ID: "a4223106", Name: "watch-aged-backlog", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited-aged", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	queue := newEventQueue(t.TempDir(), "a4223106")
	queue.now = func() time.Time { return time.Now().Add(-73 * time.Hour) }
	if err := queue.enqueue("aged-distinct-event"); err != nil {
		t.Fatalf("seed aged queue: %v", err)
	}
	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	s.deliver = adaptWatchDelivery(func(string, string) error { return errTargetLimitReached })
	s.queueMaxAge = 72 * time.Hour
	s.drainBaseBackoff = time.Hour
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "a4223106", sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	delivered := make(chan struct{})
	originalDeliver := s.deliver
	s.deliver = func(taskID, line string, options watchDeliveryOptions) error {
		close(delivered)
		return originalDeliver(taskID, line, options)
	}
	go w.drainLoop()
	select {
	case <-delivered:
	case <-time.After(time.Second):
		close(stopCh)
		w.wg.Wait()
		t.Fatal("aged event expired before target limit observation")
	}
	close(stopCh)
	w.wg.Wait()
	if got := queue.pendingCount(); got != 1 {
		t.Fatalf("aged limit-held event was consumed: pending=%d", got)
	}
	if !queue.retainLimitParked() {
		t.Fatal("aged limit-held backlog was not durably protected")
	}
}

func TestParkedQueueHeadDoesNotRewriteTaskStoreAfterWatcherStops(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	if err := task.AddTask(task.Task{
		ID: "a4223109", Name: "watch-park-timestamp", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "limited", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	original := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		return taskPromptDeliveryResult{status: TaskStatusLimitParked}, nil
	}
	t.Cleanup(func() { deliverPromptForTask = original })
	originalUpdate := updateWatchTaskStatus
	statusWrites := 0
	updateWatchTaskStatus = func(taskID string, at *time.Time, status string) (task.Task, error) {
		statusWrites++
		return originalUpdate(taskID, at, status)
	}
	t.Cleanup(func() { updateWatchTaskStatus = originalUpdate })

	queueDir := t.TempDir()
	queue := newEventQueue(queueDir, "a4223109")
	if err := queue.enqueue("one occurrence"); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	ev, cursor, ok, err := queue.peek()
	if err != nil || !ok {
		t.Fatalf("peek seeded event: ok=%v err=%v", ok, err)
	}
	s := newWatcherSupervisor()
	w := &taskWatcher{taskID: "a4223109", sup: s, queue: queue}
	if err := w.deliverQueuedEvent(ev, cursor); !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("first park = %v, want replay request", err)
	}
	first, err := task.GetTask("a4223109")
	if err != nil || first.LastRunAt == nil {
		t.Fatalf("first parked status: task=%+v err=%v", first, err)
	}
	if statusWrites != 1 {
		t.Fatalf("first parked occurrence wrote task store %d times, want 1", statusWrites)
	}
	storeInfo, err := os.Stat(filepath.Join(home, "tasks.json"))
	if err != nil {
		t.Fatalf("stat task store: %v", err)
	}

	// Reopen the durable queue as a restarted watcher would. Its lifecycle
	// report must not replace the still-actionable parked status, and retrying
	// the same head must perform zero task-store writes.
	reopened := newEventQueue(queueDir, "a4223109")
	ev, cursor, ok, err = reopened.peek()
	if err != nil || !ok {
		t.Fatalf("peek reopened event: ok=%v err=%v", ok, err)
	}
	restarted := &taskWatcher{taskID: "a4223109", sup: s, queue: reopened}
	restarted.persistSupervisorStatus("stopped")
	if statusWrites != 1 {
		t.Fatalf("parked watcher stop rewrote %d-byte task store: writes=%d, want 1", storeInfo.Size(), statusWrites)
	}
	if err := restarted.deliverQueuedEvent(ev, cursor); !errors.Is(err, errTargetLimitReached) {
		t.Fatalf("parked replay = %v, want replay request", err)
	}
	second, err := task.GetTask("a4223109")
	if err != nil {
		t.Fatalf("reload parked status: %v", err)
	}
	if second.LastRunAt == nil || !second.LastRunAt.Equal(*first.LastRunAt) {
		t.Fatalf("same parked occurrence restamped LastRunAt: first=%v second=%v", first.LastRunAt, second.LastRunAt)
	}
	if second.LastRunStatus != TaskStatusLimitParked {
		t.Fatalf("watcher stop hid parked status: got %q", second.LastRunStatus)
	}
	if statusWrites != 1 {
		t.Fatalf("parked retry rewrote %d-byte task store: writes=%d, want 1 total", storeInfo.Size(), statusWrites)
	}
}

func TestUnattemptedProtectedBacklogStillRecordsWatcherStop(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223111")
	if err := queue.enqueue("not attempted yet", true); err != nil {
		t.Fatalf("seed protected event: %v", err)
	}
	s := newWatcherSupervisor()
	var got string
	s.setStatus = func(_ string, status string) { got = status }
	w := &taskWatcher{taskID: "a4223111", sup: s, queue: queue}
	w.persistSupervisorStatus("stopped")
	if got != "stopped" {
		t.Fatalf("generic retention marker suppressed watcher status: got %q", got)
	}
}

func TestWatchQueueAdmissionProtectsInFlightLimitResume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "resuming-limit", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	manager.setLimitReached(inst, time.Now().Add(time.Hour))
	if err := inst.BeginLimitResume(); err != nil {
		t.Fatalf("BeginLimitResume: %v", err)
	}
	if err := inst.Transition(session.ConfirmLive()); err != nil {
		t.Fatalf("ConfirmLive: %v", err)
	}
	view := inst.LifecycleView()
	if view.Liveness != session.LiveRunning || view.InFlightOp != session.OpRespawning {
		t.Fatalf("resume interleaving = %v/%v, want Running/Respawning", view.Liveness, view.InFlightOp)
	}
	if err := task.AddTask(task.Task{
		ID: "a4223110", Name: "watch-resume-window", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "resuming-limit", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	limited, err := manager.observeTaskTargetLimit("a4223110")
	if err == nil || limited {
		t.Fatalf("in-flight resume observation = limited=%v err=%v, want unknown", limited, err)
	}
	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	w := &taskWatcher{taskID: "a4223110", sup: s}
	if !w.targetLimitRequiresRetention() {
		t.Fatal("in-flight limit resume was treated as clean queue state")
	}
}
