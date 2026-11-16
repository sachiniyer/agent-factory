package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// #7392: a daemon's own stop must never spend a watch task's crash-loop budget.
// On 11-15 the outgoing daemon's drain hung for 90s (#7393) while its watch
// loops kept running: systemd's SIGTERM to each bound scope counted as failure
// 1, and every restart into systemd's refusal ("… has 'stop' job queued")
// counted as the next, until both watch tasks gave up inside the daemon's stop.

// scopeRefusalLine is what systemd-run prints, and all it prints, when the user
// manager refuses a scope bound to the daemon's unit because that unit has a
// stop job — verbatim from the box's log on 11-15.
const scopeRefusalLine = "Failed to start transient scope unit: Transaction for run-r7a9f85945db345d7bd8f22a7c72af96c.scope/start " +
	"is destructive (agent-factory-daemon.service has 'stop' job queued, but 'start' is included in transaction)."

// refusedRunScript stands in for a refused systemd-run: it records the attempt,
// prints systemd-run's refusal on stderr, and exits 1 having run nothing else.
func refusedRunScript(countFile string) string {
	return fmt.Sprintf(`echo run >> %q; printf '%%s\n' %q >&2; exit 1`, countFile, scopeRefusalLine)
}

// countingFailScript records each run, then exits with code.
func countingFailScript(countFile string, code int) string {
	return fmt.Sprintf(`echo run >> %q; exit %d`, countFile, code)
}

func countRuns(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), "run\n")
}

// spinningWatch reports its shell's PID as its one event, then idles.
func spinningWatch(t *testing.T, s *watcherSupervisor, rec *watchRecorder, id string) int {
	t.Helper()
	require.NoError(t, s.Reload())
	waitUntil(t, 5*time.Second, "the watch command to report its PID", func() bool {
		return len(rec.eventsSnapshot()) == 1
	})
	pid, err := strconv.Atoi(strings.TrimPrefix(rec.eventsSnapshot()[0], id+":"))
	require.NoError(t, err, "event is not a PID")
	return pid
}

func spinScript() string {
	return `echo $$; ` + testguard.BoundedSpin(100*time.Millisecond, 5*time.Minute)
}

// onWatchGrace installs fn as the lifecycle-grace hook for one task's watcher.
// Call it BEFORE newTestSupervisor: cleanups run last in, first out, so the
// restore then runs only after the supervisor's Stop has joined every watcher
// goroutine that reads the hook.
func onWatchGrace(t *testing.T, taskID string, fn func(grace lifecycleGrace)) {
	t.Helper()
	prev := watchGraceEntered
	watchGraceEntered = func(id string, grace lifecycleGrace) {
		if id == taskID {
			fn(grace)
		}
	}
	t.Cleanup(func() { watchGraceEntered = prev })
}

// latchInGrace latches the daemon's stop from inside the named grace, so the
// stop lands in exactly the branch under test (#7404). sup is read when the
// grace is entered, after the test has built the supervisor.
func latchInGrace(t *testing.T, taskID string, want lifecycleGrace, sup func() *watcherSupervisor) {
	t.Helper()
	onWatchGrace(t, taskID, func(grace lifecycleGrace) {
		if grace == want {
			sup().beginShutdown()
		}
	})
}

// requireStoodDown asserts the watcher returned without restarting, logging a
// failure, tripping the breaker, or persisting any status.
func requireStoodDown(t *testing.T, s *watcherSupervisor, rec *watchRecorder, warn, errBuf *logCapture, wantEvents int) {
	t.Helper()
	waitUntil(t, 3*time.Second, "the watcher to stand down instead of restarting", func() bool {
		return len(s.watchingTaskIDs()) == 0
	})
	require.NotContains(t, warn.String(), "watch command failed", "an exit caused by the daemon's stop was logged as a failure")
	require.NotContains(t, errBuf.String(), "giving up", "the daemon's stop tripped the crash-loop breaker")
	require.Empty(t, rec.statusesSnapshot(), "the daemon's stop persisted a status")
	require.Len(t, rec.eventsSnapshot(), wantEvents, "the stopping daemon restarted the watch command")
}

