package app

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestHandleSnapshotRecoveryGrace(t *testing.T) {
	h := newTestHome(t)
	clock := &fakeClock{} // Zero is also a valid first-failure time.
	h.snapshotClock = clock.Now
	h.repoRoot, h.repoID = "/project", "project"
	inst := newSnapshotTestInstance(t, "retained-session")
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	resizeHome(h, 120, 36)
	loaded := ansi.Strip(h.View())
	require.Contains(t, loaded, "retained-session")
	failed := snapshotFetchedMsg{err: errors.New("connection refused")}
	healthy := snapshotFetchedMsg{data: []session.InstanceData{inst.ToInstanceData()}}

	require.False(t, h.handleSnapshot(failed), "one failed poll needs no repaint")
	require.False(t, h.snapshotUnavailable)
	require.Equal(t, loaded, ansi.Strip(h.View()))
	clock.advance(3*time.Second - time.Millisecond)
	require.False(t, h.handleSnapshot(failed), "retain layout throughout the grace window")
	require.Equal(t, loaded, ansi.Strip(h.View()))

	// A successful poll interrupts the streak, even just before the deadline.
	h.handleSnapshot(healthy)
	require.Nil(t, h.snapshotFailureSince)
	clock.advance(time.Second)
	require.False(t, h.handleSnapshot(failed), "a new streak gets its own grace window")
	require.False(t, h.snapshotUnavailable)
	clock.advance(3 * time.Second)
	require.True(t, h.handleSnapshot(failed), "sustained failure needs a recovery repaint")
	require.Contains(t, ansi.Strip(h.View()), "Cannot reach the daemon")
	require.Same(t, inst, h.store.GetInstanceByTitle("retained-session"))
	require.False(t, h.handleSnapshot(failed), "unchanged recovery needs no extra repaint")

	require.True(t, h.handleSnapshot(healthy), "success restores the layout")
	require.False(t, h.snapshotUnavailable)
	require.Nil(t, h.snapshotFailureSince)
	require.Equal(t, loaded, ansi.Strip(h.View()))
	clock.advance(time.Minute)
	require.False(t, h.handleSnapshot(failed), "recovery also resets the failure window")
	require.Equal(t, loaded, ansi.Strip(h.View()))
}

func TestHandleSnapshotColdStartRecoveryDoesNotWaitForGrace(t *testing.T) {
	h := newTestHome(t)
	clock := &fakeClock{}
	h.snapshotClock = clock.Now
	require.Error(t, h.coldStartFromSnapshot())
	require.True(t, h.snapshotUnavailable)
	for _, err := range []error{
		errors.New("connection refused"),
		errors.New("agent-factory daemon is starting (restoring sessions); retry shortly"),
	} {
		require.False(t, h.handleSnapshot(snapshotFetchedMsg{err: err}))
		require.True(t, h.snapshotUnavailable, "failed polls retain immediate cold-start recovery")
	}
	require.True(t, h.handleSnapshot(snapshotFetchedMsg{}))
	require.False(t, h.snapshotUnavailable)
	require.Nil(t, h.snapshotFailureSince)
}
