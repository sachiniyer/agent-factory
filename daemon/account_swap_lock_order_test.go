package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The documented lock order has two separate hierarchies that must never
// cross: the per-repo start-lock hierarchy (op -> start -> admission -> m.mu)
// and the fence hierarchy (configApplyMu -> accountLimitMu -> m.mu). The
// account-swap resume path holds the fences across commitNewAccountSwapIdentity,
// which used to call persistSettlement — a function that takes the per-repo
// start lock. CreateSession holds that same start lock across its whole body
// and then blocks on accountLimitMu when a new session's startup hits a
// usage-limit wall. The two paths nest the locks in opposite orders
// (CreateSession: start -> accountLimitMu; swap commit: accountLimitMu ->
// start), forming a permanent ABBA deadlock between plain non-reentrant
// sync.Mutexes (#6197 inversion class, fence <-> start-lock variant).
//
// The fix releases the fences before the settlement persist, so the persist
// takes the start lock without spanning the fence hierarchy. These tests pin
// that ordering.

// fenceStartLockProbe watches every durable row write for the one thing the
// order forbids: the per-repo start lock taken while a global fence
// (accountLimitMu or configApplyMu) is held.
//
// A write that goes through persistSettlement reaches persistInstanceData with
// the start lock held, and the fences are held by the swap commit that issued
// it, so both read as busy at the hook. Nothing else in these tests takes the
// fences, so a busy fence is the swap commit's.
type fenceStartLockProbe struct {
	mu         sync.Mutex
	underFence int
	violations []string
}

func installFenceStartLockProbe(t *testing.T, manager *Manager, repoID string) *fenceStartLockProbe {
	t.Helper()
	p := &fenceStartLockProbe{}
	start := manager.startLockForRepo(repoID)
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(id string, data session.InstanceData) error {
		if id != repoID {
			return nil
		}
		startHeld := lockBusy(start)
		fenceHeld := lockBusy(&manager.accountLimitMu)
		configFenceHeld := lockBusy(&manager.configApplyMu)
		p.mu.Lock()
		defer p.mu.Unlock()
		if fenceHeld || configFenceHeld {
			p.underFence++
		}
		if startHeld && (fenceHeld || configFenceHeld) {
			p.violations = append(p.violations, data.Title)
		}
		return nil
	}
	return p
}

func (p *fenceStartLockProbe) require(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Empty(t, p.violations,
		"a row write took the per-repo start lock while a global fence (accountLimitMu or configApplyMu) was held — "+
			"the fence <-> start-lock ABBA that deadlocks against CreateSession (#6197 inversion class)")
}

// TestAccountSwapResume_SettlementPersistDoesNotSpanFencesAndStartLock is the
// probe oracle: an automatic account-swap resume must never reach a durable
// row write with both a global fence and the per-repo start lock held. Before
// the fix, commitNewAccountSwapIdentity called persistSettlement (which takes
// the start lock) while the caller held configApplyMu and accountLimitMu, so
// every settlement write on the swap's success path spanned both hierarchies.
func TestAccountSwapResume_SettlementPersistDoesNotSpanFencesAndStartLock(t *testing.T) {
	manager, repoID, _, _ := newAutoResumeManager(t, "", true, "finish the migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	probe := installFenceStartLockProbe(t, manager, repoID)
	manager.ResumeLimitedSessions()
	probe.require(t)
}

// TestAccountSwapResume_ReleasesFencesBeforeSettlementPersist is the overlap
// oracle: a create that already holds the per-repo start lock (and is about to
// block on accountLimitMu) against an account-swap commit for another session
// in the same repository.
//
// The swap commit must release the fences before its settlement persist blocks
// on the start lock — otherwise the create's start-lock wait and the swap
// commit's fence-held start-lock wait are each other's cause. Every wait is
// under a deadline so a regression fails with a message instead of hanging the
// package.
//
// The test uses testHookAccountSwapBeforeAdmissionReturns (which fires inside
// the fenced section, after the fences are acquired and admission succeeds) to
// coordinate: it blocks the swap commit goroutine inside the fences until the
// test has acquired the start lock, then lets it proceed. With the fix the
// swap commit releases the fences before calling persistSettlement (which
// needs the start lock), so accountLimitMu becomes obtainable while the start
// lock is still held. Without the fix the persist runs inside the fences and
// blocks on the start lock, so accountLimitMu stays unobtainable — the ABBA.
func TestAccountSwapResume_ReleasesFencesBeforeSettlementPersist(t *testing.T) {
	manager, repoID, _, _ := newAutoResumeManager(t, "", true, "finish the migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")

	startLock := manager.startLockForRepo(repoID)

	// The hook fires inside the fenced section (fences held). Block the swap
	// commit there until the test has acquired the start lock, so the persist
	// is guaranteed to contend with the start lock the test holds.
	fencesHeld := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	previousHook := testHookAccountSwapBeforeAdmissionReturns
	testHookAccountSwapBeforeAdmissionReturns = func() {
		close(fencesHeld)
		<-proceed // block until the test has acquired the start lock
	}
	t.Cleanup(func() {
		testHookAccountSwapBeforeAdmissionReturns = previousHook
		closeProceed()
	})

	done := make(chan struct{})
	go func() {
		manager.ResumeLimitedSessions()
		close(done)
	}()

	// Wait for the swap commit to enter the fenced admission (fences held).
	select {
	case <-fencesHeld:
	case <-time.After(10 * time.Second):
		t.Fatal("the swap commit did not enter the fenced admission within 10s")
	}

	// Acquire the start lock, simulating a CreateSession that holds it and is
	// about to block on accountLimitMu.
	startLock.Lock()
	startHeld := true
	defer func() {
		if startHeld {
			startLock.Unlock()
		}
	}()

	// Let the swap commit proceed past the hook. It does teardown, identity
	// selection, then (with the fix) releases the fences and calls the settle
	// closure whose persistSettlement blocks on the start lock the test holds.
	closeProceed()

	// With the fix, the fences are released before persistSettlement blocks on
	// the start lock, so accountLimitMu must become obtainable while the start
	// lock is still held. Without the fix, persistSettlement runs inside the
	// fences and blocks on the start lock — accountLimitMu stays held and this
	// wait times out, proving the ABBA.
	acquired := make(chan struct{})
	go func() {
		manager.accountLimitMu.Lock()
		manager.accountLimitMu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
		// Good: the fences were released before the persist blocked on the
		// start lock.
	case <-time.After(10 * time.Second):
		t.Fatal("accountLimitMu was not obtainable while the start lock was held: the swap commit holds the fences across the settlement persist, which deadlocks against a create (#6197 fence <-> start-lock ABBA)")
	}

	// Release the start lock so the persist can complete.
	startLock.Unlock()
	startHeld = false
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the swap commit did not complete within 10s of the start lock being released")
	}
}