// TestDrainStandsWatchersDownBeforeItCanHang is the 11-15 failure 1 end to
// end: the drain hangs in its control-plane join, and meanwhile systemd's stop
// of the unit SIGTERMs the watch's bound scope. The watcher must already have
// been told the daemon is stopping, so it neither counts the kill nor restarts —
// not wait for the deferred watchers.Stop that a hung drain never reaches.
func TestDrainStandsWatchersDownBeforeItCanHang(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	warn, errBuf := captureWatcherLogs(t)
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392001", spinScript(), t.TempDir())))
	pid := spinningWatch(t, s, rec, "d7392001")

	hung := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var workers sync.WaitGroup
	controlClosed := false
	go func() {
		defer close(drained)
		drainDaemon(manager, s, nil, func() error {
			close(hung)
			<-release
			return nil
		}, &controlClosed, make(chan struct{}), &workers)
	}()
	t.Cleanup(func() {
		close(release)
		<-drained
	})
	select {
	case <-hung:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never reached its control-plane join")
	}

	require.NoError(t, syscall.Kill(pid, syscall.SIGTERM), "SIGTERM the watch command as systemd's scope stop does")
	requireStoodDown(t, s, rec, warn, errBuf, 1)
}

// TestWatcherSigtermJustBeforeTheDaemonsStopIsNotAFailure covers systemd's
// ordering: the bound scope is stopped first, so the watch command dies of
// SIGTERM a beat before the daemon is signalled — and the daemon's drain may
// then never reach Stop. The SIGTERM grace must end on the shutdown latch. The
// latch lands inside that grace (watchGraceEntered), so the branch under test
// is the one that sees it, and its own log line says so (#7404).
func TestWatcherSigtermJustBeforeTheDaemonsStopIsNotAFailure(t *testing.T) {
	warn, errBuf := captureWatcherLogs(t)
	info := captureInfo(t)
	var s *watcherSupervisor
	latchInGrace(t, "d7392002", graceSigterm, func() *watcherSupervisor { return s })
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392002", spinScript(), t.TempDir())))
	s.stopGrace = watcherStopGrace
	pid := spinningWatch(t, s, rec, "d7392002")

	require.NoError(t, syscall.Kill(pid, syscall.SIGTERM), "SIGTERM the watch command as systemd's scope stop does")
	requireStoodDown(t, s, rec, warn, errBuf, 1)
	require.Contains(t, info.String(), "watch task d7392002: watch command was terminated as the daemon began stopping; not a failure")
}

// TestWatcherScopeRefusedForTheDaemonsStopIsNotAFailure is 11-15's failures
// 2–5: a restart while the daemon's unit has a stop job is refused by systemd,
// and that refusal is the daemon's stop, not the script's failure. It is not
// counted, and the stopping daemon does not try again. The latch lands inside
// the refusal grace, never before the run has exited (#7404).
func TestWatcherScopeRefusedForTheDaemonsStopIsNotAFailure(t *testing.T) {
	warn, errBuf := captureWatcherLogs(t)
	info := captureInfo(t)
	countFile := filepath.Join(t.TempDir(), "runs")
	var s *watcherSupervisor
	latchInGrace(t, "d7392003", graceScopeRefused, func() *watcherSupervisor { return s })
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392003", refusedRunScript(countFile), t.TempDir())))
	s.stopGrace = watcherStopGrace
	require.NoError(t, s.Reload())

	requireStoodDown(t, s, rec, warn, errBuf, 0)
	require.Equal(t, 1, countRuns(t, countFile), "the stopping daemon retried a refused watch command")
	require.Contains(t, info.String(), "watch task d7392003: systemd refused the watch command's scope because the daemon's unit is stopping; not a failure")
}

