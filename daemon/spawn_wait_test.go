package daemon

import (
	"testing"
	"time"
)

// testSpawnReadyTimeout bounds how long the spawn/kill tests wait for a real
// child process to reach an OBSERVABLE state — its rewritten cmdline becoming
// visible (`exec -a` argv[0] rewrite) or a trap-ready sentinel file appearing.
// It is deliberately far larger than the few milliseconds that transition takes
// on a healthy box: the tests poll an observable condition, so an over-generous
// bound costs nothing on success yet removes the wall-clock race that made the
// TestSigtermFallback_* / TestStopDaemon_* family flake under CI runner load
// (#878). This is a TEST wait bound only — production grace/timeouts
// (sigtermFallbackGrace, stopDaemonGrace) are unchanged.
const testSpawnReadyTimeout = 20 * time.Second

// waitForReady polls cond until it returns true, sleeping briefly between
// checks, and fails the test if cond is still false after testSpawnReadyTimeout.
// It is the event-driven replacement for the family's old fixed-bound readiness
// loops (500ms / 2s), which could expire just before a loaded runner finished
// spawning the child and rewriting its cmdline. desc names what we were waiting
// for so a genuine hang (as opposed to a too-tight bound) produces an
// actionable failure rather than a misleading downstream assertion.
func waitForReady(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testSpawnReadyTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s but condition never held: %s", testSpawnReadyTimeout, desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
