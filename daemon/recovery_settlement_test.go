package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// failRecoverySettlement fails the FIRST write for title that records a live row
// — the settlement a recovery owes once its respawn succeeded. Every write before
// it (the Lost row going in) and every write after it (the retry) is real, so a
// test can tell "the settlement never became durable" apart from "nothing was
// written at all". Returns a func reporting whether the injection actually fired.
func failRecoverySettlement(t *testing.T, title string, failure error) func() bool {
	t.Helper()
	var mu sync.Mutex
	fired := false
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		mu.Lock()
		defer mu.Unlock()
		if fired || data.Title != title || data.Liveness != session.LiveRunning {
			return nil
		}
		fired = true
		return failure
	}
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fired
	}
}

// A recovery's settlement write must survive a failure (#2883).
//
// Recover can rebuild a vanished worktree, which RECREATES the branch and flips
// branchCreatedByUs — the flag that authorizes deleting that branch later. The
// write recording it was best-effort, and the code already says why nothing else
// covers it: "Recover's ConfirmLive already left the instance LiveRunning, so the
// next tick compares LiveRunning against LiveRunning and persistPollChange skips
// the write". So a lost write reverted the flag on the next start, the next
// recovery skipped the rebuild because the worktree now existed, and the branch
// was orphaned with nothing left to re-flip it.
//
// The automatic loop has no caller to report to, which is exactly why the retry —
// not an error return — is the mitigation here.
func TestRestoreLostSessions_SettlementPersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

	diskFull := errors.New("no space left on device")
	injected := failRecoverySettlement(t, "stranded", diskFull)

	manager.RestoreLostSessions()

	if !injected() {
		t.Fatal("no recovery settlement write was ever attempted, so this test exercised nothing")
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running: the recovery itself must still have succeeded", got)
	}
	// The premise: the write did not land, so disk still describes the Lost row.
	if rec := recordFor(t, repoID, "stranded"); rec == nil || rec.Liveness == session.LiveRunning {
		t.Fatalf("record after the failed settlement = %+v, want the stale pre-recovery row", rec)
	}

	// The daemon's next poll owes this write and must finish it. Without the retry
	// nothing ever would: the row is already LiveRunning in memory, so the status
	// poll compares LiveRunning against LiveRunning and writes nothing, and the
	// whole-state shutdown checkpoint is what an unclean exit skips.
	manager.FlushOwedSettlements()

	rec := recordFor(t, repoID, "stranded")
	if rec == nil || rec.Liveness != session.LiveRunning {
		t.Fatalf("record after the poll = %+v, want the recovered state durable: a lost recovery "+
			"settlement reverts what the rebuild established, and an af-created branch is then "+
			"orphaned with nothing left to re-flip its provenance", rec)
	}
}

// The same obligation on the manual restore RPC, which shares the write and the
// reasoning with the automatic loop above and differs only in its trigger.
func TestRestoreLostOrDeadSession_SettlementPersistFailureIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "manual", backend, true, session.Lost)

	diskFull := errors.New("no space left on device")
	injected := failRecoverySettlement(t, "manual", diskFull)

	// The caller is told, unlike the automatic loop which has nobody to tell: the
	// retry set only helps if the daemon lives long enough to drain it, and the
	// user is the one who can free the disk.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual", RepoID: repoID})
	if err == nil || !errors.Is(err, diskFull) {
		t.Fatalf("RestoreSession error = %v, want the settlement write failure surfaced (%v)", err, diskFull)
	}
	if !strings.Contains(err.Error(), "agent is running") {
		t.Fatalf("error must say the session IS restored, not just that a write failed: %v", err)
	}
	if !injected() {
		t.Fatal("no recovery settlement write was ever attempted, so this test exercised nothing")
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running", got)
	}

	manager.FlushOwedSettlements()

	rec := recordFor(t, repoID, "manual")
	if rec == nil || rec.Liveness != session.LiveRunning {
		t.Fatalf("record after the poll = %+v, want the recovered state durable", rec)
	}
}
