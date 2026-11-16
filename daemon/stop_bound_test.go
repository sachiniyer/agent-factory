package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// #7416, through RunDaemon: every stop path ends inside the stopper's grace.
// The teardown ends at the stop's deadline whatever a step is waiting on, and
// the warm-up and probation exits drain both planes under the same bound as the
// graceful path, naming what they abandon.

// stopWithin is how long after a Shutdown these tests let RunDaemon run. Each
// shortens the budgets it exercises well below it; on master the stuck step
// holds the stop until the test releases it, far past this.
const stopWithin = 4 * time.Second

// boundMargin is what a stop may run past the bound a test configured: the
// Shutdown's round trip, scheduling, and the exit itself. On CI's macOS
// runners the stops below measured about 0.1s over their bounds (#7439).
const boundMargin = 500 * time.Millisecond

// stubContainerSweep keeps the real startup sweeps cheap and side-effect free,
// as TestRunDaemonStopsSandboxTunnelsOnExit does.
func stubContainerSweep(t *testing.T) {
	t.Helper()
	previous := sweepOrphanContainers
	sweepOrphanContainers = func(string, map[string]bool) session.OrphanSweepResult {
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanContainers = previous })
}

// shortenTeardown makes the teardown end d after the stop began.
func shortenTeardown(t *testing.T, d time.Duration) {
	t.Helper()
	previous := stopTeardownBy
	stopTeardownBy = d
	t.Cleanup(func() { stopTeardownBy = previous })
}

// requestShutdown sends Shutdown and waits for RunDaemon to return within
// stopWithin of it.
func requestShutdown(t *testing.T, runExited <-chan struct{}) time.Duration {
	t.Helper()
	asked := time.Now()
	var resp ShutdownResponse
	require.NoError(t, callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp))
	select {
	case <-runExited:
		return time.Since(asked)
	case <-time.After(stopWithin):
		t.Fatalf("RunDaemon was still stopping %s after Shutdown: a stuck step held the stop", stopWithin)
		return 0
	}
}

// TestRunDaemonTeardownEndsAtTheStopDeadline: a teardown step that never
// returns (here the sandbox-tunnel stop, standing in for a tmux close against a
// wedged server) is abandoned at the stop's deadline, named in the log, and the
// daemon exits.
func TestRunDaemonTeardownEndsAtTheStopDeadline(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warn := captureWarnings(t)
	stubContainerSweep(t)
	shortenTeardown(t, time.Second)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() {
		entered <- struct{}{}
		<-release
	}
	// Restored only after the daemon below has exited (cleanups run LIFO).
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	runExited := make(chan struct{})
	go func() {
		defer close(runExited)
		_ = RunDaemon(tcpDisabledConfig())
	}()
	t.Cleanup(func() {
		close(release)
		select {
		case <-runExited:
		case <-time.After(10 * time.Second):
			t.Error("in-process daemon did not exit during cleanup")
		}
	})
	waitForDaemonPhase(t, DaemonPhaseReady)

	took := requestShutdown(t, runExited)
	select {
	case <-entered:
	default:
		t.Fatal("the teardown never ran the tunnel stop")
	}
	require.Less(t, took, time.Second+boundMargin, "the stop must end at its 1s teardown deadline")
	got := warn.String()
	require.Contains(t, got, "the teardown did not finish by the stop deadline", "the log must say the stop gave up on a step")
	require.Contains(t, got, "sandbox tunnels", "the log must name the step the stop gave up on")
}

