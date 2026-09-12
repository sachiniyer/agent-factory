package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

func TestWatcherStopWaitsForInFlightLimitPublicationBeforeDiscardingPipe(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "stop-during-limit-publication")
	if err := queue.enqueue("delivery-in-flight"); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDelivery) }) }
	t.Cleanup(release)
	s := newWatcherSupervisor()
	s.deliver = func(string, string, watchDeliveryOptions) error {
		close(deliveryStarted)
		<-releaseDelivery
		return errTargetLimitReached
	}
	s.observeTargetLimit = func(string) (bool, error) { return false, nil }
	stopCh := make(chan struct{})
	w := &taskWatcher{
		taskID: "stop-during-limit-publication", sup: s, queue: queue,
		stopCh: stopCh, draining: true,
	}
	w.wg.Add(1)
	go w.drainLoop()
	select {
	case <-deliveryStarted:
	case <-time.After(time.Second):
		t.Fatal("drainer did not start delivery")
	}

	close(stopCh)
	writersStopped := make(chan struct{})
	close(writersStopped)
	readerDone := make(chan struct{})
	go func() {
		w.consumeLines(strings.NewReader("accepted-before-stop\n"), &tailBuffer{}, writersStopped)
		close(readerDone)
	}()

	returnedBeforePublication := false
	select {
	case <-readerDone:
		returnedBeforePublication = true
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("stdout reader stayed blocked after limit publication completed")
	}
	w.wg.Wait()

	if returnedBeforePublication {
		t.Fatal("stdout reader discarded its finite pipe before the in-flight drainer published limit retention")
	}
	if got := queue.pendingCount(); got != 2 {
		t.Fatalf("stop retained %d events, want in-flight head plus accepted pipe event", got)
	}
	if !queue.retainLimitParked() {
		t.Fatal("in-flight limit delivery did not publish durable retention before stop completed")
	}
}

func TestTargetLimitObservationRejectsConcurrentTaskRebind(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "healthy-a", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	limited := registerStarted(t, manager, repoID, repoPath, "limited-b", readyFakeBackend{session.NewFakeBackend()}, true, session.Running)
	manager.setLimitReached(limited, time.Now().Add(time.Hour))
	const taskID = "a4226123"
	if err := task.AddTask(task.Task{
		ID: taskID, Name: "watch-rebound-target", Prompt: "event: {{line}}",
		WatchCmd: "watch.sh", TargetSession: "healthy-a", ProjectPath: repoPath,
		Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	reachedOldTarget := make(chan struct{})
	releaseObservation := make(chan struct{})
	var hookOnce sync.Once
	originalHook := testHookTaskLimitObserveBeforeObservationFence
	testHookTaskLimitObserveBeforeObservationFence = func() {
		hookOnce.Do(func() {
			close(reachedOldTarget)
			<-releaseObservation
		})
	}
	t.Cleanup(func() { testHookTaskLimitObserveBeforeObservationFence = originalHook })

	s := newWatcherSupervisor()
	s.observeTargetLimit = manager.observeTaskTargetLimit
	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{
		taskID: taskID, sup: s, queue: newEventQueue(t.TempDir(), taskID), stopCh: stopCh,
	}
	for i := 0; i < s.eventsPerMinute; i++ {
		w.eventTimes = append(w.eventTimes, time.Now())
	}

	handleDone := make(chan struct{})
	go func() {
		w.handleEvent("must-follow-current-target", &tailBuffer{})
		close(handleDone)
	}()
	select {
	case <-reachedOldTarget:
	case <-time.After(time.Second):
		t.Fatal("limit observation did not snapshot the original target")
	}
	newTarget := "limited-b"
	if _, err := task.UpdateTask(taskID, task.TaskUpdate{TargetSession: &newTarget}, task.ProjectExpectation{}); err != nil {
		close(releaseObservation)
		t.Fatalf("rebind task target: %v", err)
	}
	close(releaseObservation)
	select {
	case <-handleDone:
	case <-time.After(time.Second):
		t.Fatal("event admission stayed blocked after target rebind")
	}

	if got := w.queue.pendingCount(); got != 1 {
		t.Fatalf("event observed against the old target was discarded after rebind: pending=%d", got)
	}
	if w.dropped != 0 {
		t.Fatalf("event for newly limited target was counted as a rate drop: %d", w.dropped)
	}
}

func TestUnreadableQueueBackpressuresUntilStateIsKnown(t *testing.T) {
	dir := t.TempDir()
	queue := newEventQueue(dir, "unknown-limit-state")
	realQueuePath := queue.path
	queue.mu.Lock()
	queue.path = dir // scanning a directory gives the constructor's unreadable-file state
	queue.mu.Unlock()
	queue.load()
	if !queue.loadFailed() {
		t.Fatal("fixture did not make queue state unreadable")
	}

	oldPoll := watcherLimitBackpressurePoll
	watcherLimitBackpressurePoll = time.Millisecond
	t.Cleanup(func() { watcherLimitBackpressurePoll = oldPoll })
	storageReady := make(chan struct{})
	attemptedBeforeRecovery := make(chan struct{}, 1)
	delivered := make(chan string, 1)
	s := newWatcherSupervisor()
	s.deliver = func(_ string, line string, _ watchDeliveryOptions) error {
		select {
		case <-storageReady:
			delivered <- line
			return nil
		default:
			select {
			case attemptedBeforeRecovery <- struct{}{}:
			default:
			}
			return errors.New("delivery unavailable while queue state is unknown")
		}
	}
	stopCh := make(chan struct{})
	w := &taskWatcher{
		taskID: "unknown-limit-state", sup: s, queue: queue, stopCh: stopCh,
	}
	readerDone := make(chan struct{})
	go func() {
		w.consumeLines(strings.NewReader("must-survive-unreadable-state\n"), &tailBuffer{}, make(chan struct{}))
		close(readerDone)
	}()

	earlyAttempt := false
	select {
	case <-attemptedBeforeRecovery:
		earlyAttempt = true
	case <-time.After(50 * time.Millisecond):
	}
	queue.mu.Lock()
	queue.path = realQueuePath
	queue.mu.Unlock()
	queue.load()
	close(storageReady)

	if !earlyAttempt {
		select {
		case got := <-delivered:
			if got != "must-survive-unreadable-state" {
				t.Fatalf("delivered line = %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("reader did not resume after queue state recovered")
		}
	}
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		close(stopCh)
		t.Fatal("stdout reader did not finish")
	}
	if earlyAttempt {
		t.Fatal("unreadable queue state admitted a delivery whose failed enqueue discarded the event")
	}
	if queue.loadFailed() {
		t.Fatal("queue did not recover to a known state")
	}
	if got := queue.pendingCount(); got != 0 {
		t.Fatalf("queue pending after successful delivery = %d", got)
	}
}
