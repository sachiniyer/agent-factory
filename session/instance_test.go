package session

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPRInfoAge_NeverFetched_IsVeryLarge — the sentinel behavior that
// fetchPRInfoCmd relies on to always dispatch the first fetch after process
// start (or after an instance is restored from disk — prInfoLastFetched is
// not persisted).
func TestPRInfoAge_NeverFetched_IsVeryLarge(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	age := i.PRInfoAge()
	// The implementation returns math.MaxInt64 - 1 ns in Duration units.
	// Any threshold well beyond a century works as a sanity floor.
	assert.Greater(t, age, 100*365*24*time.Hour,
		"age before first fetch must be effectively infinite so the first fetch always runs")
}

// TestPRInfoAge_AfterSetPRInfo_IsFresh verifies SetPRInfo bumps the age
// clock — otherwise the debounce in fetchPRInfoCmd would never engage.
func TestPRInfoAge_AfterSetPRInfo_IsFresh(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	before := time.Now()
	i.SetPRInfo(&git.PRInfo{Number: 1})
	age := i.PRInfoAge()

	assert.Less(t, age, time.Since(before)+time.Second,
		"age immediately after SetPRInfo must be near zero")
}

// TestMarkPRInfoFetched_BumpsAgeWithoutMutatingInfo — the error path relies
// on this: bump the timestamp (to debounce retries) without touching the
// cached value.
func TestMarkPRInfoFetched_BumpsAgeWithoutMutatingInfo(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	cached := &git.PRInfo{Number: 7, Title: "cached"}
	i.SetPRInfo(cached)

	// Wait enough that a broken implementation (one that doesn't bump the
	// timestamp) would show a stale age. MarkPRInfoFetched should reset it.
	time.Sleep(20 * time.Millisecond)
	before := time.Now()
	i.MarkPRInfoFetched()

	assert.Same(t, cached, i.GetPRInfo(), "MarkPRInfoFetched must not touch the cached info")
	assert.Less(t, i.PRInfoAge(), time.Since(before)+time.Millisecond,
		"MarkPRInfoFetched must reset the fetch timestamp")
}

// TestFetchPRInfoSnapshot_NotStarted returns empty values — the guard used
// by fetchPRInfoCmd to avoid firing during the Loading→Running transition.
func TestFetchPRInfoSnapshot_NotStarted(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	repoPath, branch := i.FetchPRInfoSnapshot()
	assert.Empty(t, repoPath, "snapshot must be empty for not-started instances")
	assert.Empty(t, branch)
}

// TestFetchPRInfoSnapshot_StartedWithoutWorktree returns empty values
// too — there's nothing for `gh` to target.
func TestFetchPRInfoSnapshot_StartedWithoutWorktree(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStartedForTest(true)

	repoPath, branch := i.FetchPRInfoSnapshot()
	assert.Empty(t, repoPath,
		"snapshot must be empty when gitWorktree is nil (remote / mid-setup)")
	assert.Empty(t, branch)
}

// TestGetGitWorktree_RaceWithStart guards the fix for #462. GetGitWorktree
// must read i.started and i.gitWorktree under i.mu — Start() writes both
// under the lock, so an unlocked reader trips the race detector. Run with
// `go test -race ./session/...` to validate.
func TestGetGitWorktree_RaceWithStart(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer goroutine: mirrors the lock-protected writes in
	// LocalBackend.Start (i.gitWorktree, then i.started).
	go func() {
		defer wg.Done()
		for n := 0; n < iterations; n++ {
			i.SetGitWorktreeForTest(&git.GitWorktree{})
			i.SetStartedForTest(true)
		}
	}()

	// Reader goroutine: exercises the path the bug lived on.
	go func() {
		defer wg.Done()
		for n := 0; n < iterations; n++ {
			_, _ = i.GetGitWorktree()
		}
	}()

	wg.Wait()
}

// TestSetLivenessNeverClobbersInFlightOp is the #1195 structural replacement for
// the old #844 fence (SetStatusIfNotDeleting): the daemon poll writes only the
// liveness axis, so a concurrent kill/archive op — living on the separate op axis
// — can never be clobbered. The composed status keeps reflecting the op, which is
// what the SetStatusIfNotDeleting guard used to reconstruct by hand.
func TestSetLivenessNeverClobbersInFlightOp(t *testing.T) {
	i := &Instance{liveness: LiveReady}

	// A kill is optimistically in flight (op axis), underlying liveness Running.
	i.SetLiveness(LiveRunning)
	i.SetInFlightOp(OpKilling)
	require.Equal(t, Deleting, i.GetStatus())

	// The poll writes liveness (Running/Ready/Lost) — the kill op survives every
	// write, so the composed status stays Deleting.
	i.SetLiveness(LiveReady)
	require.Equal(t, OpKilling, i.GetInFlightOp(), "SetLiveness must not touch the op axis")
	require.Equal(t, Deleting, i.GetStatus(), "a poll write must never clobber a kill marker")
	i.SetLiveness(LiveLost)
	require.Equal(t, Deleting, i.GetStatus())

	// The kill handler clears the op; the composed status then reflects liveness.
	i.SetInFlightOp(OpNone)
	require.Equal(t, Lost, i.GetStatus())
}
