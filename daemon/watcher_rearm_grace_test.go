package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7423: #7404 made an exit 0 wait out a grace before it records "stopped",
// with doneCh still open. reconcile tested w.finished() alone, so a re-arm
// landing in those 5 seconds kept the watcher, the grace expired, "stopped"
// was recorded, and the documented re-arm gesture did nothing. A watcher in
// the clean-exit grace now counts as terminal for a re-arm.

func awaitCleanExitGrace(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never entered the clean-exit grace", what)
	}
}

func cleanExitGraceSignal(t *testing.T, taskID string) <-chan struct{} {
	t.Helper()
	entered := make(chan struct{}, 4)
	onWatchGrace(t, taskID, func(grace lifecycleGrace) {
		if grace == graceCleanExit {
			entered <- struct{}{}
		}
	})
	return entered
}

// TestWatcherReArmDuringTheCleanExitGraceReplacesIt: both re-arm gestures —
// the full reload ReloadTasks drives, and an explicit enable of the task —
// replace a watcher that is only waiting out its clean-exit grace. The grace
// ends on the stop and records nothing, and a replacement runs.
func TestWatcherReArmDuringTheCleanExitGraceReplacesIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rearm func(s *watcherSupervisor, taskID string) error
	}{
		{"ReloadTasks", func(s *watcherSupervisor, _ string) error { return s.Reload() }},
		{"enable gesture", func(s *watcherSupervisor, taskID string) error { return s.ReloadScoped(oneWatchTask(taskID, true)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := captureInfo(t)
			const id = "d7423001"
			countFile := filepath.Join(t.TempDir(), "runs")
			entered := cleanExitGraceSignal(t, id)
			s, rec := newTestSupervisor(t, staticTasks(watchTask(id, countingFailScript(countFile, 0), t.TempDir())))
			s.stopGrace = watcherStopGrace // the production grace: the re-arm lands well inside it
			require.NoError(t, s.Reload())
			awaitCleanExitGrace(t, entered, "the first run")

			require.NoError(t, tc.rearm(s, id))
			waitUntil(t, 3*time.Second, "a replacement run after the re-arm", func() bool { return countRuns(t, countFile) == 2 })
			awaitCleanExitGrace(t, entered, "the replacement")
			require.Empty(t, rec.statusesSnapshot(), "the re-armed run recorded stopped")
			require.Contains(t, info.String(), "watch task "+id+": watch command exited cleanly as this task was stopped or reloaded; not recording it as stopped")
		})
	}
}

// TestWatcherNonReArmWriteDuringTheCleanExitGraceLeavesIt is the guard: a write
// that names the task but is not a re-arm gesture leaves a terminal watcher
// alone (#3837), so the grace runs out and "stopped" is recorded as before.
func TestWatcherNonReArmWriteDuringTheCleanExitGraceLeavesIt(t *testing.T) {
	const id = "d7423002"
	countFile := filepath.Join(t.TempDir(), "runs")
	entered := cleanExitGraceSignal(t, id)
	s, rec := newTestSupervisor(t, staticTasks(watchTask(id, countingFailScript(countFile, 0), t.TempDir())))
	s.stopGrace = 500 * time.Millisecond
	require.NoError(t, s.Reload())
	awaitCleanExitGrace(t, entered, "the run")

	require.NoError(t, s.ReloadScoped(oneWatchTask(id, false)))
	waitUntil(t, 3*time.Second, "stopped to be recorded", func() bool {
		statuses := rec.statusesSnapshot()
		return len(statuses) == 1 && statuses[0] == id+":stopped"
	})
	require.Equal(t, 1, countRuns(t, countFile), "a write that is not a re-arm replaced a terminal watcher")
}
