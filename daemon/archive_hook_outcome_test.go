package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

// The outcome an archive hook reports must be the outcome it HAD (#3407). The
// hook runs at the teardown chokepoint of a committed operation, and this single
// error string is the whole of what an operator learns about it — surfaced as the
// `warning` on a successful archive, and logged. A wrong one sends them looking
// for a hook that hung when what actually happened was a hook that finished, or a
// hook that failed for a nameable reason.
//
// The original misreport came from a capture pipe held by a backgrounded child:
// cmd.Run waited on that pipe after the hook shell exited, and a deadline that
// elapsed in the wait replaced the hook's real outcome. #4010's direct output
// file removes that wait altogether. The tests below retain the straggler and
// pin both halves of the replacement contract: return promptly with the shell's
// real outcome, but still kill the straggler before the worktree moves.

// withArchiveHookTimeout shortens the hook deadline for one test.
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

// A successful hook is done when its shell exits. A backgrounded child may keep
// the output file open, but unlike a capture pipe that descriptor cannot delay
// cmd.Run until the deadline.
func TestArchiveHook_OutputFileStragglerDoesNotDelaySuccess(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf("printf output; touch %q; sleep 30 >&1 2>&1 &", marker))
	withArchiveHookTimeout(t, 500*time.Millisecond)

	start := time.Now()
	err := runOnArchiveHook(archiveHookContext(t))
	elapsed := time.Since(start)

	require.NoError(t, err, "the hook exited successfully; its output-file straggler is cleanup, not failure")
	assert.FileExists(t, marker, "the hook must actually have run, or this test proves nothing")
	assert.Less(t, elapsed, onArchiveHookWaitDelay,
		"a descendant holding the direct output file must not recreate the old capture-pipe wait")
}

// The same shape with a failing shell: the exit status and output are the
// outcome, and an output-file straggler cannot hold that answer past the clock.
func TestArchiveHook_OutputFileStragglerDoesNotHideExitStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeOnArchiveCommand(t, "printf 'prune failed loudly'; sleep 30 >&1 2>&1 & exit 23")
	withArchiveHookTimeout(t, 500*time.Millisecond)

	start := time.Now()
	err := runOnArchiveHook(archiveHookContext(t))
	elapsed := time.Since(start)

	require.Error(t, err, "a hook that exits 23 failed, whatever the clock did")
	assert.Contains(t, err.Error(), "exit status 23",
		"the operator must be told what the hook actually reported")
	assert.Contains(t, err.Error(), "prune failed loudly",
		"the hook's own output is the diagnosis and must not be replaced by a timeout")
	assert.NotContains(t, err.Error(), "timed out",
		"the hook exited on its own terms; nothing timed out")
	assert.Less(t, elapsed, onArchiveHookWaitDelay,
		"a descendant holding the direct output file must not recreate the old capture-pipe wait")
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

func TestArchiveHook_FailureSurfacesBoundedTailAndLogPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	payloadPath := filepath.Join(t.TempDir(), "hook-output")
	payload := "discarded beginning\n" + strings.Repeat("x", hooklog.TailLimit) + "\nkept ending\n"
	require.NoError(t, os.WriteFile(payloadPath, []byte(payload), 0o600))
	writeOnArchiveCommand(t, fmt.Sprintf("cat %q; exit 23", payloadPath))

	err := runOnArchiveHook(archiveHookContext(t))

	require.Error(t, err)
	logs, globErr := filepath.Glob(filepath.Join(home, "logs", "hooks", "on-archive-*.log"))
	require.NoError(t, globErr)
	require.Len(t, logs, 1)
	for _, want := range []string{"exit status 23", logs[0], "[output truncated to last 65536 bytes]", "kept ending"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, err.Error(), "discarded beginning",
		"the surfaced diagnostic must stay bounded")
	full, readErr := os.ReadFile(logs[0])
	require.NoError(t, readErr)
	assert.Contains(t, string(full), "discarded beginning")
	assert.Contains(t, string(full), "kept ending")
}

func TestArchiveHook_SuccessRemovesOutputLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeOnArchiveCommand(t, "printf success")

	require.NoError(t, runOnArchiveHook(archiveHookContext(t)))

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "on-archive-*.log"))
	require.NoError(t, err)
	assert.Empty(t, logs, "successful archive hook retained an output log")
}

// End to end through the surface an operator actually reads. A committed archive
// carries its hook's outcome as a warning, so a false timeout is not an internal
// detail — it is what `af sessions archive` prints, and what the daemon log keeps.
func TestArchiveSession_OutputFileStragglerCommitsWithoutAFalseTimeoutWarning(t *testing.T) {
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
