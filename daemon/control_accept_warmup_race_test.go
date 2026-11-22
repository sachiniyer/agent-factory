package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// These tests pin the contract that every non-terminal warm-up exit path in
// runDaemon consults the recorded fatal accept error (fatalAcceptShutdownErr)
// before choosing the exit code. The co-ready sigChan arm of the restore and
// probation selects used to return a bare nil, so a fatal accept error that had
// already been recorded — and was racing a buffered SIGTERM in the very same
// select — could be demoted to exit 0 by Go's uniform-random arm selection,
// defeating the fatal-accept mechanism's purpose (the supervisor never restarts
// a clean exit). The fix makes the signal and shutdown arms return
// fatalAcceptShutdownErr(), so the recorded error is never forgotten regardless
// of which arm wins a co-ready race.
//
// The throwawayExpireCh arms are the exception: a throwaway sandbox whose
// lifetime has ended is an intentional clean terminal exit (the main path's
// throwawayFired() gate also returns nil after teardown), so those arms keep the
// clean nil return even with a recorded fatal accept error — relaunching an
// expired sandbox under Restart=on-failure would loop against its marker (#7620).
//
// holdHomeIfAbandoned likewise used to classify the exit code only from the
// run's own error/panic; when err == nil it exited 0 even with a pending fatal
// accept error. The fix adds a default branch that consults
// fatalAcceptShutdownErr and exits 1 when one is recorded, except for an
// intentional clean terminal exit (a deleted home or a throwaway expiry) passed
// as cleanExit, which must stay 0 so the supervisor does not relaunch the daemon
// into a deleted home or against an expired throwaway marker.

// waitSocketBound polls the control socket until it answers a Ping or the
// deadline elapses. The binding is the first observable event strictly after
// stopStartupSignalWatch, so a signal sent after this point buffers in sigChan
// for the warm-up select rather than being re-raised by the startup watcher.
func waitSocketBound(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := pingDaemon(); err == nil {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("control socket never bound: %s", msg)
}

// holdRestoreAndStart boots a plain runDaemon with a gated restore so the main
// flow parks in the restore select. It returns the restore-started channel
// (signalling the main flow is about to reach the select), the hold gate
// (released in cleanup or by the test to let the restore finish), and the
// runDaemon error channel. The cleanup closes the hold gate and joins the
// daemon if the test has not already done so.
func holdRestoreAndStart(t *testing.T) (<-chan struct{}, chan struct{}, <-chan error) {
	t.Helper()
	restoreStarted := make(chan struct{}, 1)
	holdRestore := make(chan struct{})
	previousRestore := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		select {
		case restoreStarted <- struct{}{}:
		default:
		}
		<-holdRestore
		return nil
	}
	runExited := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		defer close(runExited)
		runErr <- runDaemon(tcpDisabledConfig(), "")
	}()
	t.Cleanup(func() {
		restoreManagerForStartup = previousRestore
		select {
		case <-holdRestore:
		default:
			close(holdRestore)
		}
		select {
		case <-runExited:
		default:
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			select {
			case <-runExited:
			case <-time.After(10 * time.Second):
				t.Error("in-process daemon did not exit during cleanup")
			}
		}
	})
	return restoreStarted, holdRestore, runErr
}

// startProbationDaemon boots runDaemon as an upgrade candidate and returns the
// error channel. The cleanup stops the daemon if the test has not already done
// so.
func startProbationDaemon(t *testing.T, transactionID string) <-chan error {
	t.Helper()
	runExited := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		defer close(runExited)
		runErr <- runDaemon(tcpDisabledConfig(), transactionID)
	}()
	t.Cleanup(func() {
		select {
		case <-runExited:
		default:
			var resp ShutdownResponse
			_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
			select {
			case <-runExited:
			case <-time.After(10 * time.Second):
				t.Error("in-process daemon did not exit during cleanup")
			}
		}
	})
	return runErr
}

