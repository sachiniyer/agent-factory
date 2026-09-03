package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #3600: the op-lock WAIT, the one gap left in front of the restore fence after
// #3597 made that fence coextensive with the claim from the op-lock onward.
//
// claimRestoreOperation used to run BEFORE lockSessionOperationWithin, so a manual
// restore that arrived while the automatic Lost-restore loop held the same
// per-session lock across its own UNFENCED probe/preserve phase installed
// killsInFlight and then parked for up to opLockTimeout (30s) with the row still
// at {LiveLost, OpNone}. canKillFor reads the op axis alone, so the row went on
// advertising a Kill that killSessionRequestedBy refused at the admission gate
// with "kill already in progress for session X" — an operation the user never
// started. The same false affordance as #3586/#3596, one level narrower.
//
// Option 2 (the maintainer's decision on #3600) removes the window where it is
// created rather than projecting around it: the op-lock is acquired FIRST, so the
// claim and the OpRestoring raise are adjacent and no observer can see the row
// claimed-but-unfenced. Both surfaces are correct for the reason they were
// designed to be — Instance.CanKill() (ui/menu.go, app/handle_actions.go) and the
// projected can_kill (web/src/ui.ts isKillableSession) both read the op axis, and
// the op axis is the truth.
//
// The trade, which these also pin: a restore racing a delete or another
// exclusive operation now reports its refusal AFTER the wait instead of before
// it, so the refusal names the wait.

// withOpLockTimeout shortens the bounded op-lock wait for one test.
func withOpLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := opLockTimeout
	opLockTimeout = d
	t.Cleanup(func() { opLockTimeout = prev })
}

// holdOpLock takes a session's operation lock the way a peer operation does and
// hands back the release. It is the stand-in for whichever holder is in front of
// the restore — the automatic loop's probe/preserve phase, an archive, a kill.
func holdOpLock(t *testing.T, m *Manager, key string) (release func()) {
	t.Helper()
	opLock := m.opLockFor(key)
	opLock.Lock()
	var once bool
	release = func() {
		if !once {
			once = true
			opLock.Unlock()
		}
	}
	t.Cleanup(release)
	return release
}

// parkRestoreAtItsOpLock substitutes the #3600 hook so the next manual restore
// stops at the head of its operation-lock wait. In the PRE-FIX ordering that
// point sits AFTER claimRestoreOperation, so everything the assertions read
// there is what a client would have seen for the whole wait.
func parkRestoreAtItsOpLock(t *testing.T) (parked <-chan struct{}, resume func()) {
	t.Helper()
	at := make(chan struct{})
	go_ := make(chan struct{})
	var entered bool
	prev := beforeRestoreOperationLock
	beforeRestoreOperationLock = func() {
		if !entered {
			entered = true
			close(at)
			<-go_
		}
	}
	var released bool
	resume = func() {
		if !released {
			released = true
			close(go_)
		}
	}
	t.Cleanup(func() {
		resume()
		beforeRestoreOperationLock = prev
	})
	return at, resume
}

func awaitParkedRestore(t *testing.T, parked <-chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the manual restore never reached its operation-lock wait")
	}
}

// assertKillIsAdmittedButBoundedByThePeer is the #3600 assertion. The row is
// advertising Kill, so a Kill pressed now must be ADMITTED and then bounded by
// the peer that actually holds the lock — never turned away at the admission gate
// for an operation the user never started.
func assertKillIsAdmittedButBoundedByThePeer(t *testing.T, m *Manager, repoID, title string) {
	t.Helper()
	_, err := m.KillSession(KillSessionRequest{Title: title, RepoID: repoID})
	require.Error(t, err, "the peer holds the operation lock, so this Kill cannot complete")
	assert.NotContains(t, err.Error(), "already in progress",
		"the row advertises Kill while a manual restore is parked in front of the operation lock, but the "+
			"daemon refused that Kill at its ADMISSION gate — the restore's claim is held across a wait it "+
			"has not earned, so the only outcome of the advertised control is a message about an operation "+
			"the user never started (#3600)")
	assert.Contains(t, err.Error(), "timed out",
		"the Kill must be bounded by the peer that genuinely holds the operation lock")
	assert.Contains(t, err.Error(), "retry", "a bounded refusal has to be actionable")
}

// assertRowAdvertisesKill reads both surfaces a client asks.
func assertRowAdvertisesKill(t *testing.T, m *Manager, inst *session.Instance, key, when string) {
	t.Helper()
	require.Truef(t, inst.CanKill(), "Instance.CanKill() is false %s: the TUI hid a control this test is about", when)
	require.Truef(t, inst.ToInstanceData().CanKill, "the projected can_kill is false %s", when)
	require.Falsef(t, killGuardHeld(m, key),
		"the row advertises Kill %s, yet the daemon holds an exclusive-operation claim: the advertised "+
			"affordance and the admission gate disagree, which is exactly #3600", when)
}

