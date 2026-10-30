package daemon

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// Writers to one instances.json row, and how each is ordered against the others
// (#6210):
//
//   - polls (persistPollChangeWithIdleEvidence, persistWorktreeMissing, the
//     settlement retry): gate on OpNone, take the start lock, and snapshot the row;
//   - client ops and restore/recover: hold the session op lock and raise an
//     InFlightOp fence, which makes the polls cede;
//   - the admission writers (persistUnderAdmission, persistSettlementUnderAdmission):
//     run under admission and cannot take the start lock (#6197), so the start lock
//     does not order them against a poll that already passed its fence check.
//
// The repository row-write lock (rowWriteLockForRepo) closes that last gap: a poll
// holds it from its final fence check through its write, an admission writer holds
// it across its snapshot and write.

// TestPollSnapshotCannotRegressAnAdmissionWrite interleaves a poll that has
// passed its fence check, a restore's targeted clear of the row, and the poll's
// persist. The restore's write must land AFTER the poll's, so the row ends up
// holding what the restore cleared.
//
// Nothing in the harness orders the two writes. Only the poll's write is gated,
// and it is identified by its payload (its snapshot still carries the recovery
// record the restore clears), so the restore's write passes through the same
// hook freely. The restore therefore ends up behind the poll only if production
// code puts it there (rowWriteLockForRepo): with the lock a no-op its write is
// observed while the poll is still paused, and the poll then lands the older
// snapshot over it. Both outcomes are reached through signals, never a delay:
// the test waits until the restore has either written or is parked on a mutex.
func TestPollSnapshotCannotRegressAnAdmissionWrite(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "poll-vs-restore", session.NewFakeBackend(), true, session.Running)

	require.True(t, inst.SetLostRestoreFailure(3, errors.New("agent exited at startup")))
	manager.persistInstance(repoID, inst)
	require.NotNil(t, recordFor(t, repoID, "poll-vs-restore").LostRestoreFailure)

	var (
		pollClaimed  atomic.Bool
		pollAtHook   = make(chan struct{})
		releasePoll  = make(chan struct{})
		restoreWrote = make(chan struct{})
		restoreOnce  sync.Once
	)
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id != repoID || data.Title != "poll-vs-restore" {
			return nil
		}
		if data.LostRestoreFailure != nil && pollClaimed.CompareAndSwap(false, true) {
			// The POLL's write: its snapshot is taken and its fence check passed.
			// Hold it here until the test lets it go.
			close(pollAtHook)
			<-releasePoll
			return nil
		}
		// The restore's write (its payload has the record cleared): never held.
		restoreOnce.Do(func() { close(restoreWrote) })
		return nil
	}

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		manager.persistPollChangeWithIdleEvidence(repoID, inst, inst.GetLiveness(), time.Time{}, true, true)
	}()
	waitSignal(t, pollAtHook, "the poll never reached its write")

	// The restore clears the record in memory and persists it from under admission.
	inst.ClearLostRestoreFailure()
	restoreDone := make(chan struct{})
	go func() {
		defer close(restoreDone)
		if err := manager.persistUnderAdmission(repoID, inst); err != nil {
			t.Errorf("persistUnderAdmission: %v", err)
		}
	}()

	// Wait for whichever the code under test produces: the restore's write
	// (nothing held it behind the poll) or the restore parked on a mutex.
	waitUntilTrue(t, "the restore neither wrote nor blocked on a lock", func() bool {
		select {
		case <-restoreWrote:
			return true
		default:
		}
		return goroutineBlockedOnMutex("persistUnderAdmission")
	})
	select {
	case <-restoreWrote:
		t.Errorf("the restore's admission write landed while an already-admitted poll snapshot was still to be written; " +
			"the poll will now overwrite it (#6210)")
	default:
	}

	close(releasePoll)
	waitSignal(t, pollDone, "the poll never finished")
	waitSignal(t, restoreDone, "the restore's admission write never completed")
	require.Nil(t, recordFor(t, repoID, "poll-vs-restore").LostRestoreFailure,
		"the poll's older whole-row snapshot overwrote the restore's cleared relocation record (#6210)")
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatal(what)
	}
}

// waitUntilTrue polls cond until it holds. The deadline only bounds a hang; it never
// decides the outcome, which is what cond reports.
func waitUntilTrue(t *testing.T, what string, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	return true
}

// goroutineBlockedOnMutex reports whether a goroutine whose stack contains
// frame is parked acquiring a sync.Mutex.
func goroutineBlockedOnMutex(frame string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) &&
			(strings.Contains(g, "sync.(*Mutex).lockSlow") || strings.Contains(g, "[sync.Mutex.Lock")) {
			return true
		}
	}
	return false
}
