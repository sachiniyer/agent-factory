package daemon

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// #7440: a stop joins what it used to leave running — cron deliveries (a
// scheduled fire, the boot replay and its scan, `af tasks trigger`) and the
// startup sandbox reattach — by its deadline, or enters them in its ledger and
// exits holding the home. A delivery parked behind an attached target stops
// parking as soon as the daemon starts to stop, and work refused or cut short
// by the stop tells its caller so.

// installDeliveryTracker installs tracker as the running daemon's, as runDaemon
// does, and restores the previous one at cleanup.
func installDeliveryTracker(t *testing.T, tracker *joinedRuns) {
	t.Helper()
	previous := taskDeliveries.Swap(tracker)
	t.Cleanup(func() { taskDeliveries.Store(previous) })
}

// TestParkedCronDeliveryStopsParkingWhenTheDaemonStops: a cron fire parked
// behind an attached target returns the stop's abort as soon as the daemon
// starts to stop, rather than sleeping out its poll and waiting for the next
// transport failure.
func TestParkedCronDeliveryStopsParkingWhenTheDaemonStops(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	const poll = 2 * time.Second
	previousPoll := cronDeferPollInterval
	cronDeferPollInterval = poll
	var stopping, ended atomic.Bool
	previousDraining := daemonDrainingFn
	daemonDrainingFn = stopping.Load
	previousDeliver := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		if ended.Load() {
			return taskPromptDeliveryResult{}, errors.New("test over")
		}
		return taskPromptDeliveryResult{status: StatusDeferredAttached}, nil
	}

	parked := &task.Task{ID: "ab7440a1", Prompt: "p", TargetSession: "attached-target", ProjectPath: t.TempDir()}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		var run task.RunKey
		_, err := deliverCronTaskPromptRun(parked, "p", &run)
		done <- err
	}()
	// The delivery must be gone before its seams are restored, whatever this
	// test concludes: the stub ends it within one poll once the test is over.
	t.Cleanup(func() {
		ended.Store(true)
		select {
		case <-exited:
		case <-time.After(3 * poll):
			t.Error("the parked delivery did not end after the test")
		}
		cronDeferPollInterval, daemonDrainingFn, deliverPromptForTask = previousPoll, previousDraining, previousDeliver
	})
	time.Sleep(100 * time.Millisecond)
	stopped := time.Now()
	stopping.Store(true)

	select {
	case err := <-done:
		require.True(t, IsDaemonStoppingErr(err), "a parked delivery must end with the stop's abort, got %v", err)
		require.Less(t, time.Since(stopped), poll/2, "it must stop parking at once, not after its poll")
	case <-time.After(2 * poll):
		t.Fatal("a parked delivery kept parking after the daemon began to stop")
	}
}

// TestTaskDeliveryAfterTheStopClosedItIsRefusedAndWritesNothing: once the stop
// has closed the delivery tracker, a new delivery does nothing: no session, no
// status, no fire marker. Its caller is told the daemon is shutting down.
func TestTaskDeliveryAfterTheStopClosedItIsRefusedAndWritesNothing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab7440b1", repo)
	closed := &joinedRuns{}
	closed.closeAndWait()
	installDeliveryTracker(t, closed)
	created := false
	previousCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		created = true
		return nil, io.ErrUnexpectedEOF
	}
	t.Cleanup(func() { createSessionForTask = previousCreate })

	err := RunTask("ab7440b1", task.ProjectExpectation{})

	require.True(t, IsDaemonStoppingErr(err), "the caller must be told the daemon is shutting down, got %v", err)
	require.Contains(t, err.Error(), "retry after it restarts")
	require.False(t, created, "a refused delivery must not reach a session")
	require.False(t, markerExists(t, "ab7440b1"), "a refused delivery must write no fire marker")
	require.Empty(t, lastRunStatus(t, "ab7440b1"), "a refused delivery must write no status")
}

// TestShutdownAbortOfAnOwedFireSaysItIsReplayed: an untargeted fire cut short
// by the stop keeps its marker, which the next daemon replays, so its caller
// (`af tasks trigger`) is told it will be replayed, not to fire it again.
func TestShutdownAbortOfAnOwedFireSaysItIsReplayed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab7440c1", repo)
	pinDaemonDraining(t, true)
	previousCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, io.ErrUnexpectedEOF
	}
	t.Cleanup(func() { createSessionForTask = previousCreate })

	err := RunTask("ab7440c1", task.ProjectExpectation{})

	require.True(t, IsDaemonStoppingErr(err), "the caller must be told the daemon is shutting down, got %v", err)
	require.Contains(t, err.Error(), "replays it when it next starts")
	require.NotContains(t, err.Error(), "retry after it restarts", "a replayed fire must not invite a second one")
	require.True(t, markerExists(t, "ab7440c1"))
}