// TestWarmupRestoreSelectSignalArmReturnsFatalAcceptErr forces the restore
// select to take the sigChan arm while a fatal accept error is recorded, and
// asserts runDaemon returns that error. Only sigChan is ready (shutdownCh is
// not closed, the restore is held, throwawayExpireCh is not closed), so the
// select deterministically takes the signal arm — the arm that used to return
// nil, suppressing the supervisor restart.
func TestWarmupRestoreSelectSignalArmReturnsFatalAcceptErr(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	captureExitHoldingHome(t, nil)

	fatalErr := errors.New("simulated fatal accept error (restore-signal arm)")
	restoreStarted, _, runErr := holdRestoreAndStart(t)

	waitSocketBound(t, "the daemon never reached the restore select")
	select {
	case <-restoreStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore goroutine never started; the daemon is not parked in the restore select")
	}
	// Let the main flow reach the select after launching the restore goroutine.
	time.Sleep(50 * time.Millisecond)

	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case err := <-runErr:
		require.ErrorIs(t, err, fatalErr,
			"the restore-select sigChan arm must return the recorded fatal accept error, not nil (exit 0)")
	case <-time.After(15 * time.Second):
		t.Fatal("runDaemon did not exit after SIGTERM during the instance restore")
	}
}

// TestWarmupProbationSelectSignalArmReturnsFatalAcceptErr forces the probation
// select to take the sigChan arm while a fatal accept error is recorded. The
// restore completes normally (empty home), the daemon parks in the probation
// select, and only sigChan is ready so the select deterministically takes the
// signal arm.
func TestWarmupProbationSelectSignalArmReturnsFatalAcceptErr(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	captureExitHoldingHome(t, nil)

	fatalErr := errors.New("simulated fatal accept error (probation-signal arm)")
	const transactionID = "txn-warmup-probation-signal"
	runErr := startProbationDaemon(t, transactionID)

	probation := waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
	require.Equal(t, transactionID, probation.TransactionID)
	time.Sleep(50 * time.Millisecond) // ensure the main flow is parked in the select

	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case err := <-runErr:
		require.ErrorIs(t, err, fatalErr,
			"the probation-select sigChan arm must return the recorded fatal accept error, not nil (exit 0)")
	case <-time.After(15 * time.Second):
		t.Fatal("runDaemon did not exit after SIGTERM during upgrade probation")
	}
}

// throwawaySetup arms a throwaway sandbox marker with a live owner process and a
// fast (20ms) watchdog poll, so killing the owner closes throwawayExpireCh
// within tens of milliseconds. It returns the owner process and restores all
// seams in cleanup. Mirrors the pattern in throwaway_expiry_race_test.go.
func throwawaySetup(t *testing.T) *exec.Cmd {
	t.Helper()
	owner := exec.Command("sleep", "30")
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = owner.Process.Kill()
		_, _ = owner.Process.Wait()
	})

	notDefault, registry := t.TempDir(), t.TempDir()
	previousDefault := throwawayDefaultHomeFn
	throwawayDefaultHomeFn = func() (string, error) { return notDefault, nil }
	previousRegistry := throwawayRegistryDirFn
	throwawayRegistryDirFn = func() string { return registry }
	previousInterval := throwawayCheckInterval
	throwawayCheckInterval = 20 * time.Millisecond
	previousTeardown := throwawayTeardownFn
	throwawayTeardownFn = func(string, *throwawaysandbox.Marker) {}
	previousReap := throwawayHomeContainerReapFn
	throwawayHomeContainerReapFn = func(string) bool { return false }
	t.Cleanup(func() {
		throwawayDefaultHomeFn, throwawayRegistryDirFn = previousDefault, previousRegistry
		throwawayCheckInterval = previousInterval
		throwawayTeardownFn, throwawayHomeContainerReapFn = previousTeardown, previousReap
	})

	home, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, throwawaysandbox.WriteMarker(home, &throwawaysandbox.Marker{
		Version:            1,
		Kind:               "test-warmup-fatal",
		CreatedUnix:        time.Now().Unix(),
		MaxLifetimeSeconds: 3600,
		OwnerPID:           owner.Process.Pid,
	}))
	return owner
}