// holdTasksFileLock takes tasks.json's file lock, as a writer wedged on a full
// disk would hold it, so a task write admitted after this blocks in its own
// WithFileLock. The returned func releases it.
func holdTasksFileLock(t *testing.T) (release func()) {
	t.Helper()
	path, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	held := make(chan struct{})
	unlock := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- config.WithFileLock(path, func() error {
			close(held)
			<-unlock
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-finished:
		t.Fatalf("could not take the tasks.json lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out taking the tasks.json lock")
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(unlock)
		<-finished
	}
}

// TestRunDaemonWarmupExitsDrainBothPlanesUnderTheBound: a Shutdown during the
// instance restore, and one during upgrade probation, used to close both planes
// with no bound. A handler stuck in one (an AddTask blocked on tasks.json's
// lock) held the stop until the stopper's SIGKILL and skipped every later
// defer. Each now drains under drainBudget and names the handler it abandoned.
func TestRunDaemonWarmupExitsDrainBothPlanesUnderTheBound(t *testing.T) {
	// The in-flight registry's name for the stuck write, as the control codec
	// records it: the registered service name, not a guess at it.
	addTaskCall := "control RPC " + controlServiceName + ".AddTask"
	for _, tc := range []struct {
		name string
		// transactionID runs the daemon as an upgrade candidate, parked in
		// probation; empty runs a plain daemon held in its instance restore.
		transactionID string
	}{
		{name: "during the instance restore"},
		{name: "during upgrade probation", transactionID: "txn-7416-probation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			warn := captureWarnings(t)
			stubContainerSweep(t)
			shortenDrainTotal(t, 300*time.Millisecond)
			shortenTeardown(t, time.Second)

			// The restore seam hands the test the daemon's manager, so it can
			// see the stuck write in flight. A plain daemon's restore is held
			// until the test ends, keeping it in warm-up.
			managers := make(chan *Manager, 1)
			holdRestore := make(chan struct{})
			previousRestore := restoreManagerForStartup
			restoreManagerForStartup = func(m *Manager) error {
				managers <- m
				if tc.transactionID == "" {
					<-holdRestore
					return nil
				}
				return m.restoreInstances()
			}

			cfg := config.DefaultConfig()
			cfg.ListenAddr = ""
			runExited := make(chan struct{})
			addDone := make(chan struct{})
			releaseLock := func() {}
			var manager *Manager
			// One cleanup, in order: let the stuck write and the held restore
			// finish, stop a daemon a failure left running, then join the
			// daemon, the client and the abandoned handler, so none outlives
			// this test or writes into its home after it is removed.
			t.Cleanup(func() {
				releaseLock()
				close(holdRestore)
				select {
				case <-runExited:
				default:
					// A failure before the Shutdown below; stop the daemon so
					// the failure reports only its own cause.
					var resp ShutdownResponse
					_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
				}
				for _, ch := range []chan struct{}{runExited, addDone} {
					select {
					case <-ch:
					case <-time.After(10 * time.Second):
						t.Error("an in-process daemon or its stuck client did not exit during cleanup")
					}
				}
				if manager != nil {
					deadline := time.Now().Add(10 * time.Second)
					for strings.Contains(manager.lifecycle.inFlight().describe(time.Now()), addTaskCall) {
						if time.Now().After(deadline) {
							t.Error("the abandoned task write never finished after its lock was released")
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				restoreManagerForStartup = previousRestore
			})
			go func() {
				defer close(runExited)
				_ = runDaemon(cfg, tc.transactionID)
			}()
			select {
			case manager = <-managers:
			case <-time.After(10 * time.Second):
				t.Fatal("the daemon never reached its instance restore")
			}
			if tc.transactionID == "" {
				waitForDaemonPhase(t, DaemonPhaseWarming)
			} else {
				waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
				// Released, the candidate admits task writes while it stays
				// parked in the probation select.
				var release ReleaseUpgradeProbationResponse
				require.NoError(t, callDaemonNoEnsure("ReleaseUpgradeProbation",
					ReleaseUpgradeProbationRequest{TransactionID: tc.transactionID}, &release))
				waitForDaemonPhase(t, DaemonPhaseHandoffPending)
			}

			releaseLock = holdTasksFileLock(t)
			go func() {
				defer close(addDone)
				var resp AddTaskResponse
				_ = callDaemonNoEnsure("AddTask", AddTaskRequest{Task: task.Task{
					ID: "d7416001", Name: "held behind tasks.json's lock", WatchCmd: "true",
				}}, &resp)
			}()
			// Dispatched means admitted: the admission check is the handler's
			// first step, and the stop cannot close admission until well after
			// the Shutdown below is acknowledged.
			waitForReady(t, "the task write to be in flight", func() bool {
				return strings.Contains(manager.lifecycle.inFlight().describe(time.Now()), addTaskCall)
			})

			took := requestShutdown(t, runExited)
			require.Less(t, took, 300*time.Millisecond+boundMargin, "the warm-up exit must end at its 300ms drain bound")
			got := warn.String()
			require.Contains(t, got, "drain daemon control server did not finish within the 300ms drain budget",
				"the warm-up exit must drain the control plane under the bound")
			require.Contains(t, got, addTaskCall+" (running", "the log must name the handler the stop abandoned")
		})
	}
}
