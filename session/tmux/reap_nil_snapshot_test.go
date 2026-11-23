package tmux

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// The #7827 tests: a missing process-table snapshot must never widen what may
// be signalled. Before the fix, reapProtectedSet's nil-snapshot arm collapsed
// protection to the reaper's own pid — the fail-open direction the function's
// own contract names unsafe — and reapSessionProcesses's nil-protected entry
// recompute reached it with a NON-empty capture whenever the snapshot failed
// at reap entry. Every captured process was then signalable: a positively
// identified af-daemon subtree, the reaper's own ancestors — anything but
// self, which is the only pid proctree.Signal spares on its own.

// failReapEntrySnapshot pins the reap-entry process-table read to err for the
// duration of t and returns its invocation counter.
func failReapEntrySnapshot(t *testing.T, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	real := reapEntrySnapshot
	reapEntrySnapshot = func() (map[int]proctree.Process, error) {
		calls.Add(1)
		return nil, err
	}
	t.Cleanup(func() { reapEntrySnapshot = real })
	return &calls
}

// A non-empty capture with no process table protects EVERY captured pid, on
// the same blocking terms an unprovable-ancestry process earns: never
// signalled, still reported so a cleanup-authorizing caller refuses rather
// than reading "nothing found".
func TestReapProtectedSetNilSnapshotProtectsEveryCapturedProcess(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	captured := []proctree.Process{
		{PID: 700001, PPID: 1, Comm: "af"},
		{PID: 700002, PPID: 700001, Comm: "sleep"},
	}
	protected := reapProtectedSet(nil, captured, nil)

	for _, p := range captured {
		verdict, ok := protected[p.PID]
		require.True(t, ok,
			"captured pid %d must be protected — a missing snapshot proves nothing about its ancestry", p.PID)
		require.False(t, verdict.positive,
			"the exclusion is BLOCKING: never signalled, but still reported to cleanup-authorizing callers")
		require.Zero(t, verdict.unverifiedDaemon,
			"no daemon claim was ever observed — there is no claimant pid for a remedy to name")
		require.NotEmpty(t, verdict.reason,
			"the exclusion carries its reason, like every other protected verdict")
	}
	require.True(t, protected.protects(os.Getpid()),
		"the reaper's own pid stays positively protected")
	require.Contains(t, warnBuf.String(), "process-table snapshot is missing",
		"a non-empty capture with no table IS a kill-path anomaly — the warning names the new consequence")
}

// The common harmless case: the session was already gone, so the capture
// failed and handed in an empty set. There is nothing to protect, and nothing
// that sounds like a kill-path warning may be logged.
func TestReapProtectedSetNilSnapshotEmptyCaptureSaysNothing(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	protected := reapProtectedSet(nil, nil, nil)

	require.Equal(t, selfProtectedSet(), protected,
		"an empty capture narrows protection to the reaper's own pid — there was nothing else to protect")
	require.Empty(t, warnBuf.String(),
		"a missing snapshot with an empty capture must not log a kill-path warning")
	require.Empty(t, infoBuf.String())
}

// The kill path itself: the nil-protected entry recompute reads the table, the
// read fails, and the captured set is NON-empty — the shape the production
// warning on 2026-11-23 proved reachable. On master every captured process
// was signalable. Now nothing is signalled, and every still-live captured
// process is reported as a survivor the way an unprovable-ancestry process
// already is, so the caller's cleanup verdict refuses.
func TestReapSessionProcessesNilSnapshotSignalsNothingAndReportsSurvivors(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	calls := failReapEntrySnapshot(t, errors.New("injected process-table read failure"))

	// The signal seam: proctree.Kill is the one syscall every signal the reap
	// delivers funnels through (signalIf -> teardownreq -> proctree.Signal ->
	// Kill), so recording it reads "was anything signalled" directly.
	var killed []int
	realKill := proctree.Kill
	proctree.Kill = func(pid int, sig syscall.Signal) error {
		killed = append(killed, pid)
		return realKill(pid, sig)
	}
	t.Cleanup(func() { proctree.Kill = realKill })

	survivor := spawnRequesterSleeper(t)
	remaining := reapSessionProcesses(reapOnRequest, "af_nil_snapshot",
		[]proctree.Process{survivor}, 200*time.Millisecond, 300*time.Millisecond, nil, nil)

	require.Equal(t, int32(1), calls.Load(),
		"the entry recompute reads the table exactly once for a non-empty capture")
	require.Empty(t, killed,
		"with the table missing NOTHING in the captured set may be signalled")
	require.Equal(t, []proctree.Process{survivor}, remaining,
		"the captured process is reported alive and unattributable — the caller refuses, never reads 'nothing found'")
	require.True(t, proctree.AliveSame(survivor),
		"the captured process was never touched")
	require.Contains(t, warnBuf.String(), "process-table snapshot is missing")
	require.Contains(t, infoBuf.String(), "excluding it from signalling",
		"each exclusion is logged at INFO like every other unproven-ancestry protection")
}

// The same entry path with an EMPTY capture: the session was already gone, so
// there is nothing to protect — the table is not even read and nothing is
// logged.
func TestReapSessionProcessesEmptyCaptureNeverReadsTheTable(t *testing.T) {
	var infoBuf, warnBuf logtest.Buffer
	redirectReapLogs(t, &infoBuf, &warnBuf)

	calls := failReapEntrySnapshot(t, errors.New("injected process-table read failure"))

	remaining := reapSessionProcesses(reapOnRequest, "af_nil_snapshot_empty",
		nil, time.Millisecond, time.Millisecond, nil, nil)

	require.Zero(t, calls.Load(),
		"an empty capture has nothing to protect — the process table is never read")
	require.Empty(t, remaining)
	require.Empty(t, warnBuf.String(),
		"a missing snapshot with an empty capture must not log a kill-path warning")
	require.Empty(t, infoBuf.String())
}
