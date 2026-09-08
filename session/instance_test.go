package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

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

// TestObserveLivenessNeverClobbersInFlightOp is the #1195 structural replacement
// for the old #844 fence (SetStatusIfNotDeleting): the daemon poll writes only
// the liveness axis (via the ObserveLiveness chokepoint edge), so a concurrent
// kill/archive op — living on the separate op axis — can never be clobbered. The
// composed status keeps reflecting the op, which is what SetStatusIfNotDeleting
// used to reconstruct by hand.
func TestObserveLivenessNeverClobbersInFlightOp(t *testing.T) {
	i := &Instance{liveness: LiveReady}

	// A kill is optimistically in flight (op axis), underlying liveness Running.
	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	require.NoError(t, i.Transition(BeginKill()))
	require.Equal(t, Deleting, i.GetStatus())

	// The poll writes liveness (Running/Ready/Lost) — the kill op survives every
	// write, so the composed status stays Deleting.
	require.NoError(t, i.Transition(ObserveLiveness(LiveReady)))
	require.Equal(t, OpKilling, i.GetInFlightOp(), "ObserveLiveness must not touch the op axis")
	require.Equal(t, Deleting, i.GetStatus(), "a poll write must never clobber a kill marker")
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))
	require.Equal(t, Deleting, i.GetStatus())

	// The kill handler clears the op; the composed status then reflects liveness.
	require.NoError(t, i.Transition(RevertKill()))
	require.Equal(t, Lost, i.GetStatus())
}
