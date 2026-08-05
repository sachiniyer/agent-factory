package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// failRecoverySettlement fails the first write for title that carries a
// POST-recovery row — the settlement a recovery owes once its respawn succeeded.
// Keying on "no longer the pre-recovery Lost state" rather than on one exact
// liveness keeps it from silently matching nothing if a path settles through a
// different live state; a seam that never fires would make every assertion
// downstream of it vacuous.
//
// It records every write it sees so a failure can say what actually happened
// instead of only that the expectation did not.
func failRecoverySettlement(t *testing.T, title string, failure error) (fired func() bool, seen func() string) {
	t.Helper()
	var mu sync.Mutex
	hit := false
	var writes []string
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		mu.Lock()
		defer mu.Unlock()
		if data.Title != title {
			return nil
		}
		writes = append(writes, fmt.Sprintf("liveness=%v op=%v started-row", data.Liveness, data.InFlightOp))
		if hit || data.Liveness == session.LiveLost {
			return nil
		}
		hit = true
		return failure
	}
	return func() bool {
			mu.Lock()
			defer mu.Unlock()
			return hit
		}, func() string {
			mu.Lock()
			defer mu.Unlock()
			if len(writes) == 0 {
				return "no writes at all for this session"
			}
			return strings.Join(writes, " | ")
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
	injected, seen := failRecoverySettlement(t, "stranded", diskFull)

	manager.RestoreLostSessions()

	if !injected() {
		t.Fatalf("no recovery settlement write was ever failed, so this test exercised nothing; writes seen: %s", seen())
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
	injected, seen := failRecoverySettlement(t, "manual", diskFull)

	// The caller is told, unlike the automatic loop which has nobody to tell: the
	// retry set only helps if the daemon lives long enough to drain it, and the
	// user is the one who can free the disk.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual", RepoID: repoID})
	if err == nil || !errors.Is(err, diskFull) {
		t.Fatalf("RestoreSession error = %v, want the settlement write failure surfaced (%v); writes seen: %s",
			err, diskFull, seen())
	}
	if !strings.Contains(err.Error(), "agent is running") {
		t.Fatalf("error must say the session IS restored, not just that a write failed: %v", err)
	}
	if !injected() {
		t.Fatalf("no recovery settlement write was ever failed, so this test exercised nothing; writes seen: %s", seen())
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
