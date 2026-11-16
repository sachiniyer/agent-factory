package daemon

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #7416: the teardown after the drain used to unwind one defer at a time with
// no bound, so one slow step pushed every later one past the stopper's SIGKILL.
// It now runs its steps in parallel and ends at the stop's deadline, and that
// deadline is measured from when the stop was requested.

// runTeardown runs td until deadline and fails if it outlives limit.
func runTeardown(t *testing.T, td *daemonTeardown, deadline time.Time, limit time.Duration) []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() { done <- td.run(deadline) }()
	select {
	case abandoned := <-done:
		return abandoned
	case <-time.After(limit):
		t.Fatalf("the teardown was still running after %s: it waited out a step that never returns", limit)
		return nil
	}
}

// TestTeardownRunsItsStepsInParallelAndAbandonsAtTheDeadline: a step that never
// returns neither stops the others from running nor holds the teardown past its
// deadline, and it is the only step named as abandoned. It is added first, so a
// teardown that ran its steps one after another would never reach the others.
func TestTeardownRunsItsStepsInParallelAndAbandonsAtTheDeadline(t *testing.T) {
	warn := captureWarnings(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var editors, panes atomic.Bool
	td := &daemonTeardown{}
	td.add("a stuck step", func() { <-release })
	td.add("editors", func() { editors.Store(true) })
	td.add("panes", func() { panes.Store(true) })

	start := time.Now()
	abandoned := runTeardown(t, td, start.Add(300*time.Millisecond), 5*time.Second)
	require.Less(t, time.Since(start), 2*time.Second, "the teardown must end near its deadline")
	require.True(t, editors.Load(), "a stuck step kept a later step from running")
	require.True(t, panes.Load(), "a stuck step kept a later step from running")
	require.Equal(t, []string{"a stuck step"}, abandoned)
	require.Contains(t, warn.String(), "exiting with a stuck step still running")
}

// TestTeardownGivesAPastDeadlineItsFloor: when the drain and the save spent the
// whole stop, the teardown still gets teardownFloor, so a step that finishes at
// once is not reported as abandoned.
func TestTeardownGivesAPastDeadlineItsFloor(t *testing.T) {
	td := &daemonTeardown{}
	var ran atomic.Bool
	td.add("a quick step", func() {
		time.Sleep(10 * time.Millisecond)
		ran.Store(true)
	})
	require.Empty(t, runTeardown(t, td, time.Now().Add(-time.Second), 5*time.Second))
	require.True(t, ran.Load())
}

// TestStopBudgetsShipThePinnedValues: the var the teardown reads starts at the
// production const the compile-time pin in daemon_teardown.go checks, and the
// worst stop those values allow leaves the teardown its floor and the exit its
// reserve inside the stopper's grace.
func TestStopBudgetsShipThePinnedValues(t *testing.T) {
	require.Equal(t, defaultStopTeardownBy, stopTeardownBy)
	require.Equal(t, sigtermFallbackGrace-exitReserve, defaultStopTeardownBy)
	worst := defaultDrainBudget + drainPlaneSteps*drainStepFloor + drainSaveReserve + teardownFloor + exitReserve
	require.LessOrEqual(t, worst, sigtermFallbackGrace, "the drain, the save, the teardown's floor and the exit reserve outgrow the stopper's grace")
}

// TestStopClockStartsWhenTheStopIsRequested: the stop's deadline is measured
// from the Shutdown RPC's acceptance and from a stop signal's arrival — when
// the stopper's clock starts — not from when the main loop next looks.
func TestStopClockStartsWhenTheStopIsRequested(t *testing.T) {
	// Asked this long after the request, a clock nothing stamped would say
	// "now", and the check below would fail by at least this much.
	const askedLater = 200 * time.Millisecond

	t.Run("Shutdown RPC", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
		clock := &stopClock{}
		manager.lifecycle.useStopClock(clock)
		cs := &controlServer{manager: manager, shutdownCh: make(chan struct{}), shutdownOnce: &sync.Once{}}

		accepted := time.Now()
		require.NoError(t, cs.Shutdown(ShutdownRequest{}, &ShutdownResponse{}))
		select {
		case <-cs.shutdownCh:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown never signalled the main loop")
		}
		time.Sleep(askedLater)
		require.Less(t, clock.began().Sub(accepted), shutdownAckGrace,
			"the stop must begin when Shutdown is accepted, not when the main loop wakes after the ack grace")
	})

	t.Run("signal", func(t *testing.T) {
		raw := make(chan os.Signal, 1)
		out := make(chan os.Signal, 1)
		done := make(chan struct{})
		relayed := make(chan struct{})
		clock := &stopClock{}
		go func() {
			defer close(relayed)
			relayStopSignals(raw, out, clock, done)
		}()
		t.Cleanup(func() {
			close(done)
			<-relayed
		})

		arrived := time.Now()
		raw <- syscall.SIGTERM
		select {
		case sig := <-out:
			require.Equal(t, syscall.SIGTERM, sig)
		case <-time.After(5 * time.Second):
			t.Fatal("the relay never handed the signal on")
		}
		time.Sleep(askedLater)
		require.Less(t, clock.began().Sub(arrived), askedLater/2,
			"the stop must begin when its signal arrives, not when the main flow next reads it")
	})
}
