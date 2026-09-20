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

	"github.com/sachiniyer/agent-factory/config"
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
	createSessionForTask = func(req CreateSessionRequest) (*session.InstanceData, error) {
		creates++
		// What a real create returns for a parked task session, not just a title:
		// the run identity its status write is addressed by (#4222) — the admitted
		// generation, the stable session id, and the admission sequence. Without
		// them the write names no row and is refused, and the park goes unrecorded.
		return &session.InstanceData{
			ID: "parked-run-id", Title: "parked-run",
			TaskID: req.TaskID, TaskGenerationID: req.TaskGenerationID,
			TaskRunSequence: 1, CreatedAt: time.Now(),
			Liveness: session.LiveLimitReached,
		}, nil
	}
	t.Cleanup(func() { createSessionForTask = original })

	if err := deliverWatchEvent("a4223101", taskGenerationForTest(t, "a4223101"), "issue 4223"); err != nil {
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

	if err := deliverWatchEvent("a4223104", taskGenerationForTest(t, "a4223104"), "issue 4223"); err != nil {
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
	w := &taskWatcher{taskID: "a4223107", generationID: taskGenerationForTest(t, "a4223107"), sup: s, queue: queue, stopCh: stopCh}

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

type blockingTaskPromptBackend struct {
	readyFakeBackend
	recorder *promptRecorder
	started  chan struct{}
	release  chan struct{}
}

func (b *blockingTaskPromptBackend) SendPromptCommandWithStatus(_ *session.Instance, prompt string) (session.PromptDeliveryStatus, error) {
	close(b.started)
	<-b.release
	b.recorder.add(prompt)
	return session.PromptDelivered, nil
}

type clearingLimitSnapshotBackend struct {
	readyFakeBackend
	recorder *promptRecorder
	started  chan struct{}
	release  chan struct{}
}

func (b *clearingLimitSnapshotBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	close(b.started)
	<-b.release
	return false, false, "ready\n❯"
}

func (b *clearingLimitSnapshotBackend) SendPromptCommandWithStatus(_ *session.Instance, prompt string) (session.PromptDeliveryStatus, error) {
	b.recorder.add(prompt)
	return session.PromptDelivered, nil
}

func TestTaskDeliveryWaitsForHealthySnapshotToClearLimit(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	recorder := &promptRecorder{}
	backend := &clearingLimitSnapshotBackend{
		readyFakeBackend: readyFakeBackend{session.NewFakeBackend()},
		recorder:         recorder,
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "clearing-limit", backend, true, session.Running)
	manager.setLimitReached(inst, time.Now().Add(time.Hour))

	pollDone := make(chan struct{})
	go func() {
		manager.refreshInstanceStatus(repoID, inst)
		close(pollDone)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("status poll did not enter its healthy snapshot")
	}

	type result struct {
		status string
		err    error
	}
	deliveryDone := make(chan result, 1)
	go func() {
		status, _, _, err := manager.deliverPromptWithOutcome(DeliverPromptRequest{
			Title: "clearing-limit", RepoPath: repoPath, Program: "claude",
			Prompt: "scheduled", TaskRepoID: repoID, TaskOrigin: true,
		})
		deliveryDone <- result{status: status, err: err}
	}()
	var early *result
	select {
	case got := <-deliveryDone:
		early = &got
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.release)
	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("status poll did not publish its healthy snapshot")
	}
	if early != nil {
		t.Fatalf("task delivery skipped before its in-flight healthy snapshot settled: status=%q err=%v", early.status, early.err)
	}
	select {
	case got := <-deliveryDone:
		if got.err != nil || got.status != "sent" {
			t.Fatalf("delivery after healthy settlement = status %q err %v, want sent", got.status, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("task delivery stayed blocked after healthy snapshot settlement")
	}
	if got := recorder.snapshot(); len(got) != 1 || got[0] != "scheduled" {
		t.Fatalf("healthy target received prompts %v, want [scheduled]", got)
	}
}

// heldTransportSafetyTimeout bounds each wait in
// TestSlowTaskPromptDoesNotBlockAnUnrelatedTarget. It is not a latency budget,
// and missing it asserts nothing about ordering: its only job is to stop a
// regression from hanging CI until the package timeout.
const heldTransportSafetyTimeout = 10 * time.Second

// receiveWithin receives from ch, reporting false if d elapses first.
func receiveWithin[T any](ch <-chan T, d time.Duration) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}

func TestSlowTaskPromptDoesNotBlockAnUnrelatedTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	firstRecorder := &promptRecorder{}
	firstBackend := &blockingTaskPromptBackend{
		readyFakeBackend: readyFakeBackend{session.NewFakeBackend()},
		recorder:         firstRecorder,
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	registerStarted(t, manager, repoID, repoPath, "slow-target", firstBackend, true, session.Running)
	independentPath := setupTaskRepo(t)
	independentRepo, err := config.RepoFromPath(independentPath)
	if err != nil {
		t.Fatalf("independent RepoFromPath: %v", err)
	}
	secondRecorder := &promptRecorder{}
	registerStarted(t, manager, independentRepo.ID, independentPath, "independent-target", recordingBackend{
		readyFakeBackend{session.NewFakeBackend()}, secondRecorder,
	}, true, session.Running)
	limitPath := setupTaskRepo(t)
	limitRepo, err := config.RepoFromPath(limitPath)
	if err != nil {
		t.Fatalf("limit RepoFromPath: %v", err)
	}
	limitTarget := registerStarted(t, manager, limitRepo.ID, limitPath, "limit-target",
		readyFakeBackend{session.NewFakeBackend()}, true, session.Running)

	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "slow-target", RepoID: repoID, Prompt: "first", TaskOrigin: true,
		})
		firstDone <- err
	}()
	select {
	case <-firstBackend.started:
	case <-time.After(heldTransportSafetyTimeout):
		t.Fatalf("timed out after %v waiting for the first prompt to reach its blocking transport", heldTransportSafetyTimeout)
	}
	limitDone := make(chan struct{})
	go func() {
		manager.setLimitReached(limitTarget, time.Now().Add(time.Hour))
		close(limitDone)
	}()

	secondDone := make(chan error, 1)
	go func() {
		_, err := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "independent-target", RepoID: independentRepo.ID, Prompt: "second", TaskOrigin: true,
		})
		secondDone <- err
	}()
	// firstBackend.release is still open, so the first target's transport I/O is
	// demonstrably in progress until the close below. A limit publication or
	// prompt serialized behind that I/O cannot finish before then, however long it
	// is given; an independent one finishes. Finishing inside this window is the
	// proof of independence, so neither wait carries a latency budget, and missing
	// the safety timeout is reported as a timeout, not as serialization (#4608: a
	// 100ms budget read a slow arm64 runner as serialized).
	_, limitWhileHeld := receiveWithin(limitDone, heldTransportSafetyTimeout)
	secondErr, secondWhileHeld := receiveWithin(secondDone, heldTransportSafetyTimeout)
	close(firstBackend.release)
	// Unwind every outstanding call before reporting, so none outlives the test.
	firstErr, firstReturned := receiveWithin(firstDone, heldTransportSafetyTimeout)
	if !limitWhileHeld {
		receiveWithin(limitDone, heldTransportSafetyTimeout)
	}
	if !secondWhileHeld {
		receiveWithin(secondDone, heldTransportSafetyTimeout)
	}
	if !firstReturned {
		t.Fatalf("timed out after %v waiting for the first prompt once its transport was released", heldTransportSafetyTimeout)
	}
	if firstErr != nil {
		t.Fatalf("first prompt failed after release: %v", firstErr)
	}
	if !limitWhileHeld {
		t.Errorf("timed out after %v waiting for the unrelated limit publication while slow-target's transport was held open", heldTransportSafetyTimeout)
	}
	if !secondWhileHeld {
		t.Errorf("timed out after %v waiting for the unrelated task prompt while slow-target's transport was held open", heldTransportSafetyTimeout)
	} else if secondErr != nil {
		t.Errorf("unrelated prompt failed: %v", secondErr)
	}
	if t.Failed() {
		return
	}
	if got := limitTarget.GetLiveness(); got != session.LiveLimitReached {
		t.Fatalf("unrelated limit publication left liveness %v, want LimitReached", got)
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
	w := &taskWatcher{taskID: "a4223102", generationID: taskGenerationForTest(t, "a4223102"), sup: s, queue: queue, stopCh: stopCh}
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
		taskID: "a4223110", generationID: taskGenerationForTest(t, "a4223110"), sup: s, queue: newEventQueue(t.TempDir(), "a4223110"), stopCh: stopCh,
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

// TestExitedWriterPipeStagesUntilQueueHasRoom is the #4226 review finding: a
// command that exits while the protected queue is full must leave its finite
// kernel pipe as the staging buffer. Draining it on exit appends past the cap
// once per restart — a command that repeatedly exits after crashWindow grows
// the durable backlog without bound. The staged lines land at queue pace once
// replay makes room.
func TestExitedWriterPipeStagesUntilQueueHasRoom(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "a4223112")
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
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	writersStopped := make(chan struct{})
	close(writersStopped)

	oldPoll := watcherLimitBackpressurePoll
	watcherLimitBackpressurePoll = time.Millisecond
	t.Cleanup(func() { watcherLimitBackpressurePoll = oldPoll })

	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "a4223112", sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh}
	done := make(chan struct{})
	go func() {
		w.consumeLines(reader, &tailBuffer{}, writersStopped)
		close(done)
	}()
	t.Cleanup(func() {
		close(stopCh)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("reader did not finish after stop")
		}
	})

	// The exited writer's finite pipe is staging, not a drain: the backlog
	// must not grow while the queue is full.
	select {
	case <-done:
		t.Fatal("reader drained an exited writer's pipe past the queue cap")
	case <-time.After(50 * time.Millisecond):
	}
	if got := queue.pendingCount(); got != watcherQueueMaxEvents {
		t.Fatalf("exited writer's pipe drained past the cap: pending=%d", got)
	}

	// Replay makes room; the staged lines land at queue pace and the reader
	// reaches the pipe's real EOF.
	for i := 0; i < 3; i++ {
		_, cursor, ok, err := queue.peek()
		if err != nil || !ok {
			t.Fatalf("peek %d while making room: ok=%v err=%v", i, ok, err)
		}
		advanceEventQueue(t, queue, cursor)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader stayed blocked after replay made room")
	}
	got := drainAllEvents(t, queue)
	if len(got) != watcherQueueMaxEvents-1 ||
		got[len(got)-2] != "kernel-one" || got[len(got)-1] != "kernel-two" {
		t.Fatalf("staged pipe events did not land in order at queue pace: %v", got[len(got)-3:])
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
	s.deliver = adaptWatchDelivery(func(string, string, string) error { return errTargetLimitReached })
	s.queueMaxAge = 72 * time.Hour
	s.drainBaseBackoff = time.Hour
	stopCh := make(chan struct{})
	w := &taskWatcher{taskID: "a4223106", generationID: taskGenerationForTest(t, "a4223106"), sup: s, queue: queue, stopCh: stopCh, draining: true}
	w.wg.Add(1)
	delivered := make(chan struct{})
	originalDeliver := s.deliver
	s.deliver = func(taskID, generationID, line string, options watchDeliveryOptions) error {
		close(delivered)
		return originalDeliver(taskID, generationID, line, options)
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
	updateWatchTaskStatus = func(taskID, generationID string, at *time.Time, status string) (task.Task, bool, error) {
		statusWrites++
		return originalUpdate(taskID, generationID, at, status)
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
	w := &taskWatcher{taskID: "a4223109", generationID: taskGenerationForTest(t, "a4223109"), sup: s, queue: queue}
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
	restarted := &taskWatcher{taskID: "a4223109", generationID: taskGenerationForTest(t, "a4223109"), sup: s, queue: reopened}
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
	s.setStatus = func(_, _ string, status string) { got = status }
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
	w := &taskWatcher{taskID: "a4223110", generationID: taskGenerationForTest(t, "a4223110"), sup: s}
	if !w.targetLimitRequiresRetention() {
		t.Fatal("in-flight limit resume was treated as clean queue state")
	}
}
