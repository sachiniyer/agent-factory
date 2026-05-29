package app

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// TestMetadataTickSnapshotRace is the race-detector regression for issue #682.
//
// The tickUpdateMetadataMessage handler snapshots the sidebar instance list and
// hands it to a background tea.Cmd goroutine (runMetadataTickCmd). Before the
// fix the handler passed the sidebar's own slice (GetInstances), so the
// goroutine iterated the same backing array the event loop mutates via
// RemoveInstanceByTitle/AddInstance — a data race on the slice memory.
//
// This test drives the real handler (h.Update) so the snapshot is taken exactly
// where production takes it, runs the returned cmd on its own goroutine just as
// bubbletea would, and churns the sidebar on the event-loop goroutine in
// between. Removing the front instance forces a full-array slicecopy, which is
// the write that races with the goroutine's read.
//
// All instances are kept in Loading status so runMetadataTick's per-instance
// branch is skipped (the loop body only does mutex-guarded GetStatus/Started
// reads): that isolates the slice race so a clean run proves #682 is fixed
// rather than masking an unrelated instance-internal race.
//
// Run with `go test -race`: this reports a DATA RACE on master and passes once
// the handler copies the list via GetInstancesSnapshot.
func TestMetadataTickSnapshotRace(t *testing.T) {
	h := newTestHome(t)
	for i := 0; i < 24; i++ {
		inst := instanceWithFakeBackend(t, fmt.Sprintf("seed-%d", i))
		inst.SetStatus(session.Loading)
		h.sidebar.AddInstance(inst)
	}

	var wg sync.WaitGroup
	const iters = 60
	for i := 0; i < iters; i++ {
		// The handler snapshots the instance list on the event loop and returns
		// the tick cmd; bubbletea runs that cmd on a fresh goroutine.
		_, cmd := h.Update(tickUpdateMetadataMessage{})
		if cmd == nil {
			t.Fatal("handler must return a re-schedule cmd")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd()
		}()

		// Event-loop-side churn: removing the front instance shifts the whole
		// backing array (slicecopy) — the write that races with the goroutine's
		// iteration when the list is shared rather than copied. Re-adding the
		// same pointer keeps the population stable across iterations.
		insts := h.sidebar.GetInstances()
		front := insts[0]
		h.sidebar.RemoveInstanceByTitle(front.Title)
		h.sidebar.AddInstance(front)
	}
	wg.Wait()
}
