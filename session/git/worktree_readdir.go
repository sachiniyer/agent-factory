package git

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// archiveReadDir is guarded by boundedReadDirFlights and captured before each
// worker starts, so test restoration cannot race an in-flight syscall.
var archiveReadDir = os.ReadDir

// SetArchiveReadDirForTest replaces enumeration of one archive parent.
// The caller must release blocked probes when the test finishes.
func SetArchiveReadDirForTest(path string, read func(string) ([]os.DirEntry, error)) func() {
	boundedReadDirFlights.Lock()
	previous := archiveReadDir
	archiveReadDir = func(observed string) ([]os.DirEntry, error) {
		if observed == path {
			return read(observed)
		}
		return previous(observed)
	}
	boundedReadDirFlights.Unlock()
	return func() {
		boundedReadDirFlights.Lock()
		archiveReadDir = previous
		boundedReadDirFlights.Unlock()
	}
}

// BoundedReadDir follows BoundedLstat's identity-probe deadline and per-path
// flight. A stalled enumeration refuses admission without pinning the session
// operation lock; retries join the worker or observe its timed-out latch until
// the filesystem returns, rather than accumulating blocked workers.
func BoundedReadDir(path string) ([]os.DirEntry, error) {
	boundedReadDirFlights.Lock()
	if active := boundedReadDirFlights.byPath[path]; active != nil {
		if active.timedOut {
			boundedReadDirFlights.Unlock()
			return nil, fmt.Errorf(
				"directory scan for %s is still running after an earlier deadline: %w",
				path, context.DeadlineExceeded,
			)
		}
		boundedReadDirFlights.Unlock()
		return waitForBoundedReadDir(path, active)
	}
	flight := &boundedReadDirFlight{done: make(chan struct{})}
	boundedReadDirFlights.byPath[path] = flight
	read := archiveReadDir
	boundedReadDirFlights.Unlock()
	go func() {
		flight.entries, flight.err = read(path)
		boundedReadDirFlights.Lock()
		if boundedReadDirFlights.byPath[path] == flight {
			delete(boundedReadDirFlights.byPath, path)
		}
		close(flight.done)
		boundedReadDirFlights.Unlock()
	}()
	return waitForBoundedReadDir(path, flight)
}

type boundedReadDirFlight struct {
	done     chan struct{}
	entries  []os.DirEntry
	err      error
	timedOut bool
}

var boundedReadDirFlights = struct {
	sync.Mutex
	byPath map[string]*boundedReadDirFlight
}{byPath: map[string]*boundedReadDirFlight{}}

func waitForBoundedReadDir(path string, flight *boundedReadDirFlight) ([]os.DirEntry, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.entries, flight.err
	case <-timer.C:
		boundedReadDirFlights.Lock()
		if boundedReadDirFlights.byPath[path] == flight {
			flight.timedOut = true
			boundedReadDirFlights.Unlock()
			return nil, fmt.Errorf(
				"timed out after %s while listing directory %s: %w",
				relocationIdentityTimeout, path, context.DeadlineExceeded,
			)
		}
		boundedReadDirFlights.Unlock()
		<-flight.done
		return flight.entries, flight.err
	}
}