// TestRestoreSession_OpLockWaitLeavesKillAdmitted is the issue reproduced with
// its own actors: the AUTOMATIC Lost-restore loop holds the per-session op lock
// across its unfenced liveness probe (daemon/lostrestore.go), and a manual
// restore arrives behind it.
func TestRestoreSession_OpLockWaitLeavesKillAdmitted(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 30*time.Second)
	zeroRestoreBackoff(t)
	withOpLockTimeout(t, 500*time.Millisecond)

	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersAlive, true)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "contested", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)
	key := daemonInstanceKey(repoID, "contested")

	autoDone := make(chan struct{})
	go func() {
		manager.RestoreLostSessions()
		close(autoDone)
	}()
	// The automatic loop is now inside its probe, holding the op lock with the row
	// still at {LiveLost, OpNone} — the holder #3600 names.
	sandbox.awaitProbe(t)

	parked, resume := parkRestoreAtItsOpLock(t)
	manualDone := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "contested", RepoID: repoID})
		manualDone <- err
	}()
	awaitParkedRestore(t, parked)

	assertRowAdvertisesKill(t, manager, inst, key, "while a manual restore waits for the operation lock")
	assertKillIsAdmittedButBoundedByThePeer(t, manager, repoID, "contested")

	resume()
	sandbox.releaseProbe()
	select {
	case <-manualDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the manual restore never returned")
	}
	select {
	case <-autoDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the automatic restore loop never returned")
	}
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the contended restore left the row busy: no poll, kill, archive or restore could touch it again")
}

// TestRestoreArchived_OpLockWaitLeavesKillAdmitted is the archived half. There is
// no automatic loop for an archived row, so the holder is a plain peer operation.
func TestRestoreArchived_OpLockWaitLeavesKillAdmitted(t *testing.T) {
	withOpLockTimeout(t, 500*time.Millisecond)

	manager, repoID, _, inst := seedArchivedForFence(t, "archived-contested")
	key := daemonInstanceKey(repoID, "archived-contested")
	releasePeer := holdOpLock(t, manager, key)

	parked, resume := parkRestoreAtItsOpLock(t)
	manualDone := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-contested", RepoID: repoID})
		manualDone <- err
	}()
	awaitParkedRestore(t, parked)

	assertRowAdvertisesKill(t, manager, inst, key, "while an archived restore waits for the operation lock")
	assertKillIsAdmittedButBoundedByThePeer(t, manager, repoID, "archived-contested")

	resume()
	select {
	case err := <-manualDone:
		require.Error(t, err, "the peer never released the lock, so the restore cannot have run")
	case <-time.After(30 * time.Second):
		t.Fatal("the archived restore never returned")
	}
	releasePeer()
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(), "a refused restore must leave the session shelved")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the refused restore left the row busy")
}

// TestRestoreSession_NoClaimIsHeldThroughTheOpLockWait watches the GENUINE wait
// rather than its head, and pins the other half of the decision: the wait is
// still a WAIT. Option 3 — refuse instead of waiting — was rejected as a product
// change, so a restore that queues behind a peer must still succeed once the peer
// lets go.
func TestRestoreSession_NoClaimIsHeldThroughTheOpLockWait(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "queued", backend, true, session.Lost)
	key := daemonInstanceKey(repoID, "queued")
	releasePeer := holdOpLock(t, manager, key)

	parked, resume := parkRestoreAtItsOpLock(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "queued", RepoID: repoID})
		done <- err
	}()
	awaitParkedRestore(t, parked)
	resume()

	// The peer holds the lock for the whole sampling window, so the restore cannot
	// get past it and every sample lands inside the wait.
	require.Never(t, func() bool { return killGuardHeld(manager, key) }, 250*time.Millisecond, 5*time.Millisecond,
		"the restore held an exclusive-operation claim while merely QUEUED behind a peer: every Kill, "+
			"archive, prompt and rename against the session is refused for the length of that wait, and the "+
			"row advertises Kill throughout (#3600)")
	require.True(t, inst.CanKill(), "the queued restore must leave the row's advertised Kill honest")

	releasePeer()
	select {
	case err := <-done:
		require.NoError(t, err, "a restore that waits for a peer must still run once the peer releases")
	case <-time.After(30 * time.Second):
		t.Fatal("the queued restore never returned")
	}
	assert.Equal(t, session.Running, inst.GetStatus(), "the restore that waited must have recovered the session")
	assert.Equal(t, 1, backend.recoverCalls(), "the queued restore must have reached its backend exactly once")
}

