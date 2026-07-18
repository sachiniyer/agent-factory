package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestDeliverPromptResumeFromLimit_NoInvertedLockDeadlock is the #2006 regression.
//
// DeliverPrompt takes the per-target lock and then the per-session op lock (the op
// lock is acquired inside SendPrompt), i.e. target-before-op. resumeFromLimit used
// to take those same two locks in the OPPOSITE order: the op lock first (TryLock),
// then the target lock (inside resumeFromLimitLocked). A manual send-prompt that
// overlapped a resume of the SAME session therefore formed an ABBA deadlock — and
// the auto-resume scheduler drives resumeFromLimitLocked automatically whenever
// limit_auto_resume is on, so any such daemon could wedge indefinitely.
//
// The interleaving is forced deterministically through two no-op production seams:
// a resumeFromLimit goroutine is pinned holding its FIRST lock, a DeliverPrompt
// goroutine is then let take the target lock, and only then is the resume released
// to go for its SECOND lock. Under the inverted order the two goroutines block on
// each other's second lock forever, the guard timeout fires, and the test FAILS
// (rather than hanging CI). Under the canonical target-before-op order both paths
// acquire in the same order, so both goroutines complete — the asserted outcome.
func TestDeliverPromptResumeFromLimit_NoInvertedLockDeadlock(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// alive=true → a live stall: resumeFromLimit re-delivers the stored prompt with
	// no respawn, so the whole path runs against the fake backend. The session must
	// be LimitReached or resumeFromLimit bails out before it acquires any lock.
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "shared", backend, true, session.Running)
	inst.Prompt = "resume the work"
	inst.SetLimitReached(time.Now())

	resumeGotFirstLock := make(chan struct{}, 1)
	deliverGotTargetLock := make(chan struct{}, 1)
	releaseResume := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResume) }) }

	// The resume goroutine signals once it holds its first lock, then parks here so
	// the test can arrange the crossing before it reaches for the second.
	testHookResumeAfterFirstLock = func() {
		select {
		case resumeGotFirstLock <- struct{}{}:
		default:
		}
		<-releaseResume
	}
	// The delivery goroutine signals once it holds the target lock. Non-blocking so
	// it never stalls the real path, and buffered so a late fire after the test has
	// stopped listening is harmless.
	testHookDeliverAfterTargetLock = func() {
		select {
		case deliverGotTargetLock <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		release() // never strand a parked resume goroutine on a failure path
		testHookResumeAfterFirstLock = func() {}
		testHookDeliverAfterTargetLock = func() {}
	})

	var wg sync.WaitGroup
	wg.Add(2)
	var resumeErr, deliverErr error

	// Goroutine A: resumeFromLimit — grabs its first lock and parks in the seam.
	go func() {
		defer wg.Done()
		resumeErr = manager.resumeFromLimit(ResumeFromLimitRequest{Title: "shared", RepoID: repoID})
	}()

	// Do not start the delivery until the resume goroutine is provably holding its
	// first lock, so the two paths are guaranteed to contend on the second.
	select {
	case <-resumeGotFirstLock:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("resumeFromLimit never reached its first lock; test harness is broken, not the code under test")
	}

	// Goroutine B: DeliverPrompt — a manual send-prompt to the SAME session.
	go func() {
		defer wg.Done()
		_, deliverErr = manager.DeliverPrompt(DeliverPromptRequest{
			Title:    "shared",
			RepoPath: repoPath,
			Prompt:   "manual send",
		})
	}()

	// Under the inverted order the delivery takes the free target lock immediately
	// and heads for the op lock the resume holds; this fires. Under the fixed order
	// it blocks on the target lock the resume holds and never fires, so this wait
	// falls through after the deadline — that is expected and fine.
	select {
	case <-deliverGotTargetLock:
	case <-time.After(3 * time.Second):
	}

	// Release the resume goroutine to reach for its SECOND lock. This is the moment
	// the ABBA closes under the inverted order.
	release()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		if resumeErr != nil {
			t.Fatalf("resumeFromLimit returned %v, want nil", resumeErr)
		}
		if deliverErr != nil {
			t.Fatalf("DeliverPrompt returned %v, want nil", deliverErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock (#2006): DeliverPrompt and resumeFromLimit did not both complete — " +
			"the two paths acquire the per-target and per-session op locks in inverted order")
	}
}