// TestWatcherScopeRefusalWithoutADaemonStopStillGivesUp is the classifier's
// guard: the refusal text is a hint, never an exemption. With no daemon stop
// behind it, it fails, and gives up, exactly like any other failing command.
func TestWatcherScopeRefusalWithoutADaemonStopStillGivesUp(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "runs")
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392004", refusedRunScript(countFile), t.TempDir())))
	s.stopGrace = 50 * time.Millisecond
	require.NoError(t, s.Reload())
	waitUntil(t, 10*time.Second, "the crash-loop breaker to trip", func() bool {
		statuses := rec.statusesSnapshot()
		return len(statuses) > 0 && strings.HasPrefix(statuses[len(statuses)-1], "d7392004:errored")
	})
	statuses := rec.statusesSnapshot()
	last := statuses[len(statuses)-1]
	require.True(t, strings.HasPrefix(last, "d7392004:errored: gave up after 5 failures in 10m0s"), "breaker status = %q", last)
	require.Contains(t, last, "Failed to start transient scope unit", "the give-up names the refusal")
	require.Equal(t, s.crashMaxExits, countRuns(t, countFile))
}

// TestWatcherStoppingDaemonDoesNotRestartAFailedCommand: a stopping daemon
// restarts nothing, whatever the exit — the backoff wait ends on the latch.
func TestWatcherStoppingDaemonDoesNotRestartAFailedCommand(t *testing.T) {
	warn := captureWarnings(t)
	countFile := filepath.Join(t.TempDir(), "runs")
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392005", countingFailScript(countFile, 7), t.TempDir())))
	s.baseBackoff = time.Second
	require.NoError(t, s.Reload())
	waitUntil(t, 5*time.Second, "the failure to enter its backoff", func() bool {
		return strings.Contains(warn.String(), "watch task d7392005: watch command failed (exit status 7); restarting in")
	})

	s.beginShutdown()
	waitUntil(t, 3*time.Second, "the watcher to stand down instead of restarting", func() bool {
		return len(s.watchingTaskIDs()) == 0
	})
	require.Equal(t, 1, countRuns(t, countFile), "the stopping daemon restarted a failed watch command")
	require.Empty(t, rec.statusesSnapshot())
}

// TestWatcherBreakerDoesNotTripAsTheDaemonStops: an exit the daemon cannot yet
// attribute to its stop (a script that traps TERM and exits 1) may still be
// counted, but it must not be what trips the breaker — the trip is durable, and
// it would outlive a stop that caused it. The latch lands inside the backstop's
// grace, at the fifth failure (#7404).
func TestWatcherBreakerDoesNotTripAsTheDaemonStops(t *testing.T) {
	errBuf := captureErrors(t)
	info := captureInfo(t)
	countFile := filepath.Join(t.TempDir(), "runs")
	var s *watcherSupervisor
	latchInGrace(t, "d7392006", graceBreaker, func() *watcherSupervisor { return s })
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392006", countingFailScript(countFile, 1), t.TempDir())))
	s.stopGrace = watcherStopGrace
	require.NoError(t, s.Reload())

	waitUntil(t, 10*time.Second, "the watcher to stand down at its fifth failure", func() bool { return len(s.watchingTaskIDs()) == 0 })
	require.Equal(t, s.crashMaxExits, countRuns(t, countFile))
	require.NotContains(t, errBuf.String(), "giving up", "the breaker tripped as the daemon stopped")
	require.Empty(t, rec.statusesSnapshot(), "a give-up was persisted as the daemon stopped")
	require.Contains(t, info.String(), "watch task d7392006: watch command failed (exit status 1) as the daemon began stopping; not giving up")
}