// TestRestore_DeleteFenceRaisedDuringTheOpLockWaitRefusesTheRestore is the first
// DeleteProject question the decision comment answered: a delete that STARTS
// during the wait is caught by the claim, because the claim now runs AFTER the
// lock and still checks projectDeletes. The restore refuses with the same
// "project is being deleted" it gave before — just after the wait instead of
// before it — and says how long it waited, so the delay reads as a wait and not
// as a hang.
//
// PRE-FIX this cannot even be set up: the restore's claim is already held while
// it waits, so DeleteProject's fail-closed gate refuses the delete outright and
// it never reaches its first mutation.
func TestRestore_DeleteFenceRaisedDuringTheOpLockWaitRefusesTheRestore(t *testing.T) {
	for _, tc := range restoreWaitCases() {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := tc.prepare(t, manager, repoID, repoPath)
			before := inst.LifecycleView()
			key := daemonInstanceKey(repoID, "worker")
			releasePeer := holdOpLock(t, manager, key)

			parked, resume := parkRestoreAtItsOpLock(t)
			restoreDone := make(chan error, 1)
			go func() { restoreDone <- tc.restore(manager, repoID) }()
			awaitParkedRestore(t, parked)
			resume() // the restore is now genuinely blocked on the peer's lock

			mutationReached := make(chan struct{})
			resumeDelete := make(chan struct{})
			orig := deregisterRootAgents
			deregisterRootAgents = func(...string) ([]string, error) {
				close(mutationReached)
				<-resumeDelete
				return nil, errors.New("forced stop before mutation")
			}
			t.Cleanup(func() { deregisterRootAgents = orig })

			deleteDone := make(chan error, 1)
			go func() {
				_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
				deleteDone <- err
			}()
			select {
			case <-mutationReached:
			case <-time.After(30 * time.Second):
				close(resumeDelete)
				t.Fatal("DeleteProject never raised its fence: a restore merely QUEUED behind a peer still " +
					"blocks the delete, so the claim is being held across a wait it has not earned (#3600)")
			}

			// The fence is up and the restore has not yet been admitted. Let it through.
			releasePeer()
			var restoreErr error
			select {
			case restoreErr = <-restoreDone:
			case <-time.After(30 * time.Second):
				close(resumeDelete)
				t.Fatal("the restore never returned after the peer released its operation lock")
			}
			close(resumeDelete)
			require.Error(t, <-deleteDone)

			require.Error(t, restoreErr, "a restore must not cross an active project-deletion fence")
			assert.Contains(t, restoreErr.Error(), "delet",
				"the refusal must still name the deletion that caused it")
			assert.Contains(t, restoreErr.Error(), "after waiting",
				"a refusal reported at the end of a wait has to say it waited, or a 30s delay reads as a hang")
			assert.Equal(t, before, inst.LifecycleView(), "the refused restore must not mutate the session")
		})
	}
}

// TestRestore_InstanceReplacedDuringTheOpLockWaitRefusesTheRestore is the second
// answered question: a delete that COMPLETES during the wait is caught by the
// re-read of m.instances[key] after the lock. The residue a completed delete
// leaves for its in-place/external sessions is exactly this — the row gone from
// the manager map — so that is what the wait is released into.
func TestRestore_InstanceReplacedDuringTheOpLockWaitRefusesTheRestore(t *testing.T) {
	for _, tc := range restoreWaitCases() {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := tc.prepare(t, manager, repoID, repoPath)
			before := inst.LifecycleView()
			key := daemonInstanceKey(repoID, "worker")
			releasePeer := holdOpLock(t, manager, key)

			parked, resume := parkRestoreAtItsOpLock(t)
			restoreDone := make(chan error, 1)
			go func() { restoreDone <- tc.restore(manager, repoID) }()
			awaitParkedRestore(t, parked)
			resume()

			manager.mu.Lock()
			delete(manager.instances, key)
			manager.mu.Unlock()
			releasePeer()

			select {
			case err := <-restoreDone:
				require.Error(t, err, "a restore must not act on a session the delete already removed")
				assert.Contains(t, err.Error(), "changed state before restore could start")
			case <-time.After(30 * time.Second):
				t.Fatal("the restore never returned after the peer released its operation lock")
			}
			assert.Equal(t, before, inst.LifecycleView(), "the refused restore must not mutate the session")
		})
	}
}

type restoreWaitCase struct {
	name    string
	prepare func(*testing.T, *Manager, string, string) *session.Instance
	restore func(*Manager, string) error
}

// restoreWaitCases covers both manual restore routes, which share
// claimRestoreOperation and therefore shared the #3600 window.
func restoreWaitCases() []restoreWaitCase {
	return []restoreWaitCase{
		{
			name: "archived restore",
			prepare: func(t *testing.T, manager *Manager, repoID, repoPath string) *session.Instance {
				inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
				inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
				_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
				require.NoError(t, err)
				return inst
			},
			restore: func(manager *Manager, repoID string) error {
				_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
				return err
			},
		},
		{
			name: "lost restore",
			prepare: func(t *testing.T, manager *Manager, repoID, repoPath string) *session.Instance {
				backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
				return registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Lost)
			},
			restore: func(manager *Manager, repoID string) error {
				_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "worker", RepoID: repoID})
				return err
			},
		},
	}
}
