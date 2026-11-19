//go:build darwin

package proctree

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7469 R2: a vnode read parked on a hung mount must answer "could not see"
// in bounded wall-clock time — undecidable, never foreign or own — and the
// expired walk must never report a partial table as "holds none". The staged
// walk blocks until released: exactly what a getattr on a dead NFS/SMB/FUSE
// mount does to VNODEPATHINFO.
func TestPtyFdIndices_AHungWalkAnswersCouldNotSee(t *testing.T) {
	oldWalk, oldBudget := ptyFdWalk, ptyFdReadBudget
	release := make(chan struct{})
	ptyFdReadBudget = 100 * time.Millisecond
	ptyFdWalk = func(pid int, classify func(string, int32, uint32) (PtyIndex, bool)) ([]PtyFdIndex, error) {
		<-release
		return nil, nil
	}
	t.Cleanup(func() {
		ptyFdWalk, ptyFdReadBudget = oldWalk, oldBudget
		close(release) // free the abandoned workers — buffered sends keep them from leaking
	})

	start := time.Now()
	_, err := ptySlaveIndices(4242)
	require.ErrorIs(t, err, errPtyFdTimeout,
		"a walk that cannot answer inside the budget is 'could not see', never a verdict")
	require.Less(t, time.Since(start), 5*time.Second,
		"the deadline is wall-clock, not the mount's mercy")

	_, err = PtyMasterIndices(4242)
	require.ErrorIs(t, err, errPtyFdTimeout)
}

// #7581 item 3: concurrent walks of the SAME (pid, StartID, side) share one
// flight — a wedged vnode read holds at most one worker no matter how many
// waiters attach, exactly the attestFlight shape exeCheckShared uses for
// its kernel reads. Each waiter still bounds its own wait; a different
// side is different work and never joins the slave flight.
func TestPtyFdIndices_ConcurrentWalksShareOneFlight(t *testing.T) {
	oldWalk, oldBudget := ptyFdWalk, ptyFdReadBudget
	release := make(chan struct{})
	var walks atomic.Int64
	ptyFdReadBudget = 100 * time.Millisecond
	ptyFdWalk = func(pid int, classify func(string, int32, uint32) (PtyIndex, bool)) ([]PtyFdIndex, error) {
		walks.Add(1)
		<-release
		return nil, nil
	}
	t.Cleanup(func() {
		ptyFdWalk, ptyFdReadBudget = oldWalk, oldBudget
		close(release) // free the shared workers — published sends never leak
	})

	const waiters = 5
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ptySlaveIndices(os.Getpid()) // same pid, same side → one flight
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	require.Equal(t, 1, ptyFdFlights.inFlightCount(),
		"the wedged walk holds exactly ONE flight — no per-waiter state")
	for err := range errs {
		require.ErrorIs(t, err, errPtyFdTimeout, "each waiter bounds its own wait")
	}
	require.Equal(t, int64(1), walks.Load(),
		"N waiters on (pid, StartID, side) share a single vnode walk")

	// The master side of the same pid is different work — its own flight.
	_, _ = ptyMasterFdIndices(os.Getpid())
	require.Equal(t, 2, ptyFdFlights.inFlightCount(),
		"a different side never joins the slave flight")
}

// Codex on #7581: the single-fd re-verify shares one worker per
// (pid, StartID, fd) — a carrier whose recorded master descriptor now names
// a file on a dead mount must hold at most one blocked read however many
// attribution passes re-check it. Each caller still bounds its own wait.
func TestPtyMasterFdIndex_ConcurrentReadsShareOneFlight(t *testing.T) {
	oldRead, oldBudget := ptyFdVnodeRead, ptyFdReadBudget
	release := make(chan struct{})
	var reads atomic.Int64
	ptyFdReadBudget = 100 * time.Millisecond
	ptyFdVnodeRead = func(pid, fd int) (PtyIndex, bool) {
		reads.Add(1)
		<-release
		return PtyIndex{Index: 7}, true
	}
	t.Cleanup(func() {
		ptyFdVnodeRead, ptyFdReadBudget = oldRead, oldBudget
		close(release) // free the shared worker — the flight publishes, never leaks
	})

	const waiters = 5
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ptyMasterFdIndex(os.Getpid(), 3) // same instance, same fd → one flight
		}()
	}
	wg.Wait()
	require.Equal(t, 1, ptyVnodeFlights.inFlightCount(),
		"the wedged fd read holds exactly ONE flight — no per-waiter state")
	require.Equal(t, int64(1), reads.Load(),
		"N carriers on the recorded descriptor share a single vnode read")

	// A different descriptor is different work — its own flight.
	_, _ = ptyMasterFdIndex(os.Getpid(), 4)
	require.Equal(t, 2, ptyVnodeFlights.inFlightCount(),
		"another fd never joins the first flight")
}
