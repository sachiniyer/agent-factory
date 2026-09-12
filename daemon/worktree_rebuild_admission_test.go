package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortenWorktreeAdmissionWait(t *testing.T) {
	t.Helper()
	previousTimeout, previousPoll := opLockTimeout, opLockPollInterval
	opLockTimeout = 25 * time.Millisecond
	opLockPollInterval = time.Millisecond
	t.Cleanup(func() {
		opLockTimeout = previousTimeout
		opLockPollInterval = previousPoll
	})
}

func TestRestoreArchivedAdmissionWaitIsBoundedBeforeFence(t *testing.T) {
	manager, repoID, _, inst := seedArchivedForFence(t, "bounded-admission")
	archivePath := inst.GetWorktreePath()
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	admission.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			admission.Unlock()
		}
	})
	shortenWorktreeAdmissionWait(t)

	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreArchived(RestoreArchivedRequest{
			Title: inst.Title, RepoID: repoID,
		})
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorContains(t, err, "timed out")
		require.ErrorContains(t, err, "worktree operation")
	case <-time.After(time.Second):
		// Release before failing so a regression cannot strand the test goroutine
		// until the package-wide timeout.
		locked = false
		admission.Unlock()
		t.Fatal("archived restore waited indefinitely for worktree admission")
	}
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "a wait refusal must precede the restore fence")
	assert.True(t, inst.IsArchived())
	assert.DirExists(t, archivePath, "a wait refusal must leave the archive untouched")
}

func TestRestoreLostSessionsSkipsContendedWorktreeAdmission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "lost-admission", backend, true, session.Lost)
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	admission.Lock()

	manager.RestoreLostSessions()
	assert.Zero(t, backend.recoverCalls(), "the operational poll must not rebuild while create owns admission")
	assert.Equal(t, session.Lost, inst.GetStatus())

	admission.Unlock()
	manager.RestoreLostSessions()
	assert.Equal(t, 1, backend.recoverCalls(), "a later poll may recover once admission is free")
}

func TestRestoreLostSessionAdmissionWaitIsBoundedBeforeFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "manual-admission", backend, true, session.Lost)
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	admission.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			admission.Unlock()
		}
	})
	shortenWorktreeAdmissionWait(t)

	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreSession(RestoreSessionRequest{
			Title: inst.Title, RepoID: repoID,
		})
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "timed out") && strings.Contains(err.Error(), "worktree operation"), err)
	case <-time.After(time.Second):
		locked = false
		admission.Unlock()
		t.Fatal("manual Lost restore waited indefinitely for worktree admission")
	}
	assert.Zero(t, backend.recoverCalls())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "a wait refusal must precede the recover fence")
	assert.Equal(t, session.Lost, inst.GetStatus())
}