// TestWarmupRestoreThrowawayArmPreservesCleanExit forces the restore select to
// take the throwawayExpireCh arm while a fatal accept error is recorded. The
// throwaway watchdog closes throwawayExpireCh (only ready arm besides the held
// restore), so the select deterministically takes the throwaway arm. A throwaway
// sandbox's lifetime has ended, so the arm must take the clean terminal exit
// (nil, exit 0) — not the recorded fatal accept error — so Restart=on-failure
// does not relaunch an expired sandbox against its still-expired marker (#7620).
func TestWarmupRestoreThrowawayArmPreservesCleanExit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	captureExitHoldingHome(t, nil)

	owner := throwawaySetup(t)
	fatalErr := errors.New("simulated fatal accept error (restore-throwaway arm)")
	restoreStarted, _, runErr := holdRestoreAndStart(t)

	waitSocketBound(t, "the daemon never reached the restore select")
	select {
	case <-restoreStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore goroutine never started; the daemon is not parked in the restore select")
	}
	time.Sleep(50 * time.Millisecond)

	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)
	require.NoError(t, owner.Process.Kill())
	_, _ = owner.Process.Wait()

	select {
	case err := <-runErr:
		require.NoError(t, err,
			"the restore-select throwawayExpireCh arm must preserve the clean terminal exit (nil/exit 0) even with a recorded fatal accept error, so an expired sandbox is not relaunched")
	case <-time.After(15 * time.Second):
		t.Fatal("runDaemon did not exit after throwaway expiry during the instance restore")
	}
}

// TestWarmupProbationThrowawayArmPreservesCleanExit forces the probation select
// to take the throwawayExpireCh arm while a fatal accept error is recorded. The
// restore completes, the daemon parks in probation, and the throwaway watchdog
// closes throwawayExpireCh. Same clean terminal exit contract as the restore
// arm: an expired sandbox must not be relaunched (#7620).
func TestWarmupProbationThrowawayArmPreservesCleanExit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	captureExitHoldingHome(t, nil)

	owner := throwawaySetup(t)
	fatalErr := errors.New("simulated fatal accept error (probation-throwaway arm)")
	const transactionID = "txn-warmup-probation-throwaway"
	runErr := startProbationDaemon(t, transactionID)

	probation := waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
	require.Equal(t, transactionID, probation.TransactionID)
	time.Sleep(50 * time.Millisecond)

	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)
	require.NoError(t, owner.Process.Kill())
	_, _ = owner.Process.Wait()

	select {
	case err := <-runErr:
		require.NoError(t, err,
			"the probation-select throwawayExpireCh arm must preserve the clean terminal exit (nil/exit 0) even with a recorded fatal accept error, so an expired sandbox is not relaunched")
	case <-time.After(15 * time.Second):
		t.Fatal("runDaemon did not exit after throwaway expiry during upgrade probation")
	}
}

