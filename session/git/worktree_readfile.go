package git

import (
	"context"
	"fmt"
	"io"
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
	return boundedReadFile(path, -1)
}

// BoundedReadFileLimit is BoundedReadFile capped at maxBytes so a restored
// symlink destination containing a very large regular `.git` file cannot make
// the diagnostic consume gigabytes or OOM the daemon: the deadline only limits
// how long the caller waits and does not stop the worker or cap its allocation,
// so os.ReadFile would preallocate and read the entire file. A gitfile is a
// single `gitdir: <path>` line, so only the small prefix needed for parsing is
// read, with an explicit maximum size. maxBytes < 0 reads the whole file the
// way BoundedReadFile does.
func BoundedReadFileLimit(path string, maxBytes int64) ([]byte, error) {
	return boundedReadFile(path, maxBytes)
}

func boundedReadFile(path string, maxBytes int64) ([]byte, error) {
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
	flight := &boundedReadFileFlight{done: make(chan struct{}), maxBytes: maxBytes}
	boundedReadFileFlights.byPath[path] = flight
	read := archiveReadFile
	boundedReadFileFlights.Unlock()
	go func() {
		if maxBytes >= 0 {
			flight.data, flight.err = readLimitedFile(path, maxBytes)
		} else {
			flight.data, flight.err = read(path)
		}
		boundedReadFileFlights.Lock()
		if boundedReadFileFlights.byPath[path] == flight {
			delete(boundedReadFileFlights.byPath, path)
		}
		close(flight.done)
		boundedReadFileFlights.Unlock()
	}()
	return waitForBoundedReadFile(path, flight)
}

// readLimitedFile reads at most maxBytes bytes from path so a huge regular
// file (for example a `.git` entry that is not a gitfile) cannot OOM the
// daemon or block the worker on an unbounded allocation. A gitdir path is a
// single short line, so a bounded prefix is all the caller needs to parse it.
func readLimitedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

type boundedReadFileFlight struct {
	done     chan struct{}
	data     []byte
	err      error
	timedOut bool
	maxBytes int64
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
