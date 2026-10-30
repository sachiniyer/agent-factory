package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A daemon that accepts the connection and never answers must not park the event
// loop (#6045). The fetcher here behaves like the real transport: it returns only
// when the context it was handed is done. Without a deadline on that context the
// call below never returns; with one, the picker reports the snapshot as degraded
// — the state its destructive D path already gates on — and the Projects section,
// which builds through the same function, still gets its registry rows.
func TestProjectListSnapshotFetchIsBoundedOnTheEventLoop(t *testing.T) {
	h := newTestHome(t)
	oldTimeout := projectSnapshotFetchTimeout
	projectSnapshotFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { projectSnapshotFetchTimeout = oldTimeout })
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(ctx context.Context) ([]session.InstanceData, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))

	type result struct{ degraded bool }
	done := make(chan result, 1)
	go func() {
		_, _, snapshotDegraded := h.buildProjectListWithSnapshotStatus()
		done <- result{snapshotDegraded}
	}()
	select {
	case r := <-done:
		require.True(t, r.degraded, "a timed-out snapshot read is the degraded state, not an empty success")
	case <-time.After(5 * time.Second):
		t.Fatal("buildProjectListWithSnapshotStatus did not return: the event loop would be parked on an unanswered daemon")
	}

	// The sidebar path shares the builder, so it is bounded by the same deadline.
	finished := make(chan struct{})
	go func() { h.refreshSidebarProjects(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshSidebarProjects did not return against an unanswered daemon")
	}
}