// TestWarmupCoreadyFatalAcceptRaceRestoresCleanExitWhenThrowawayFired pins the
// follow-up to the throwaway-arm clean-exit contract: when the throwaway
// watchdog fires AND a fatal accept error is recorded, the warm-up restore
// select's signal/shutdown arm can win the co-ready race and return
// fatalAcceptShutdownErr() directly. The deferred reconciliation must then
// clear that error so the daemon takes the clean terminal exit the main path's
// throwawayFired() gate also takes — not exit non-zero and relaunch the expired
// sandbox under Restart=on-failure against its still-expired marker (#7620).
// Pre-fix, the reconciliation only touched a still-nil retErr, so the signal
// arm's fatal error survived the drain and the daemon exited non-zero.
//
// The race is exercised across iterations: the throwaway owner is killed
// (closing throwawayExpireCh) and SIGTERM is sent (buffering in sigChan) while
// the restore is held, so both channels are co-ready in the restore select.
// Go's uniform-random arm selection takes the signal arm on roughly half the
// iterations; the throwaway arm already returns nil, so only the signal-arm
// outcomes distinguish the fix. The throwaway fires before the deferred
// settleThrowaway runs, so the reconciliation observes it.
func TestWarmupCoreadyFatalAcceptRaceRestoresCleanExitWhenThrowawayFired(t *testing.T) {
	const iterations = 10
	var nonNilReturns atomic.Int32

	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iter_%d", i), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			stubContainerSweep(t)
			captureExitHoldingHome(t, nil)

			owner := throwawaySetup(t)
			fatalErr := errors.New("co-ready throwaway fatal accept error")
			restoreStarted, _, runErr := holdRestoreAndStart(t)

			waitSocketBound(t, "the daemon never reached the restore select")
			select {
			case <-restoreStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("the restore goroutine never started; the daemon is not parked in the restore select")
			}
			time.Sleep(50 * time.Millisecond)

			setFatalAcceptErr(fatalErr)
			t.Cleanup(resetFatalAcceptErr)
			// Kill the throwaway owner so the watchdog closes throwawayExpireCh,
			// then wait long enough for the 20ms poll to observe it. Send SIGTERM
			// so sigChan is co-ready with throwawayExpireCh in the restore select.
			// The restore is held and shutdownCh is not closed (the fatal error
			// was recorded directly, not via the accept loop), so the two arms
			// are the only ready cases.
			require.NoError(t, owner.Process.Kill())
			_, _ = owner.Process.Wait()
			time.Sleep(50 * time.Millisecond)
			require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

			select {
			case err := <-runErr:
				if err != nil {
					nonNilReturns.Add(1)
					t.Fatalf("runDaemon returned %v (non-zero) despite the throwaway expiry — the co-ready race demoted the clean terminal exit", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("runDaemon did not exit after the co-ready throwaway/signal race")
			}
		})
	}
	require.Zero(t, nonNilReturns.Load(), "%d/%d co-ready iterations returned non-nil despite the throwaway expiry", nonNilReturns.Load(), iterations)
}

// TestHoldHomeIfAbandonedExitsOneOnPendingFatalAcceptErr pins the
// defense-in-depth fix in holdHomeIfAbandoned's exit classification: when the
// run returned nil (no run-level error or panic) but a fatal accept error is
// recorded, the exit must be 1 (not 0) so the supervisor restarts the daemon.
// Pre-fix, the default branch left exit.code at 0, suppressing the restart.
func TestHoldHomeIfAbandonedExitsOneOnPendingFatalAcceptErr(t *testing.T) {
	fatalErr := errors.New("simulated fatal accept error (holdHome test)")
	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)

	exits := captureExitHoldingHome(t, nil)
	stop := &stopClock{}
	stop.abandon("a stuck teardown step")

	holdHomeIfAbandoned(stop, func() {}, nil, nil, nil, false)

	select {
	case exit := <-exits:
		require.Equal(t, 1, exit.code,
			"a pending fatal accept error must produce exit 1 even when err == nil, so the supervisor restarts the daemon")
		require.Contains(t, exit.abandoned, "a stuck teardown step")
	default:
		t.Fatal("holdHomeIfAbandoned did not call exitHoldingHome despite a non-empty ledger")
	}
}

// TestHoldHomeIfAbandonedExitsZeroWhenNoFatalAcceptErr confirms the
// non-regression: when there is no fatal accept error and err == nil, the
// exit code remains 0 (the pre-existing behaviour for a clean abandoned stop).
func TestHoldHomeIfAbandonedExitsZeroWhenNoFatalAcceptErr(t *testing.T) {
	resetFatalAcceptErr()

	exits := captureExitHoldingHome(t, nil)
	stop := &stopClock{}
	stop.abandon("a stuck teardown step")

	holdHomeIfAbandoned(stop, func() {}, nil, nil, nil, false)

	select {
	case exit := <-exits:
		require.Zero(t, exit.code,
			"no fatal accept error and no run error must still exit 0 (a requested stop that abandoned a step)")
	default:
		t.Fatal("holdHomeIfAbandoned did not call exitHoldingHome despite a non-empty ledger")
	}
}