// TestReplayScanIsRefusedOnceTheStopClosedTheTracker: the boot replay's scan
// holds a delivery slot of its own, because it writes too: it clears the
// marker of a task that was disabled or deleted. A stop that has closed the
// tracker therefore stops the scan before it clears anything.
func TestReplayScanIsRefusedOnceTheStopClosedTheTracker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(task.Task{
		ID: "ab7440d1", Name: "disabled", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: t.TempDir(), Enabled: false, CreatedAt: time.Now(),
	}))
	seedPendingFire(t, taskFireMarker{TaskID: "ab7440d1", IntendedAt: time.Now().Add(-time.Minute)})
	closed := &joinedRuns{}
	closed.closeAndWait()
	installDeliveryTracker(t, closed)

	replayPendingTaskFires()

	require.True(t, markerExists(t, "ab7440d1"), "the closed-out scan must not clear any marker")
}

// TestTriggerDuringAStopTellsTheCallerToRetryAfterItRestarts: a mutation that
// arrives while the daemon is stopping — `af tasks trigger` among them — is
// refused with "shutting down; retry after it restarts", not the upgrade
// hand-off's "retry shortly", which the client would retry into a daemon it
// starts behind the caller's own stop. A hand-off still refuses as one.
func TestTriggerDuringAStopTellsTheCallerToRetryAfterItRestarts(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	cs := &controlServer{manager: manager, scheduler: newTaskScheduler()}

	manager.lifecycle.markStopping()
	err := cs.TriggerTask(TriggerTaskRequest{ID: "ab7440e1"}, &TriggerTaskResponse{})
	require.EqualError(t, err, "agent-factory daemon is shutting down; retry after it restarts")

	handoff, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	handoff.lifecycle.markQuiescing()
	handoff.lifecycle.markStopping()
	require.True(t, IsDaemonQuiescingErr(handoff.lifecycle.mutationAdmissionError()),
		"an upgrade hand-off's own drain must not turn its refusal into a stop's")
}

// TestStoppingRefusalIsNotRetriedIntoANewDaemon: the client retries an
// admission refusal, and treats a quiescing one as proof of an upgrade
// hand-off, re-ensuring a daemon when the old one goes. A stop is neither, so
// its refusal reaches the caller at once rather than starting a daemon
// behind the caller's own stop.
func TestStoppingRefusalIsNotRetriedIntoANewDaemon(t *testing.T) {
	require.False(t, IsDaemonAdmissionRetryable(errDaemonStopping()), "a stopping daemon's refusal must not be retried")
	require.False(t, IsDaemonQuiescingErr(errDaemonStopping()), "a stop is not an upgrade hand-off")
}

// TestHTTPAnswersAStoppingDaemonsRefusalWith503: over HTTP the stop's refusal
// is a 503, like the other admission refusals, not a 500.
func TestHTTPAnswersAStoppingDaemonsRefusalWith503(t *testing.T) {
	handler := rpcHandler(func(struct{}, *struct{}) error { return errDaemonStopping() })
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodPost, "/v1/tasks/trigger", strings.NewReader("{}")))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

// TestShutdownClassifierCountsTheStoppingRefusal: a delivery whose own RPC the
// stopping daemon refused was interrupted by the stop, so it is an abort that
// keeps its marker, not an error that consumes the occurrence.
func TestShutdownClassifierCountsTheStoppingRefusal(t *testing.T) {
	require.True(t, isShutdownAbortErr(errDaemonStopping()))
}

// holdOneRun takes a slot from tracker for the length of the test, as a
// delivery or a reattach still running would.
func holdOneRun(t *testing.T, tracker func() *joinedRuns) {
	t.Helper()
	done, ok := tracker().begin()
	require.True(t, ok, "the daemon's tracker is open while it runs")
	t.Cleanup(done)
}

