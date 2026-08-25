package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The outcome an archive hook reports must be the outcome it HAD (#3407). The
// hook runs at the teardown chokepoint of a committed operation, and this single
// error string is the whole of what an operator learns about it — surfaced as the
// `warning` on a successful archive, and logged. A wrong one sends them looking
// for a hook that hung when what actually happened was a hook that finished, or a
// hook that failed for a nameable reason.
//
// The misreport came from the ordering of two checks after cmd.Run(). ctx.Err()
// was consulted first, so ANY deadline that had elapsed by then produced "timed
// out" — including the deadline that elapses while the run is blocked on
// WaitDelay cleanup, long after the hook itself exited. That window is not
// exotic: a hook that backgrounds anything inheriting stdout (`sleep 30 &`,
// a detached prune, a daemonizing tool) exits immediately and leaves Run waiting
// out the full WaitDelay on the pipe.

// withArchiveHookTimeout shortens the hook deadline for one test. The window the
// bug lives in is bounded by onArchiveHookWaitDelay, so reaching it at all needs
// a deadline shorter than that — impossible against the 30-minute production
// value.
func withArchiveHookTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := onArchiveHookTimeout
	onArchiveHookTimeout = d
	t.Cleanup(func() { onArchiveHookTimeout = original })
}

func archiveHookContext(t *testing.T) onArchiveHookContext {
	t.Helper()
	return onArchiveHookContext{
		sessionID:   "test-id",
		title:       "test-session",
		repoRoot:    t.TempDir(),
		worktree:    t.TempDir(),
		archivePath: t.TempDir(),
	}
}

// The headline case: the hook succeeded, and only its straggler's grip on the
// capture pipe carried the run past the deadline. Reporting that as a timeout
// tells the operator their cleanup command hung when it had in fact completed.
func TestArchiveHook_DeadlineCrossedDuringCleanupIsStillSuccess(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf("printf output; touch %q; sleep 30 >&1 2>&1 &", marker))
	withArchiveHookTimeout(t, 500*time.Millisecond)

	err := runOnArchiveHook(archiveHookContext(t))

	require.NoError(t, err, "the hook exited successfully; only pipe cleanup crossed the deadline")
	assert.FileExists(t, marker, "the hook must actually have run, or this test proves nothing")
}

// The same window, with the hook FAILING. A pure reordering of the two checks
// leaves this one misreported: the error is an *exec.ExitError rather than
// ErrWaitDelay, so it fell through to the elapsed deadline and printed "timed
// out" — burying the one thing the operator can act on. The exit status and the
// hook's own output are the outcome; the clock is not.
func TestArchiveHook_ExitStatusSurvivesADeadlineCrossedDuringCleanup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeOnArchiveCommand(t, "printf 'prune failed loudly'; sleep 30 >&1 2>&1 & exit 23")
	withArchiveHookTimeout(t, 500*time.Millisecond)

	err := runOnArchiveHook(archiveHookContext(t))

	require.Error(t, err, "a hook that exits 23 failed, whatever the clock did")
	assert.Contains(t, err.Error(), "exit status 23",
		"the operator must be told what the hook actually reported")
	assert.Contains(t, err.Error(), "prune failed loudly",
		"the hook's own output is the diagnosis and must not be replaced by a timeout")
	assert.NotContains(t, err.Error(), "timed out",
		"the hook exited on its own terms; nothing timed out")
}

// The guard against fixing the misreport by never reporting a timeout: a hook
// that is genuinely still running when the deadline fires is killed, and that IS
// a timeout. ProcessState.Exited() is what separates the two — false here,
// because the deadline's SIGKILL ended the shell rather than the shell exiting.
func TestArchiveHook_HookRunningAtTheDeadlineIsStillATimeout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeOnArchiveCommand(t, "printf 'still working'; sleep 30")
	withArchiveHookTimeout(t, 500*time.Millisecond)

	err := runOnArchiveHook(archiveHookContext(t))

	require.Error(t, err, "a hook that never finished must be reported")
	assert.Contains(t, err.Error(), "timed out after 500ms",
		"a hook still running at the deadline is exactly what the timeout message is for")
	assert.Contains(t, err.Error(), "still working",
		"the timeout must still carry whatever the hook managed to say")
}

// End to end through the surface an operator actually reads. A committed archive
// carries its hook's outcome as a warning, so a false timeout is not an internal
// detail — it is what `af sessions archive` prints, and what the daemon log keeps.
func TestArchiveSession_StragglerHookCommitsWithoutAFalseTimeoutWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf("printf pruned; touch %q; sleep 30 >&1 2>&1 &", marker))
	withArchiveHookTimeout(t, 500*time.Millisecond)

	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.NoError(t, err, "the hook completed; the archive must report a clean outcome")
	assert.FileExists(t, marker, "the hook must actually have run")
	assert.Equal(t, inst.ID, archived.ID)
	assert.False(t, exists(srcPath), "the archive itself must still have committed")
	assert.True(t, exists(archivedPath))
}
