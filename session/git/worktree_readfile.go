package git

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// archiveReadFile is guarded by boundedReadFileFlights and captured before each
// worker starts, so test restoration cannot race an in-flight syscall.
var archiveReadFile = os.ReadFile

// SetArchiveReadFileForTest replaces read of one archive owner file.
// The caller must release blocked probes when the test finishes.
func SetArchiveReadFileForTest(path string, read func(string) ([]byte, error)) func() {
	boundedReadFileFlights.Lock()
	previous := archiveReadFile
	archiveReadFile = func(observed string) ([]byte, error) {
		if observed == path {
			return read(observed)
		}
		return previous(observed)
	}
	boundedReadFileFlights.Unlock()
	return func() {
		boundedReadFileFlights.Lock()
		archiveReadFile = previous
		boundedReadFileFlights.Unlock()
	}
}

// BoundedReadFile follows BoundedLstat's identity-probe deadline and per-path
// flight. A stalled read refuses admission without pinning the session
// operation lock; retries join the worker or observe its timed-out latch until
// the filesystem returns, rather than accumulating blocked workers.
func BoundedReadFile(path string) ([]byte, error) {
	boundedReadFileFlights.Lock()
	if active := boundedReadFileFlights.byPath[path]; active != nil {
		if active.timedOut {
			boundedReadFileFlights.Unlock()
			return nil, fmt.Errorf(
				"file read for %s is still running after an earlier deadline: %w",
				path, context.DeadlineExceeded,
			)
		}
		boundedReadFileFlights.Unlock()
		return waitForBoundedReadFile(path, active)
	}
	flight := &boundedReadFileFlight{done: make(chan struct{})}
	boundedReadFileFlights.byPath[path] = flight
	read := archiveReadFile
	boundedReadFileFlights.Unlock()
	go func() {
		flight.data, flight.err = read(path)
		boundedReadFileFlights.Lock()
		if boundedReadFileFlights.byPath[path] == flight {
			delete(boundedReadFileFlights.byPath, path)
		}
		close(flight.done)
		boundedReadFileFlights.Unlock()
	}()
	return waitForBoundedReadFile(path, flight)
}

type boundedReadFileFlight struct {
	done     chan struct{}
	data     []byte
	err      error
	timedOut bool
}

var boundedReadFileFlights = struct {
	sync.Mutex
	byPath map[string]*boundedReadFileFlight
}{byPath: map[string]*boundedReadFileFlight{}}

func waitForBoundedReadFile(path string, flight *boundedReadFileFlight) ([]byte, error) {
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.data, flight.err
	case <-timer.C:
		boundedReadFileFlights.Lock()
		if boundedReadFileFlights.byPath[path] == flight {
			flight.timedOut = true
			boundedReadFileFlights.Unlock()
			return nil, fmt.Errorf(
				"timed out after %s while reading file %s: %w",
				relocationIdentityTimeout, path, context.DeadlineExceeded,
			)
		}
		boundedReadFileFlights.Unlock()
		<-flight.done
		return flight.data, flight.err
	}
}
