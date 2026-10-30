package daemon

import (
	"errors"
	"sync"
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
func TestPollSnapshotCannotRegressAnAdmissionWrite(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "poll-vs-restore", session.NewFakeBackend(), true, session.Running)

	// The poll's checkpoint writes the row; seed a recovery record so there is
	// something for the "restore" to clear.
	require.True(t, inst.SetLostRestoreFailure(3, errors.New("agent exited at startup")))
	manager.persistInstance(repoID, inst)
	require.NotNil(t, recordFor(t, repoID, "poll-vs-restore").LostRestoreFailure)

	var (
		once        sync.Once
		restoreDone = make(chan struct{})
	)
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id != repoID || data.Title != "poll-vs-restore" {
			return nil
		}
		once.Do(func() {
			// This is the POLL's write: its snapshot is already taken and its fence
			// check passed. The restore now clears the record and persists it from
			// under admission.
			inst.ClearLostRestoreFailure()
			go func() {
				defer close(restoreDone)
				if err := manager.persistUnderAdmission(repoID, inst); err != nil {
					t.Errorf("persistUnderAdmission: %v", err)
				}
			}()
			// Unfixed, the restore's write completes right here, and the poll's
			// older snapshot then overwrites it. Fixed, it queues behind the poll.
			select {
			case <-restoreDone:
			case <-time.After(300 * time.Millisecond):
			}
		})
		return nil
	}

	// A checkpoint-bearing poll persist: the cleared failure is observed alive.
	manager.persistPollChangeWithIdleEvidence(repoID, inst, inst.GetLiveness(), time.Time{}, true, true)

	select {
	case <-restoreDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the restore's admission write never completed")
	}
	require.Nil(t, recordFor(t, repoID, "poll-vs-restore").LostRestoreFailure,
		"the poll's older whole-row snapshot overwrote the restore's cleared relocation record (#6210)")
}
