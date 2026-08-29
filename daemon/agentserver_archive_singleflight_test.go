package daemon

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A retry must JOIN a running archive, never start a second one on the same
// worktree (#2997 finding 2).
//
// The situation this reproduces: rpcHandler discards the request context, so a
// client that gives up ends the request and not the server-side git work. The
// daemon's preserveSandboxBeforeReap then refuses to reap and "keeps retrying" —
// its own words — and that retry reaches this handler while the first archive is
// still inside git add/commit/push.
func TestSingleFlightArchive_RetryJoinsTheRunningArchive(t *testing.T) {
	var s singleFlightArchive
	var starts atomic.Int32
	release := make(chan struct{})

	archive := func() (string, error) {
		starts.Add(1)
		<-release // stands in for a push that outlasts the client's deadline
		return "af/worker-archived", nil
	}

	const joiners = 4
	var wg sync.WaitGroup
	branches := make([]string, joiners)
	errs := make([]error, joiners)
	for i := range joiners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			branches[i], errs[i] = s.do(archive)
		}()
	}

	// Let them pile up on the in-flight attempt before it completes — that is the
	// overlap window the finding is about.
	require.Eventually(t, func() bool { return starts.Load() == 1 }, 2*time.Second, time.Millisecond,
		"the first caller must have started the archive")
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 1, starts.Load(),
		"a concurrent retry started a SECOND archive: two git add/commit runs against one worktree")

	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, starts.Load(), "exactly one archive ran for %d concurrent requests", joiners)
	for i := range joiners {
		require.NoError(t, errs[i])
		assert.Equal(t, "af/worker-archived", branches[i],
			"a joiner must receive the branch the running archive produced — recovering the answer the timed-out "+
				"caller never got is the reason this joins rather than refusing")
	}
}

// The failure is shared too: joiners must not silently succeed on an archive that
// failed, and must not each retry it in place.
func TestSingleFlightArchive_SharesTheFailure(t *testing.T) {
	var s singleFlightArchive
	var starts atomic.Int32
	release := make(chan struct{})
	boom := errors.New("push rejected")

	archive := func() (string, error) {
		starts.Add(1)
		<-release
		return "", boom
	}

	var entered atomic.Int32
	var wg sync.WaitGroup
	results := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered.Add(1)
			_, results[i] = s.do(archive)
		}()
	}
	// Both waits, then the settle, or this test races its own joiners (#3528).
	//
	// Waiting for starts==1 proves only that the LEADER is inside archive(). It
	// says nothing about the other two, and `close(release)` immediately after
	// lets the leader finish and clear the slot — so a joiner that had not yet
	// reached s.mu.Lock() finds s.active nil, becomes a second leader, and runs
	// a second archive. That is what the failure reports: starts==2, while every
	// ErrorIs below still passes, because the error sharing was never the part
	// that broke.
	//
	// The sibling test above never had this hole: it sleeps 50ms before
	// releasing, "to let them pile up on the in-flight attempt". Same window
	// here, plus an explicit entered==3 so the wait does not depend on three
	// goroutines being scheduled within it.
	require.Eventually(t, func() bool { return starts.Load() == 1 }, 2*time.Second, time.Millisecond,
		"the first caller must have started the archive")
	require.Eventually(t, func() bool { return entered.Load() == 3 }, 2*time.Second, time.Millisecond,
		"every caller must have reached do() before the archive is allowed to finish")
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, starts.Load())
	for i := range 3 {
		assert.ErrorIs(t, results[i], boom, "every joiner must see the archive's real outcome")
	}
}

// A caller arriving AFTER one finishes starts a fresh archive: the slot is
// released, not cached. An archive is a snapshot of a moment, and a later caller
// wants the state as it is then rather than an old branch name.
func TestSingleFlightArchive_LaterCallStartsAFreshArchive(t *testing.T) {
	var s singleFlightArchive
	var starts atomic.Int32
	archive := func() (string, error) {
		n := starts.Add(1)
		if n == 1 {
			return "af/first", nil
		}
		return "af/second", nil
	}

	branch, err := s.do(archive)
	require.NoError(t, err)
	assert.Equal(t, "af/first", branch)

	branch, err = s.do(archive)
	require.NoError(t, err)
	assert.Equal(t, "af/second", branch, "a sequential call must run again, not replay a cached result")
	assert.EqualValues(t, 2, starts.Load())
}

// A joiner does not outwait its own caller (#2997 finding 2).
//
// runGitCommand has no timeout, so a stalled push can run indefinitely; without a
// bound, every retry would park a handler goroutine on it forever and they would
// accumulate one per retry. The expiry is an ERROR rather than an empty branch,
// because the caller must read it as "we do not know" — which is what makes
// preserveSandboxBeforeReap refuse to reap instead of reaping onto a branch that
// may not exist.
func TestSingleFlightArchive_JoinerDoesNotWaitForever(t *testing.T) {
	prev := archiveJoinTimeout
	archiveJoinTimeout = 40 * time.Millisecond
	t.Cleanup(func() { archiveJoinTimeout = prev })

	var s singleFlightArchive
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	leaderRunning := make(chan struct{})

	go func() {
		_, _ = s.do(func() (string, error) {
			close(leaderRunning)
			<-stall // a push that never finishes
			return "", nil
		})
	}()
	<-leaderRunning

	start := time.Now()
	branch, err := s.do(func() (string, error) {
		t.Error("the joiner started a SECOND archive instead of waiting for the running one")
		return "", nil
	})
	elapsed := time.Since(start)

	require.Error(t, err, "a joiner that gives up must report an error, not an empty branch that reads as success")
	assert.Empty(t, branch)
	assert.Contains(t, err.Error(), "not starting a second one",
		"the message must say why it refused, so an operator is not left guessing")
	assert.Less(t, elapsed, 2*time.Second, "the joiner must give up on its own bound, not block on the stalled push")
}
