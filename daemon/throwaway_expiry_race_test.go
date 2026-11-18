package daemon

import (
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// TestRunDaemonThrowawayExpiryIsObservedWhenItRacesTheStop pins the fix for the
// flag-loss race in the throwaway-sandbox lifetime enforcement (introduced with
// the feature in commit 3b6d5504, #6844): the watchdog closing
// throwawayExpireCh while a concurrent stop vector is already mid-drain never
// reaches the main select (the stop case won, and the close lands during
// drainDaemon's join, which can run for drainBudget). Pre-fix, the
// throwawayExpired boolean was only ever set in the select arm that happened to
// receive from the channel, so it stayed false and the daemon exited as a fully
// clean stop — throwawayTeardownFn and the expired-container reap were
// skipped, and the final save persisted live-session state into the expired
// home. Post-fix, the channel is re-drained non-blockingly after drainDaemon
// and inside endThrowaway, so the close is observed on the way out and the
// whole sandbox dies with the daemon.
//
// The race is made deterministic: a marked home whose marker declares a live
// owner process the watchdog polls every 20ms; a background durable-state
// writer admitted before the stop blocks on its stop channel and then sleeps
// 1.5s, so drainDaemon's join holds the stop open; the test waits until that
// stop channel closes (proof the Shutdown RPC was received and the main select
// already chose shutdownCh) before killing the declared owner, so the
// watchdog fires mid-drain. The test asserts the drain really was held, the
// watchdog really fired, and then that the fix's guarantees hold: teardown
// ran, the container reap ran, and the final save was skipped.
func TestRunDaemonThrowawayExpiryIsObservedWhenItRacesTheStop(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubContainerSweep(t)

	// A live "owner" process the marker declares. The watchdog polls its
	// liveness every 20ms (throwawayCheckInterval seam below), so killing it
	// mid-drain closes throwawayExpireCh within tens of milliseconds — after
	// the main select already took the stop case.
	owner := exec.Command("sleep", "30")
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = owner.Process.Kill()
		_, _ = owner.Process.Wait()
	})

	// The test's home is AGENT_FACTORY_HOME, which the marker check would
	// read as the default install; point that check and the per-user registry
	// at directories of the test's own.
	notDefault, registry := t.TempDir(), t.TempDir()
	previousDefault := throwawayDefaultHomeFn
	throwawayDefaultHomeFn = func() (string, error) { return notDefault, nil }
	previousRegistry := throwawayRegistryDirFn
	throwawayRegistryDirFn = func() string { return registry }
	// A short owner-liveness poll so the fire lands inside the drain.
	previousInterval := throwawayCheckInterval
	throwawayCheckInterval = 20 * time.Millisecond
	// Record the teardown and the container reap so the test can observe them.
	var toreDown atomic.Bool
	previousTeardown := throwawayTeardownFn
	throwawayTeardownFn = func(string, *throwawaysandbox.Marker) { toreDown.Store(true) }
	var reaped atomic.Bool
	previousReap := throwawayHomeContainerReapFn
	throwawayHomeContainerReapFn = func(string) bool { reaped.Store(true); return true }
	t.Cleanup(func() {
		throwawayDefaultHomeFn, throwawayRegistryDirFn = previousDefault, previousRegistry
		throwawayCheckInterval = previousInterval
		throwawayTeardownFn, throwawayHomeContainerReapFn = previousTeardown, previousReap
	})

	// The marker declares the owner, with a deadline well in the future so
	// only owner-death fires during the test.
	require.NoError(t, throwawaysandbox.WriteMarker(home, &throwawaysandbox.Marker{
		Version:            1,
		Kind:               "test-expiry-race",
		CreatedUnix:        time.Now().Unix(),
		MaxLifetimeSeconds: 3600,
		OwnerPID:           owner.Process.Pid,
	}))

	// Capture the final-save seam so the test can assert it was skipped.
	var saved atomic.Bool
	previousSave := finalSave
	finalSave = func(*Manager) error { saved.Store(true); return nil }
	t.Cleanup(func() { finalSave = previousSave })

	info := captureInfo(t)
	// Record so an abandoned exit never ends the test binary.
	captureExitHoldingHome(t, nil)

	managers := captureManager(t)
	runExited := startInProcessDaemon(t, func() {
		_ = owner.Process.Kill()
		_, _ = owner.Process.Wait()
	})

	var manager *Manager
	select {
	case manager = <-managers:
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon never reached its instance restore")
	}
	waitForDaemonPhase(t, DaemonPhaseReady)

	// Admit a background durable-state writer that blocks on its stop channel
	// and then sleeps well past the kill below, so drainDaemon's join (which
	// waits for admitted background mutations through
	// stopAndWaitBackgroundMutationsForShutdown) holds the stop open. The
	// drain then runs for ~this long, giving the watchdog's fire a guaranteed
	// mid-drain landing window after the main select already returned.
	//
	// drainEntered closes the instant stopAndWaitBackgroundMutationsForShutdown
	// closes the mutation's stop channel, which is proof that the Shutdown RPC
	// was received and the main select already chose shutdownCh and returned —
	// the regression's precondition. Waiting on it before the kill (instead of
	// a fixed delay) keeps the test honest on a slow or heavily scheduled
	// runner, where a 400 ms sleep could let the main select choose
	// throwawayExpireCh under the old implementation and pass every assertion
	// without exercising the race.
	const drainHold = 1500 * time.Millisecond
	drainEntered := make(chan struct{})
	require.True(t, manager.launchBackgroundMutation(func(stop <-chan struct{}) {
		<-stop
		close(drainEntered)
		// Hold the drain open for the full drainHold after the drain entered
		// stopAndWait, so the watchdog's fire is guaranteed to land mid-drain.
		time.Sleep(drainHold)
	}), "the background mutation that holds the drain was not admitted")

	// Stop the daemon; the drain starts once the Shutdown RPC is received.
	asked := time.Now()
	go func() {
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
	}()
	// Wait until the drain has actually entered stopAndWait — the background
	// mutation's stop channel closed — so the main select already took the
	// shutdownCh case and returned before the watchdog fires. Then kill the
	// declared owner. The watchdog polls every 20ms, so it closes
	// throwawayExpireCh within ~40ms — mid-drain, the race this test pins.
	select {
	case <-drainEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("the drain never started; the Shutdown RPC was not observed before the kill")
	}
	require.NoError(t, owner.Process.Kill())
	_, _ = owner.Process.Wait()

	select {
	case <-runExited:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never exited after the racing stop")
	}

	// Sanity: the drain really was held past the kill (the fire was mid-drain,
	// not before the select). The stop began at `asked`; the daemon exits
	// after the drain join releases at ~drainHold. Allow slack for scheduling.
	require.GreaterOrEqual(t, time.Since(asked), drainHold-300*time.Millisecond,
		"the drain was not held open; the racing fire did not land mid-drain")
	// Sanity: the watchdog really fired before exit.
	require.Contains(t, info.String(), "declared owner pid",
		"the watchdog never fired; the racing-fire test did not exercise the bug")

	// The fix's guarantees: the teardown ran, the container reap ran, and the
	// final save was skipped — despite the fire losing the main select.
	require.True(t, toreDown.Load(),
		"the expired sandbox was not torn down when the fire raced the stop; the sandbox's driver, agents, and private tmux server survive past the declared lifetime")
	require.True(t, reaped.Load(),
		"the expired sandbox's session containers were not reaped when the fire raced the stop")
	require.False(t, saved.Load(),
		"the final save ran and persisted live-session state into the expired sandbox")
}
