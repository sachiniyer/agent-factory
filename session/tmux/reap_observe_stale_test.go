package tmux

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// stubSnapshot installs a proctreeSnapshot override that returns snap on every
// call and restores the real function on test cleanup.
func stubSnapshot(t *testing.T, snap func() (map[int]proctree.Process, error)) {
	t.Helper()
	previous := proctreeSnapshot
	proctreeSnapshot = snap
	t.Cleanup(func() { proctreeSnapshot = previous })
}

// snapshotCallCounter wraps a snapshot override so a test can select a specific
// invocation to fail, then pass-through the real snapshot (or a supplied result)
// on every other call.
type snapshotCallCounter struct {
	calls     int
	failCalls map[int]struct{}
	Real      func() (map[int]proctree.Process, error)
	// OnSuccess is the snapshot map returned when the call should not fail; nil
	// falls through to the real snapshot.
	OnSuccess map[int]proctree.Process
}

func (s *snapshotCallCounter) snapshot() (map[int]proctree.Process, error) {
	s.calls++
	if _, fail := s.failCalls[s.calls]; fail {
		return nil, fmt.Errorf("cannot refresh processes after tmux session %s vanished: %w",
			"af_observe_test", errors.New("reading /proc: transient procfs read failure"))
	}
	if s.OnSuccess != nil {
		return s.OnSuccess, nil
	}
	return s.Real()
}

// transientProcfsError is the error text a real proctree.Snapshot failure wraps;
// assertions use it to prove the stale observeErr (not a reaping term) is the
// source of a spurious abort.
const transientProcfsError = "cannot refresh processes"

// TestObserveOrphanAncestryClearsStaleSnapshotErrorOnProvenExit is the unit-level
// regression for the sticky observeErr bug: a transient proctree.Snapshot failure
// on one observe poll is accumulated into observeErr and never cleared. When a
// subsequent poll succeeds and its snapshot proves every captured process is
// already dead (!live), the function should return nil — it has EVIDENCE the
// capture is empty — but the stale observeErr makes it report failure instead.
//
// This is case (iii) from the deadline-return analysis: some polls fail, then a
// successful poll before the deadline with !live triggers the early return. The
// fix (observeErr = nil on the success branch) clears the transient failure.
//
// Without the fix this test FAILS: the stale observeErr is returned despite the
// successful poll proving no captured process is alive.
func TestObserveOrphanAncestryClearsStaleSnapshotErrorOnProvenExit(t *testing.T) {
	shrinkReapWaits(t)
	// A captured PID that will never appear in the successful snapshot, so the
	// !live check trips by evidence (absence from the snapshot), not by failure.
	captured := []proctree.Process{{PID: 4242, StartID: 1, SID: 4242}}
	counter := &snapshotCallCounter{
		failCalls: map[int]struct{}{1: {}},
		Real:      proctree.Snapshot,
		// An empty snapshot guarantees the captured PID is absent: !live.
		OnSuccess: map[int]proctree.Process{},
	}
	stubSnapshot(t, counter.snapshot)

	returned, err := observeOrphanAncestry(captured, "af_observe_test", reapGraceWait)
	require.NoError(t, err,
		"a successful pass that proves no captured process is alive must clear a stale transient snapshot error")
	require.GreaterOrEqual(t, counter.calls, 2,
		"the observe loop must make at least two proctreeSnapshot calls (fail then recover)")
	require.NotEmpty(t, returned, "the captured identity list must be returned in full")
}

// TestObserveOrphanAncestryPreservesErrorWhenAllSnapshotPollsFail verifies the
// fail-closed path is untouched by the fix. When every poll in the grace window
// fails and the deadline expires, the function must still return the accumulated
// error — a permanent procfs outage must not be laundered into success.
//
// This is case (i) from the deadline-return analysis: all polls fail, no success
// branch ever runs to clear observeErr, deadline returns the accumulated join.
// The fix does not change this path, so the test PASSES with and without the fix.
func TestObserveOrphanAncestryPreservesErrorWhenAllSnapshotPollsFail(t *testing.T) {
	shrinkReapWaits(t)
	captured := []proctree.Process{{PID: 4242, StartID: 1, SID: 4242}}
	counter := &snapshotCallCounter{
		Real: proctree.Snapshot,
	}
	// Every call fails: no temporary directory exists so ReadDir errors — but
	// the concrete error does not matter, only that every iteration hits the
	// err != nil branch and observeErr is never cleared.
	counter.failCalls = map[int]struct{}{}
	for i := 1; i <= 20; i++ {
		counter.failCalls[i] = struct{}{}
	}
	stubSnapshot(t, counter.snapshot)

	_, err := observeOrphanAncestry(captured, "af_observe_test", reapGraceWait)
	require.Error(t, err,
		"a permanent procfs outage must still surface an error, not be laundered into success")
	require.ErrorContains(t, err, transientProcfsError,
		"the accumulated error must be the procfs read failure, not a spurious empty result")
}