// TestHoldHomeIfAbandonedExitsOneOnRunErrorPreferencingFatalErr confirms the
// run-error path still exits 1 and that a concurrent fatal accept error does
// not downgrade it. The err != nil branch remains the primary classifier; the
// default branch is only a fallback for err == nil.
func TestHoldHomeIfAbandonedExitsOneOnRunErrorPreferencingFatalErr(t *testing.T) {
	fatalErr := errors.New("simulated fatal accept error (run-error test)")
	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)

	exits := captureExitHoldingHome(t, nil)
	stop := &stopClock{}
	stop.abandon("a stuck teardown step")

	holdHomeIfAbandoned(stop, func() {}, errors.New("run-level error"), nil, nil, false)

	select {
	case exit := <-exits:
		require.Equal(t, 1, exit.code, "a run-level error must still exit 1, fatal or not")
	default:
		t.Fatal("holdHomeIfAbandoned did not call exitHoldingHome despite a non-empty ledger")
	}
}

// TestHoldHomeIfAbandonedPreservesCleanExitOnFatalAcceptErr pins the
// defense-in-depth carve-out: when the run returned nil and a fatal accept
// error is recorded, but the exit is an intentional clean terminal one — the AF
// home was deleted, or a throwaway sandbox expired (cleanExit=true) — the exit
// must stay 0 so a Restart=on-failure supervisor does not relaunch the daemon
// into a deleted home or against an expired throwaway marker (#7620).
func TestHoldHomeIfAbandonedPreservesCleanExitOnFatalAcceptErr(t *testing.T) {
	fatalErr := errors.New("simulated fatal accept error (clean-exit test)")
	setFatalAcceptErr(fatalErr)
	t.Cleanup(resetFatalAcceptErr)

	exits := captureExitHoldingHome(t, nil)
	stop := &stopClock{}
	stop.abandon("a stuck teardown step")

	holdHomeIfAbandoned(stop, func() {}, nil, nil, nil, true)

	select {
	case exit := <-exits:
		require.Zero(t, exit.code,
			"a clean terminal exit (deleted home / throwaway expiry) must stay 0 even with a pending fatal accept error, so the supervisor does not relaunch the daemon")
	default:
		t.Fatal("holdHomeIfAbandoned did not call exitHoldingHome despite a non-empty ledger")
	}
}

// waitFatalAcceptErrOrError polls the fatal accept error until it is recorded
// or the deadline elapses. Returns nil once the error is recorded (which also
// implies shutdownCh is closed, since fatalAcceptClose stores the error before
// closing shutdownCh). Returns an error on timeout — never calls t.Fatalf,
// because it may run on the daemon's main-flow goroutine inside
// testHookStartupStep.
func waitFatalAcceptErrOrError() error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fatalAcceptShutdownErr() != nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("fatal accept error was never recorded")
}

