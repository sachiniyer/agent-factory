package daemon

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// #3452. The on-archive hook runs at the teardown chokepoint, BEFORE the worktree
// moves, so every failure below it is reachable with a broken cleanup command
// behind it. Two of those failures roll the committed archive back — and rolling
// back SUCCESSFULLY was the case that dropped the hook error entirely: not
// returned, not logged, so the buffered hook output died with the request. The
// operator's cleanup command may already have deleted a remote branch or torn down
// a container; being told only "VS Code teardown was not confirmed" sends them
// looking at the wrong thing, and every retry reports the same single cause.
//
// The rule these pin is the #3233-#3237 outcome-truthfulness rule: report what
// ACTUALLY happened. Here that is a compound outcome — the archive rolled back
// cleanly AND the hook failed — so both assertions matter, and the primary cause
// staying primary is asserted alongside the hook, so a later refactor cannot
// silently trade one truth for the other.

// captureArchiveWarnings redirects WarningLog for one test. The log half is not a
// nicety: an archive driven by an `af tasks` run has no one reading the returned
// error, so the daemon log is the only place the diagnosis survives.
func captureArchiveWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var warnings bytes.Buffer
	previous := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
	return &warnings
}

// plantStuckEditorAfterTeardown makes the FINAL VS Code sweep — the one below the
// archive commit — fail, while leaving the pre-teardown sweep to succeed. A
// process group that accepts both signals and never exits is what the supervisor
// reports as an unconfirmed exit; planting it from inside archiveTeardown is what
// puts the failure after the commit rather than before any teardown started.
func plantStuckEditorAfterTeardown(t *testing.T, manager *Manager, inst *session.Instance, key, worktree string) {
	t.Helper()
	cmd, _ := startOwnedSleep(t)
	original := archiveTeardown
	archiveTeardown = func(target *session.Instance, dest string, claim sessiongit.RelocationClaim, beforeMove func() error) (error, error) {
		hookErr, err := original(target, dest, claim, beforeMove)
		if err != nil {
			return hookErr, err
		}
		manager.vscode.mu.Lock()
		manager.vscode.servers[key] = &vscodeServer{
			worktree: worktree, instanceID: inst.ID, cmd: cmd, exited: make(chan struct{}),
			stopGrace: 10 * time.Millisecond,
			killGroup: func(int, syscall.Signal) error { return nil },
		}
		manager.vscode.mu.Unlock()
		return hookErr, nil
	}
	t.Cleanup(func() { archiveTeardown = original })
}

// TestArchiveSession_FinalVSCodeStopFailureRollbackSurfacesHookFailure is #3452's
// first arm: the archive committed, the final editor sweep could not confirm exit,
// the rollback home SUCCEEDED — and the failed hook must still reach the operator.
func TestArchiveSession_FinalVSCodeStopFailureRollbackSurfacesHookFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, origPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	writeOnArchiveCommand(t, "printf 'prune failed loudly\\n'; exit 23")
	plantStuckEditorAfterTeardown(t, manager, inst, daemonInstanceKey(repoID, "worker"), origPath)
	warnings := captureArchiveWarnings(t)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.Equal(t, origPath, inst.GetWorktreePath(),
		"precondition: the rollback-SUCCESS branch must be the one that ran")
	require.Equal(t, session.Lost, inst.GetStatus(),
		"precondition: a successful rollback leaves the session Lost for retry")
	assert.Contains(t, err.Error(), "rolled the archive back",
		"the rollback succeeded and the message must keep saying so")
	assert.Contains(t, err.Error(), "final VS Code editor teardown",
		"the teardown failure is what the caller acts on and must stay the primary cause")
	assert.Contains(t, err.Error(), "on-archive hook",
		"a clean rollback must not swallow the hook failure the operator has to fix")
	assert.Contains(t, err.Error(), "exit status 23",
		"the hook's own cause is the diagnosis, not the fact that it failed")
	assert.Contains(t, err.Error(), "prune failed loudly",
		"the hook's own output must survive to the operator")
	assert.Contains(t, warnings.String(), "on-archive hook",
		"a task-driven archive has no caller reading the error; the log is the only diagnosis left")
	assert.Contains(t, warnings.String(), "exit status 23")
}