// TestObserveOrphanAncestryReturnsLiveProcessesAfterSnapshotRecoveryAtDeadline
// verifies case (iv): some polls fail, then succeed showing the captured process
// is still alive (live == true) until the deadline expires. The fix clears
// observeErr on each success, so the deadline return yields nil error; the
// captured list of still-live processes is returned in full so the caller can
// reap them. Without the fix the stale observeErr leaks onto the deadline return,
// aborting the sweep for a transient read that has since recovered.
//
// The fail-closed guarantee for genuinely-live orphans is NOT lost: the caller
// reaps the returned captured list via reapSessionProcesses, and survivors appear
// as independent reaping terms (cleanup.go:685-697), not via observeErr.
//
// Without the fix this test FAILS: the deadline return includes the stale error.
func TestObserveOrphanAncestryReturnsLiveProcessesAfterSnapshotRecoveryAtDeadline(t *testing.T) {
	shrinkReapWaits(t)
	captured := []proctree.Process{{PID: 4242, StartID: 999, SID: 4242}}
	// A successful snapshot that includes the captured process with a matching
	// StartID, so live == true on every successful poll — the loop runs to the
	// deadline rather than returning early on !live.
	liveSnapshot := map[int]proctree.Process{
		4242: {PID: 4242, StartID: 999, SID: 4242, PPID: 1},
	}
	counter := &snapshotCallCounter{
		failCalls: map[int]struct{}{1: {}},
		Real:      proctree.Snapshot,
		OnSuccess: liveSnapshot,
	}
	stubSnapshot(t, counter.snapshot)

	returned, err := observeOrphanAncestry(captured, "af_observe_test", reapGraceWait)
	require.NoError(t, err,
		"after the transient procfs read recovers, the deadline return must not carry the stale error")
	require.NotEmpty(t, returned,
		"the still-live captured processes must be returned so the caller can reap them")
	require.Equal(t, captured[0].PID, returned[0].PID,
		"the returned identity list must match the captured set")

	// Record at least the failed call plus enough successful calls to reach the
	// deadline — proves the loop continued past the recovery and clear.
	require.GreaterOrEqual(t, counter.calls, 3,
		"the observe loop must continue past the failing pass until the deadline")
}

// TestVanishedSessionSweepSucceedsAfterTransientSnapshotFailure is the end-to-end
// regression: a transient proctree.Snapshot failure inside the observe loop must
// not abort af reset after the orphan sweep already completed.
//
// It uses graceObservationBarrier to hold the sweep at its first observe pass so
// the test can kill the captured escapee (so the next successful snapshot proves
// !live) and arm the snapshot seam to fail on the upcoming observe-poll call only.
// The bracketing refreshOrphanCandidates calls (cleanup.go:664,674,677,691) see
// the real snapshot, so only the observe poll is affected — isolating the spurious
// abort from correct fail-closed behavior.
//
// Without the fix this test FAILS: reapVanishedSessionProcesses returns non-nil
// (the stale observeErr joined into sweepErr), aborting af reset after reaping
// completed and no captured process remains.
func TestVanishedSessionSweepSucceedsAfterTransientSnapshotFailure(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_observe_transient_recovers"
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	captured := spawnMarkedSessionWithEscapee(t, name, home, "transient-gen")
	vanishSessionWithObservableEscapee(t, name, captured)
	require.True(t, proctree.AliveSame(captured), "marked escapee must outlive its tmux session")

	realSnapshot := proctreeSnapshot
	var failNextObserveSnapshot atomic.Bool
	stubSnapshot(t, func() (map[int]proctree.Process, error) {
		if failNextObserveSnapshot.CompareAndSwap(true, false) {
			return nil, fmt.Errorf("reading /proc: transient procfs read failure")
		}
		return realSnapshot()
	})

	barrier, sweepDone := startSweepAtGraceBarrier(t, name, func() error {
		return reapVanishedSessionProcesses(name, home, []proctree.Process{captured}, nil, false)
	})
	barrier.holdFirstObservation(t, func() {
		// Kill the escapee so the next successful snapshot proves !live. The
		// sweep goroutine is blocked inside graceObservationBarrier, so this
		// runs before the observe poll's refreshCapturedAncestry returns.
		require.NoError(t, proctree.Signal(captured, syscall.SIGKILL),
			"SIGKILL the marked escapee")
		require.Eventually(t, func() bool {
			return !proctree.AliveSame(captured)
		}, 5*time.Second, 5*time.Millisecond,
			"the escapee must be dead before the observe poll's successful snapshot observes absence")
		// Arm the snapshot seam so the upcoming observe-poll call fails; the
		// barrier release (channel close) happens-before the sweep goroutine
		// reads this flag, so the write is visibility-safe without a lock.
		failNextObserveSnapshot.Store(true)
	})

	err := <-sweepDone
	require.NoError(t, err,
		"a transient snapshot failure that recovers and proves no captured process is alive must not abort the sweep")
	require.False(t, proctree.AliveSame(captured),
		"the escapee must remain dead — the sweep must not resurrect it and must not need to")
}