// TestWatcherCleanExitAsTheDaemonStopsRecordsNothingAndReturnsAtOnce: a script
// that handles the scope's SIGTERM by exiting 0 must not leave a durable
// "stopped" behind, and its grace must end the instant the daemon's stop
// arrives, so a watcher in it never holds Stop for the 5s grace (#7404).
func TestWatcherCleanExitAsTheDaemonStopsRecordsNothingAndReturnsAtOnce(t *testing.T) {
	info := captureInfo(t)
	countFile := filepath.Join(t.TempDir(), "runs")
	entered := make(chan struct{}, 1)
	onWatchGrace(t, "d7392009", func(grace lifecycleGrace) {
		if grace == graceCleanExit {
			entered <- struct{}{}
		}
	})
	s, rec := newTestSupervisor(t, staticTasks(watchTask("d7392009", countingFailScript(countFile, 0), t.TempDir())))
	s.stopGrace = watcherStopGrace // the production grace: the stop must not wait it out
	require.NoError(t, s.Reload())
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean exit never entered its grace")
	}

	start := time.Now()
	s.beginShutdown()
	waitUntil(t, 2*time.Second, "the watcher to return", func() bool { return len(s.watchingTaskIDs()) == 0 })
	require.Less(t, time.Since(start), 500*time.Millisecond, "the clean-exit grace waited out its timer instead of ending on the daemon's stop")
	require.Empty(t, rec.statusesSnapshot(), "a stop-caused exit 0 was recorded as stopped")
	require.Contains(t, info.String(), "watch task d7392009: watch command exited cleanly as the daemon began stopping; not recording it as stopped")
}

// TestAwaitDaemonStopSaysWhichStopArrived: the grace reports a reload or
// disable of the task apart from the daemon's own stop, so the log names the
// one that happened (#7404's nit).
func TestAwaitDaemonStopSaysWhichStopArrived(t *testing.T) {
	newWatcher := func(grace time.Duration) *taskWatcher {
		s := &watcherSupervisor{stopGrace: grace}
		return &taskWatcher{taskID: "d7392010", sup: s, stopCh: make(chan struct{})}
	}

	w := newWatcher(time.Minute)
	close(w.stopCh)
	require.Equal(t, watcherStopArrived, w.awaitDaemonStop(graceBreaker))

	w = newWatcher(time.Minute)
	w.sup.beginShutdown()
	require.Equal(t, daemonStopArrived, w.awaitDaemonStop(graceBreaker))

	w = newWatcher(20 * time.Millisecond)
	require.Equal(t, noStopArrived, w.awaitDaemonStop(graceBreaker))
}

// TestWatcherSupervisorReadsAsStoppingOnceShutdownBegins: from the latch, the
// supervisor arms nothing new and reports arming as unknown — watchers standing
// down for a stop are not dead watches.
func TestWatcherSupervisorReadsAsStoppingOnceShutdownBegins(t *testing.T) {
	tk := watchTask("d7392007", spinScript(), t.TempDir())
	s, rec := newTestSupervisor(t, staticTasks(tk))
	spinningWatch(t, s, rec, tk.ID)
	require.Equal(t, task.ArmingArmed, s.armingFor(tk))

	s.beginShutdown()
	require.Equal(t, task.ArmingUnknown, s.armingFor(tk), "a stopping daemon reported its watch as not armed")
	require.Error(t, s.Reload(), "a stopping supervisor armed watchers")
}

// TestDaemonStartArmsAWatchTheLastGenerationGaveUpOn guards the other half of
// #7392's property: a give-up is durable only within its daemon. A fresh
// daemon start re-arms every enabled watch, whatever status the previous
// generation persisted — on 11-15 it did, at 18:40:27.
func TestDaemonStartArmsAWatchTheLastGenerationGaveUpOn(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	gaveUp := watchTask("d7392008", `echo armed; `+testguard.BoundedSpin(100*time.Millisecond, 5*time.Minute), repoPath)
	require.NoError(t, task.AddTask(gaveUp))
	_, err := task.UpdateTaskStatus(gaveUp.ID, nil, "errored: gave up after 5 failures in 10m0s (last: exit status 1): "+scopeRefusalLine)
	require.NoError(t, err)
	stored, err := task.GetTask(gaveUp.ID)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(stored.LastRunStatus, "errored: gave up"), "fixture: the previous generation's give-up is on record")

	watchers, rec := newTestSupervisor(t, task.LoadTasks)
	require.NoError(t, armTaskAutomation(manager, newTaskScheduler(), watchers))
	waitUntil(t, 5*time.Second, "the re-armed watch to run", func() bool {
		return len(rec.eventsSnapshot()) == 1
	})
	require.Equal(t, task.ArmingArmed, watchers.armingFor(*stored))
}