// TestWarmupCoreadyFatalAcceptRaceNeverExitsZero runs the genuine co-ready race
// — shutdownCh and sigChan both ready in the same select — across several
// iterations and asserts runDaemon returns a non-nil error every time. With
// the fix, every arm of the select returns fatalAcceptShutdownErr(), so the
// random arm selection is irrelevant. Pre-fix, ~50% of co-ready outcomes
// returned nil (exit 0), suppressing the supervisor restart.
//
// The race is orchestrated deterministically: the accept injector fires a
// fatal error immediately on bind (closing shutdownCh), and the
// testHookStartupStep for the "dedicated tmux server" step (which runs after
// stopStartupSignalWatch and before the restore select) waits for the fatal
// error, sends SIGTERM to buffer it in sigChan, and then returns — so by the
// time the main flow reaches the select, both channels are co-ready.
func TestWarmupCoreadyFatalAcceptRaceNeverExitsZero(t *testing.T) {
	const iterations = 10
	var nilReturns atomic.Int32

	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iter_%d", i), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			stubContainerSweep(t)
			captureExitHoldingHome(t, nil)

			// Injector: fatal error on the first Accept call. The accept
			// loop calls fatalAcceptClose immediately, recording the error
			// and closing shutdownCh before the main flow reaches the select.
			fatalErr := errors.New("co-ready race fatal accept error")
			installAcceptInjector(t, []error{fatalErr})

			// Hold the restore so the main flow parks in the restore select.
			// seamDone closes after the restore goroutine unblocks, so the
			// cleanup can wait for it before restoring testHookStartupStep —
			// the goroutine reads that package var inside startupStep BEFORE
			// calling the seam, and the seam is the earliest point the
			// cleanup can observe. Waiting here prevents a data race between
			// the cleanup's var restore and the goroutine's var read.
			holdRestore := make(chan struct{})
			seamDone := make(chan struct{})
			previousRestore := restoreManagerForStartup
			restoreManagerForStartup = func(m *Manager) error {
				<-holdRestore
				close(seamDone)
				return nil
			}

			// The "dedicated tmux server" startup step runs after
			// stopStartupSignalWatch (so SIGTERM buffers in sigChan rather
			// than being re-raised) and before the restore select. Wait for
			// the accept error to fire (shutdownCh closed), then send SIGTERM
			// and let the relay buffer it. When the hook returns, the main
			// flow continues to the select where both channels are co-ready.
			stepDone := make(chan struct{})
			hookErr := make(chan error, 1)
			previousStepHook := testHookStartupStep
			testHookStartupStep = func(name string) {
				if name != "dedicated tmux server" {
					return
				}
				if err := waitFatalAcceptErrOrError(); err != nil {
					select {
					case hookErr <- err:
					default:
					}
					return
				}
				// fatalAcceptClose has recorded the error and closed
				// shutdownCh. Send SIGTERM; it buffers in sigChan (the select
				// has no consumer yet, so the relay's send succeeds).
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					select {
					case hookErr <- fmt.Errorf("SIGTERM: %w", err):
					default:
					}
					return
				}
				time.Sleep(50 * time.Millisecond) // let the relay buffer it
				close(stepDone)
			}

			runExited := make(chan struct{})
			runErrCh := make(chan error, 1)
			go func() {
				defer close(runExited)
				runErrCh <- runDaemon(tcpDisabledConfig(), "")
			}()
			t.Cleanup(func() {
				// Close holdRestore FIRST so the restore goroutine can proceed
				// through the seam (startupStep reads testHookStartupStep before
				// calling the seam, so the read is done by the time seamDone
				// closes). Then wait for the goroutine to get past the read
				// before restoring testHookStartupStep — without this wait the
				// cleanup's var write races the goroutine's var read.
				select {
				case <-holdRestore:
				default:
					close(holdRestore)
				}
				select {
				case <-seamDone:
				case <-time.After(5 * time.Second):
				}
				testHookStartupStep = previousStepHook
				restoreManagerForStartup = previousRestore
				select {
				case <-runExited:
				default:
					_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
					select {
					case <-runExited:
					case <-time.After(10 * time.Second):
						t.Error("in-process daemon did not exit during cleanup")
					}
				}
				resetFatalAcceptErr()
			})

			select {
			case err := <-hookErr:
				t.Fatal(err)
			case <-stepDone:
			case <-time.After(15 * time.Second):
				t.Fatal("the 'dedicated tmux server' startup step never ran")
			}

			select {
			case err := <-runErrCh:
				if err == nil {
					nilReturns.Add(1)
					t.Fatalf("runDaemon returned nil (exit 0) despite a recorded fatal accept error — the co-ready race demoted the exit code")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("runDaemon did not exit after the co-ready race")
			}
		})
	}
	require.Zero(t, nilReturns.Load(), "%d/%d co-ready iterations returned nil (exit 0) — the fix was not applied", nilReturns.Load(), iterations)
}
