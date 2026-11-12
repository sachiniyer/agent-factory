package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The terminal marker is authoritative only after both its atomic write and
// its parent-directory barrier complete. The whole operation runs inside the
// bounded flight; a timed-out writer may finish later, but its caller reports
// the journal as inconclusive in the meantime.
var hookProgressMarkFinished = writeHookProgressFinished

// HookProgressMarkFinishedForTest returns the current terminal-marker writer.
// SetHookProgressMarkFinishedForTest installs a replacement and returns a
// restore function. Cross-package tests (for example the daemon-level
// abandonment/rebuild E2E) use these to inject a transient marker-storage
// failure so the bounded abandonment retry is scheduled, mirroring the
// production trigger the bug describes.
func HookProgressMarkFinishedForTest() func(string, []byte, os.FileMode) error {
	return hookProgressMarkFinished
}

// SetHookProgressMarkFinishedForTest installs a terminal-marker writer and
// returns a restore function. Production never calls this; it is the
// cross-package equivalent of the in-package `hookProgressMarkFinished` seam.
func SetHookProgressMarkFinishedForTest(fn func(string, []byte, os.FileMode) error) func() {
	previous := hookProgressMarkFinished
	hookProgressMarkFinished = fn
	return func() { hookProgressMarkFinished = previous }
}

type hookProgressFinishedFlight struct {
	done chan struct{}
	err  error
}

var hookProgressFinishedFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressFinishedFlight
}{byPath: make(map[string]*hookProgressFinishedFlight)}

func boundedMarkHookProgressFinished(path string) error {
	hookProgressFinishedFlights.Lock()
	flight := hookProgressFinishedFlights.byPath[path]
	if flight == nil {
		flight = &hookProgressFinishedFlight{done: make(chan struct{})}
		hookProgressFinishedFlights.byPath[path] = flight
		writeFinished := hookProgressMarkFinished
		go func() {
			flight.err = writeFinished(path, nil, 0600)
			hookProgressFinishedFlights.Lock()
			if hookProgressFinishedFlights.byPath[path] == flight {
				delete(hookProgressFinishedFlights.byPath, path)
			}
			close(flight.done)
			hookProgressFinishedFlights.Unlock()
		}()
	}
	hookProgressFinishedFlights.Unlock()

	timer := time.NewTimer(relocationIdentityTimeout.Load())
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		return fmt.Errorf("timed out after %s while writing hook terminal marker %s: %w", relocationIdentityTimeout.Load(), path, context.DeadlineExceeded)
	}
}

func writeHookProgressFinished(path string, data []byte, mode os.FileMode) error {
	if err := config.AtomicWriteFile(path, data, mode); err != nil {
		return err
	}
	return syncHookProgressDirectory(filepath.Dir(path))
}
