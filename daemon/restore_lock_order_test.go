package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The documented lock order (startLockForRepo) is op lock -> repository start
// lock -> worktree admission -> m.mu, and CreateSession follows it: it holds the
// start lock and then WAITS on admission. Restore used to take admission first and
// then persist through the start lock, so a restore overlapping a create was one
// unbounded wait from a deadlock; only create's bounded admission wait kept it a
// stall (#6197).

// lockOrderProbe watches every durable row write for the one thing the order
// forbids: the start lock taken while worktree admission is held.
//
// A write that goes through the start lock reaches persistInstanceData with the
// start lock held, and admission is held by the restore that issued it, so both
// locks read as busy at the hook. Nothing else in these tests takes either lock,
// so a busy lock is the restore's.
type lockOrderProbe struct {
	mu         sync.Mutex
	underAdmit int
	violations []string
}

func installLockOrderProbe(t *testing.T, manager *Manager, repoID string) *lockOrderProbe {
	t.Helper()
	p := &lockOrderProbe{}
	// Resolved here, not in the hook: both lookups take m.mu, and a writer that
	// already holds it may reach persistInstanceData.
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	start := manager.startLockForRepo(repoID)
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id != repoID {
			return nil
		}
		admissionHeld := lockBusy(admission)
		startHeld := lockBusy(start)
		p.mu.Lock()
		defer p.mu.Unlock()
		if admissionHeld {
			p.underAdmit++
		}
		if admissionHeld && startHeld {
			p.violations = append(p.violations, data.Title)
		}
		return nil
	}
	return p
}

// lockBusy reports whether mu is currently held, without keeping it.
func lockBusy(mu *sync.Mutex) bool {
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

func (p *lockOrderProbe) require(t *testing.T, wantUnderAdmission bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.violations,
		"a row write took the repository start lock while worktree admission was held — the reverse of "+
			"create's start -> admission order, which deadlocks against a create (#6197)")
	if wantUnderAdmission {
		// Without this a restore that stopped writing under admission altogether
		// would satisfy the check above for no reason.
		require.NotZero(t, p.underAdmit,
			"no row write happened under admission, so this scenario no longer exercises the window")
	}
}

func TestRestoreArchived_PersistsWithoutTheStartLockUnderAdmission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	probe := installLockOrderProbe(t, manager, repoID)
	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	probe.require(t, true)
}

func TestRestoreSession_LostPersistsWithoutTheStartLockUnderAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"recover succeeds", nil},
		{"recover fails", errors.New("tmux spawn failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: tc.failure}
			registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

			probe := installLockOrderProbe(t, manager, repoID)
			_, _, _ = manager.RestoreSession(RestoreSessionRequest{Title: "stranded", RepoID: repoID})
			require.Equal(t, 1, backend.recoverCalls(), "the restore must reach Recover")
			probe.require(t, false)
		})
	}
}

func TestRestoreLostSessions_PersistsWithoutTheStartLockUnderAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"recover succeeds", nil},
		{"recover fails", errors.New("tmux spawn failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			zeroRestoreBackoff(t)
			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: tc.failure}
			registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

			probe := installLockOrderProbe(t, manager, repoID)
			manager.RestoreLostSessions()
			require.Equal(t, 1, backend.recoverCalls(), "the restore must reach Recover")
			probe.require(t, false)
		})
	}
}

// TestRestoreArchived_OverlappingACreateAtItsStartLock is the overlap oracle: a
// create that already holds the repository start lock and is about to wait on
// admission, against a restore of an archived session.
//
// The restore must be able to make progress, and must hand admission back, while
// the start lock is held elsewhere — otherwise the create's admission wait and the
// restore's start-lock wait are each other's cause. Every wait is under a deadline
// so a regression fails with a message instead of hanging the package.
func TestRestoreArchived_OverlappingACreateAtItsStartLock(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	restored, perr := sessiongit.RestoreWorktreePath(repoPath, "worker", inst.GetBranch())
	require.NoError(t, perr)

	// The restore's commit of the new location is the first write that needs
	// ordering against a create. Signal it, then let it through.
	committed := make(chan struct{})
	var once sync.Once
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "worker" && data.Worktree.WorktreePath == restored {
			once.Do(func() { close(committed) })
		}
		return nil
	}

	// The create, past its start lock.
	startLock := manager.startLockForRepo(repoID)
	startLock.Lock()
	startHeld := true
	defer func() {
		if startHeld {
			startLock.Unlock()
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, _, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
		done <- rerr
	}()

	const deadline = 20 * time.Second
	select {
	case <-committed:
	case <-time.After(deadline):
		t.Fatalf("the restore never recorded its new location within %v while a create held the start lock: "+
			"it is waiting on the start lock while holding worktree admission, which is create's own next wait (#6197)", deadline)
	}

	// The create's next step. Restore releases admission right after the respawn,
	// so it must be obtainable even though the start lock is still held.
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	acquired, _ := lockWithin(admission, deadline)
	require.True(t, acquired, "the create could not take worktree admission within %v: the restore holds it while "+
		"waiting for the start lock the create holds (#6197)", deadline)
	admission.Unlock()

	startLock.Unlock()
	startHeld = false
	select {
	case rerr := <-done:
		require.NoError(t, rerr)
	case <-time.After(deadline):
		t.Fatalf("the restore did not finish within %v of the start lock being released", deadline)
	}
}
