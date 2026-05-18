package session

import (
	"sync"
	"testing"
	"time"
)

// TestAutoYesDataRace is the regression test for issue #563.
//
// PR #560 moved the metadata tick onto a background goroutine. That goroutine
// calls LocalBackend.TapEnter, which reads Instance.AutoYes under i.mu.RLock.
// Several event-loop call sites (app/app.go, app/sync.go, daemon/daemon.go)
// previously wrote i.AutoYes directly with no mutex, producing a real data
// race that `go test -race` would flag.
//
// The fix introduces Instance.SetAutoYes so every writer goes through the
// instance mutex. This test exercises a tight concurrent write/read loop
// against the same instance; under `go test -race -run TestAutoYesDataRace
// ./session/...` it must complete cleanly with the fix in place, and would
// have tripped the race detector against the pre-fix code that wrote
// `i.AutoYes = ...` directly.
func TestAutoYesDataRace(t *testing.T) {
	inst := &Instance{Title: "race"}
	inst.SetBackend(&LocalBackend{})
	inst.SetStartedForTest(true)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: mirrors the event-loop call sites (app/app.go,
	// app/sync.go, daemon/daemon.go) that flip AutoYes after the
	// instance has been added to the sidebar.
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			inst.SetAutoYes(toggle)
			toggle = !toggle
		}
	}()

	// Reader: mirrors the metadata-tick background goroutine introduced
	// by PR #560, which sweeps instances and calls TapEnter on each one.
	// TapEnter reads i.AutoYes under i.mu.RLock — that read is what the
	// race detector flagged against the pre-fix unsynchronized writes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			inst.TapEnter()
		}
	}()

	// 200ms is plenty for the race detector to observe a violation if
	// any read/write pair is unsynchronized; in practice the failure
	// triggers within microseconds.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