// TestArchiveSession_PersistFailureRollbackSurfacesHookFailure is the second arm:
// same committed archive, same successful rollback, but the durable write is what
// failed. The hook error is dropped by the same omission and must be surfaced by
// the same rule.
func TestArchiveSession_PersistFailureRollbackSurfacesHookFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, origPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	writeOnArchiveCommand(t, "printf 'prune failed loudly\\n'; exit 23")

	previous := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		// Only the archive's durable commit fails; the rollback's own best-effort
		// persist goes through persistInstance, so the road home stays open.
		return errors.New("forced persist failure")
	}
	t.Cleanup(func() { archivePersist = previous })
	warnings := captureArchiveWarnings(t)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.Equal(t, origPath, inst.GetWorktreePath(),
		"precondition: the rollback-SUCCESS branch must be the one that ran")
	require.Equal(t, session.Lost, inst.GetStatus(),
		"precondition: a successful rollback leaves the session Lost to be restored in place")
	assert.Contains(t, err.Error(), "rolled it back",
		"the rollback succeeded and the message must keep saying so")
	assert.Contains(t, err.Error(), "forced persist failure",
		"the durable write failure must stay the primary cause")
	assert.Contains(t, err.Error(), "on-archive hook",
		"a clean rollback must not swallow the hook failure the operator has to fix")
	assert.Contains(t, err.Error(), "exit status 23")
	assert.Contains(t, err.Error(), "prune failed loudly")
	assert.Contains(t, warnings.String(), "on-archive hook",
		"a task-driven archive has no caller reading the error; the log is the only diagnosis left")
	assert.Contains(t, warnings.String(), "exit status 23")
}

// TestArchiveSession_UndurableUnrollableArchiveSurfacesHookFailure is the adjacent
// drop the #3452 audit turned up. keepUnrollableArchiveCommitted takes hookErr and
// folds it into the committed warning — but its two PLAIN-error returns, taken
// when the committed claim cannot be made or made durable, dropped it exactly as
// the rollback-success returns did. This is the end-to-end one: the rollback home
// is blocked by a file squatting on the vacated source and the durable write never
// heals, so the archive exits through that plain double-failure shape.
func TestArchiveSession_UndurableUnrollableArchiveSurfacesHookFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	writeOnArchiveCommand(t, "printf 'prune failed loudly\\n'; exit 23")

	previous := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error {
		_ = os.WriteFile(srcPath, []byte("in the way"), 0644)
		return errors.New("forced persist failure")
	}
	t.Cleanup(func() { archivePersist = previous })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.Contains(t, err.Error(), "could not be written durably",
		"precondition: the plain double-failure branch must be the one that ran")
	assert.Contains(t, err.Error(), "could not roll it back",
		"the rollback failure must stay part of the primary cause")
	assert.Contains(t, err.Error(), "on-archive hook",
		"the plain shape carries no committed warning, so this return owes the hook failure itself")
	assert.Contains(t, err.Error(), "exit status 23")
}

// TestKeepUnrollableArchiveCommitted_RefusedClaimSurfacesHookFailure covers the
// other plain return of that helper: the bytes provably left the archive, so the
// committed claim is refused. Reaching it end to end needs a move home that lands
// the bytes and THEN fails registration repair (session/git owns that fixture), so
// this drives the manager method the archive returns verbatim — the same shape
// TestKeepUnrollableArchiveCommitted_RefusesWhenBytesLeftTheArchive uses.
func TestKeepUnrollableArchiveCommitted_RefusedClaimSurfacesHookFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	require.NoError(t, inst.Transition(session.BeginArchive()))
	require.NoError(t, inst.Transition(session.CommitArchive()))

	dest, derr := archivedWorktreePath(repoID, "worker")
	require.NoError(t, derr)
	require.Equal(t, srcPath, inst.GetWorktreePath(),
		"precondition: the bytes are at the pre-archive path, not the archive")

	cause := errors.New("registration repair failed after the bytes moved home")
	hookErr := errors.New("exit status 23: prune failed loudly")
	_, _, err := manager.keepUnrollableArchiveCommitted(repoID, dest, inst, hookErr, cause)

	require.ErrorIs(t, err, cause, "the refused-claim cause must stay the primary cause")
	assert.Contains(t, err.Error(), "on-archive hook",
		"refusing the committed claim must not also discard the hook failure — and the words the "+
			"operator greps for come from the message, not from the hook error's own text")
	assert.Contains(t, err.Error(), "exit status 23")
}
