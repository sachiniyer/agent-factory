//go:build darwin

package proctree

import (
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
