package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Test seam for directory-open and fsync stalls. Callers use the bounded
// wrapper below; the worker captures this function before it starts.
var hookProgressSyncDirectory = syncHookProgressDirectory

type hookProgressSyncFlight struct {
	done     chan struct{}
	err      error
	timedOut bool
}

var hookProgressSyncFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressSyncFlight
}{byPath: make(map[string]*hookProgressSyncFlight)}

// boundedSyncHookProgressDirectory gives the durability barrier the same
// deadline and per-path timed-out latch as hook metadata probes. A timeout is
// inconclusive: callers must not report publication or terminal progress as
// durable. The worker may finish the fsync later, but performs no namespace
// mutation after the caller releases .progress.
func boundedSyncHookProgressDirectory(path string) error {
	hookProgressSyncFlights.Lock()
	if active := hookProgressSyncFlights.byPath[path]; active != nil {
		if active.timedOut {
			hookProgressSyncFlights.Unlock()
			return fmt.Errorf("hook directory sync for %s is still running after an earlier deadline: %w", path, context.DeadlineExceeded)
		}
		hookProgressSyncFlights.Unlock()
		if err := waitForHookProgressSync(path, active); errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// A concurrent caller can mutate the directory after active's fsync has
		// begun. Wait for that flight to drain, then start this caller's own
		// barrier; joining the earlier result would not order the later mutation.
		return boundedSyncHookProgressDirectory(path)
	}
	flight := &hookProgressSyncFlight{done: make(chan struct{})}
	hookProgressSyncFlights.byPath[path] = flight
	syncDirectory := hookProgressSyncDirectory
	hookProgressSyncFlights.Unlock()
	go func() {
		flight.err = syncDirectory(path)
		hookProgressSyncFlights.Lock()
		if hookProgressSyncFlights.byPath[path] == flight {
			delete(hookProgressSyncFlights.byPath, path)
		}
		close(flight.done)
		hookProgressSyncFlights.Unlock()
	}()
	return waitForHookProgressSync(path, flight)
}

func waitForHookProgressSync(path string, flight *hookProgressSyncFlight) error {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.err
	case <-timer.C:
		hookProgressSyncFlights.Lock()
		if hookProgressSyncFlights.byPath[path] == flight {
			flight.timedOut = true
			hookProgressSyncFlights.Unlock()
			return fmt.Errorf("timed out after %s while syncing hook directory %s: %w", relocationIdentityTimeout, path, context.DeadlineExceeded)
		}
		hookProgressSyncFlights.Unlock()
		<-flight.done
		return flight.err
	}
}

func syncHookProgressDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}