// TestRunDaemonJoinsARunningCronDelivery: a delivery still running when the
// daemon stops is joined by the stop's deadline. One that never ends is
// abandoned there and named in the stop's ledger, so the exit holds the home.
func TestRunDaemonJoinsARunningCronDelivery(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	shortenTeardown(t, time.Second)
	captureExitHoldingHome(t, nil)
	managers := captureManager(t)

	runExited := startInProcessDaemon(t, nil)
	manager := <-managers
	waitForDaemonPhase(t, DaemonPhaseReady)
	holdOneRun(t, taskDeliveries.Load)
	requestShutdown(t, runExited)

	require.Contains(t, manager.lifecycle.stopClock().abandonments(), "teardown: cron deliveries")
}

// TestRunDaemonJoinsTheSandboxReattach: the startup sandbox reattach, which
// persists to instances.json, is joined by the stop's deadline too, or
// abandoned and named.
func TestRunDaemonJoinsTheSandboxReattach(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	shortenTeardown(t, time.Second)
	captureExitHoldingHome(t, nil)
	managers := captureManager(t)

	runExited := startInProcessDaemon(t, nil)
	manager := <-managers
	waitForDaemonPhase(t, DaemonPhaseReady)
	holdOneRun(t, func() *joinedRuns { return &manager.reattachRuns })
	requestShutdown(t, runExited)

	require.Contains(t, manager.lifecycle.stopClock().abandonments(), "teardown: sandbox reattach")
}

// TestCronJobLaunchedJustBeforeStopIsAdmittedAndKeepsItsMarker: robfig runs a
// job on its own goroutine, so one the cron launched just before
// scheduler.Stop can reach its delivery after the stop's teardown began. The
// "cron deliveries" step waits for every launched job before it closes the
// tracker, so that occurrence is admitted and records its fire marker for the
// next daemon. Refused at a closed tracker, it would have had no marker yet and
// been lost (#7468 review).
func TestCronJobLaunchedJustBeforeStopIsAdmittedAndKeepsItsMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab7468a1", repo)
	pinDaemonDraining(t, true)
	previousCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, io.ErrUnexpectedEOF
	}
	t.Cleanup(func() { createSessionForTask = previousCreate })
	tracker := &joinedRuns{}
	installDeliveryTracker(t, tracker)

	scheduler := newTaskScheduler()
	launched := make(chan struct{}, 1)
	jobDone := make(chan struct{})
	var once sync.Once
	scheduler.cron.Schedule(cron.Every(time.Second), cron.FuncJob(func() {
		once.Do(func() {
			defer close(jobDone)
			launched <- struct{}{}
			// Launched, but not yet at its delivery when the stop arrives.
			time.Sleep(200 * time.Millisecond)
			_ = RunTask("ab7468a1", task.ProjectExpectation{})
		})
	}))
	scheduler.Start()
	// The job must be done before its seams are restored.
	t.Cleanup(func() {
		scheduler.Stop()
		select {
		case <-jobDone:
		case <-time.After(10 * time.Second):
			t.Error("the cron job did not finish")
		}
	})
	select {
	case <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("the cron never launched the job")
	}

	scheduler.Stop()
	joinCronDeliveries(scheduler, tracker)

	require.True(t, markerExists(t, "ab7468a1"),
		"an occurrence the cron launched before it stopped must be admitted and keep its marker for replay")
	require.Equal(t, taskStatusAbortedShutdown, lastRunStatus(t, "ab7468a1"))
}

// TestShutdownAbortOfAnOwedFireNeverInvitesARetry: a fire the stop cut short
// whose marker is kept is replayed by the next daemon, so its error must not
// tell the caller to retry, even when the delivery failed on the stopping
// daemon's own "retry after it restarts" refusal. A retry and the replay would
// deliver twice. The refusal stays in the error chain (#7468 review).
func TestShutdownAbortOfAnOwedFireNeverInvitesARetry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab7468b1", repo)
	pinDaemonDraining(t, true)
	previousCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, errDaemonStopping()
	}
	t.Cleanup(func() { createSessionForTask = previousCreate })

	err := RunTask("ab7468b1", task.ProjectExpectation{})

	require.True(t, IsDaemonStoppingErr(err), "the caller must be told the daemon is shutting down, got %v", err)
	require.Contains(t, err.Error(), "replays it when it next starts")
	require.NotContains(t, err.Error(), "retry", "a replayed fire's error must never invite another one")
	require.True(t, IsDaemonStoppingErr(errors.Unwrap(err)), "the inner refusal stays in the error chain")
	require.True(t, markerExists(t, "ab7468b1"))
}
