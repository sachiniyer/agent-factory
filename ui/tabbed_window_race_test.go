package ui

import (
	"sync"
	"testing"
)

// TestTabbedWindowActiveTabRace is the race-detector regression for issue #684.
//
// Toggle()/ToggleBack() write activeTab from the bubbletea event loop while the
// refreshPanesCmd goroutine reads it via UpdatePreview()/UpdateTerminal(). With
// activeTab as a plain int this is a data race; the fix makes it an
// atomic.Int32.
//
// The background goroutine here mirrors refreshPanesCmd. Passing a nil instance
// keeps UpdateContent a cheap fallback-state write under each pane's own mutex
// (no tmux shell-out), so the only cross-goroutine access under test is
// activeTab itself.
//
// Run with `go test -race`: reports a DATA RACE on master and passes after the
// fix.
func TestTabbedWindowActiveTabRace(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = tw.UpdateContent(nil)
		}
	}()

	for i := 0; i < 100000; i++ {
		tw.Toggle()
	}
	close(stop)
	wg.Wait()
}
