package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// resetDetachWatchdog tears down any watchdog left armed by a test so it
// cannot leak its goroutine (or a spurious dump) into a sibling test that
// shares the package-global detachWatchdogDone.
func resetDetachWatchdog(t *testing.T) {
	t.Helper()
	t.Cleanup(endDetachWatchdog)
}

// TestWatchdogCanceled_WhenLastInstanceRemovedWhileAttached is the regression
// test for issue #683. The slow-detach watchdog (armed in
// attachOverlayCallback) is normally ended when the post-detach paint
// completes via panesRefreshedMsg. That msg is only emitted by refreshPanesCmd,
// which selectionChanged dispatches only when an instance row is selected.
//
// When the only instance is removed while the user is attached, the sidebar
// selection falls back to the Instances header. On detach, selectionChanged
// returns nil (no refresh cmd), so panesRefreshedMsg never arrives. Before the
// fix the watchdog stayed armed and fired a spurious goroutine dump after
// slowDetachThreshold. After the fix, the repaintAfterDetachMsg handler ends
// the watchdog when selectionChanged returns nil.
func TestWatchdogCanceled_WhenLastInstanceRemovedWhileAttached(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "only-instance")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.False(t, h.sidebar.GetSelection().IsHeader,
		"initial selection should be on the instance row")

	// Simulate: attached, then the only instance is removed out from under us.
	h.attached.Store(true)
	h.store.RemoveInstanceByTitle("only-instance")
	require.True(t, h.sidebar.GetSelection().IsHeader,
		"after removing the only instance, selection falls back to the header")

	// Detach: arm the watchdog exactly as attachOverlayCallback does, then run
	// the post-detach repaint handler.
	h.attached.Store(false)
	beginDetachWatchdog("test-attach")

	_, cmd := h.Update(repaintAfterDetachMsg{})
	require.Nil(t, cmd,
		"selectionChanged returns nil on a header selection (no panes to refresh)")

	// The handler must have ended the watchdog itself, since no
	// panesRefreshedMsg will ever arrive to do it.
	detachWatchdogMu.Lock()
	armed := detachWatchdogDone
	detachWatchdogMu.Unlock()
	require.Nil(t, armed,
		"watchdog must be canceled on the no-instances early-return, "+
			"otherwise it fires a spurious goroutine dump after slowDetachThreshold (#683)")
}

// TestWatchdog_NoSpuriousDumpAfterHeaderDetach is the end-to-end proof that the
// fix prevents the spurious diagnostic file. It runs past the real
// slowDetachThreshold and asserts detach-slow.log is never written for the
// header-selection detach. The first phase is a control that lets an
// un-canceled watchdog fire, proving the dump wiring (config dir →
// detach-slow.log) works and that this test can actually observe a dump.
func TestWatchdog_NoSpuriousDumpAfterHeaderDetach(t *testing.T) {
	resetDetachWatchdog(t)

	// newTestHome redirects AGENT_FACTORY_HOME to its own tempdir, so derive
	// the dump path from the config dir *after* building the home — otherwise
	// dumpSlowDetach writes to the home's dir while we watch the wrong one.
	h := newTestHome(t)
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	dumpPath := filepath.Join(configDir, detachSlowDumpFileName)

	// Pin the marker gate OFF: the watchdog must arm and dump regardless of
	// AF_DETACH_TRACE (#788) — a dump from a user who never set the env var
	// is how detach-perf regressions get noticed in the wild.
	prev := detachTraceEnabled
	detachTraceEnabled = false
	t.Cleanup(func() { detachTraceEnabled = prev })

	// --- Control: an un-canceled watchdog DOES write a dump after the
	// threshold. This guards against the regression assertion silently passing
	// because the dump path was misconfigured.
	beginDetachWatchdog("control-uncanceled")
	// Wait for the dump on the CONDITION, not on a fixed nap. Writing it means a
	// goroutine has to be scheduled and a full goroutine dump has to reach disk,
	// neither of which has an upper bound on a loaded box; a nap sized for an idle
	// one turns a busy machine into a failed assertion (#2879). The ceiling is a
	// failure deadline, not an expectation.
	control := time.Now()
	dumpExists := func() bool {
		_, statErr := os.Stat(dumpPath)
		return statErr == nil
	}
	require.Eventually(t, dumpExists, 30*time.Second, 10*time.Millisecond,
		"control: an un-canceled watchdog must write detach-slow.log after the threshold")
	// How long a REAL dump took on this machine, used below to size the window the
	// regression case must stay silent for.
	controlTook := time.Since(control)
	endDetachWatchdog()
	require.NoError(t, os.Remove(dumpPath))

	// --- Regression: the header-selection detach must NOT write a dump.
	inst := instanceWithFakeBackend(t, "only-instance")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	h.attached.Store(true)
	h.store.RemoveInstanceByTitle("only-instance")
	require.True(t, h.sidebar.GetSelection().IsHeader)
	h.attached.Store(false)

	beginDetachWatchdog("regression-header-detach")
	_, cmd := h.Update(repaintAfterDetachMsg{})
	require.Nil(t, cmd)

	// An ABSENCE, so the window is what gives it teeth: require nothing to appear
	// for at least as long as a real dump just took on this same machine, so a
	// merely-slow watchdog cannot be mistaken for a silent one. Sized from that
	// measurement rather than from a constant, which is what keeps it honest under
	// the load that made the constant wrong in the first place (#2879).
	require.Never(t, dumpExists, controlTook+300*time.Millisecond, 10*time.Millisecond,
		"watchdog wrote a spurious goroutine dump for a header-selection detach (#683)")
}
