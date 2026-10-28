package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

var hookProgressOpenLockFile = os.OpenFile

type hookProgressLockOpenFlight struct {
	done     chan struct{}
	file     *os.File
	err      error
	timedOut bool
}

var hookProgressLockOpenFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressLockOpenFlight
}{byPath: make(map[string]*hookProgressLockOpenFlight)}

// The flock deadline is useful only after the lock file exists. Bound that
// open separately so a stalled mount cannot prevent the acquisition deadline
// from being armed. A timed-out open is allowed to drain, but its descriptor is
// closed only after the flight mutex has been released.
func boundedOpenHookProgressLock(path string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		hookProgressLockOpenFlights.Lock()
		if active := hookProgressLockOpenFlights.byPath[path]; active != nil {
			hookProgressLockOpenFlights.Unlock()
			if err := waitForHookProgressLockOpen(path, active, time.Until(deadline), false); err != nil {
				return nil, err
			}
			continue
		}
		flight := &hookProgressLockOpenFlight{done: make(chan struct{})}
		hookProgressLockOpenFlights.byPath[path] = flight
		openFile := hookProgressOpenLockFile
		hookProgressLockOpenFlights.Unlock()
		go func() {
			file, err := openFile(path, os.O_CREATE|os.O_RDWR, 0644)
			var abandoned *os.File
			hookProgressLockOpenFlights.Lock()
			if flight.timedOut {
				abandoned = file
			} else {
				flight.file, flight.err = file, err
			}
			if hookProgressLockOpenFlights.byPath[path] == flight {
				delete(hookProgressLockOpenFlights.byPath, path)
			}
			close(flight.done)
			hookProgressLockOpenFlights.Unlock()
			if abandoned != nil {
				closeHookProgressFile(abandoned)
			}
		}()
		if err := waitForHookProgressLockOpen(path, flight, time.Until(deadline), true); err != nil {
			return nil, err
		}
		return flight.file, flight.err
	}
}

func waitForHookProgressLockOpen(path string, flight *hookProgressLockOpenFlight, timeout time.Duration, own bool) error {
	if timeout <= 0 {
		if own {
			hookProgressLockOpenFlights.Lock()
			if hookProgressLockOpenFlights.byPath[path] == flight {
				flight.timedOut = true
			}
			hookProgressLockOpenFlights.Unlock()
		}
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return nil
	case <-timer.C:
		if own {
			hookProgressLockOpenFlights.Lock()
			if hookProgressLockOpenFlights.byPath[path] == flight {
				flight.timedOut = true
				hookProgressLockOpenFlights.Unlock()
				return context.DeadlineExceeded
			}
			hookProgressLockOpenFlights.Unlock()
			<-flight.done
			return nil
		}
		return context.DeadlineExceeded
	}
}

func withBoundedHookProgressFileLock(path string, timeout time.Duration, fn func() error) error {
	lockPath := path + ".lock"
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		file, err := boundedOpenHookProgressLock(lockPath, remaining)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w on %s after %s (lock file open did not complete)", config.ErrLockTimeout, lockPath, timeout)
			}
			return fmt.Errorf("failed to open hook progress lock file %s: %w", lockPath, err)
		}
		for {
			err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				current, validateErr := hookProgressLockFileIsCurrent(file, lockPath)
				if validateErr != nil {
					unlockAndCloseHookProgressFile(file)
					return fmt.Errorf("failed to validate hook progress lock file %s: %w", lockPath, validateErr)
				}
				if current {
					callbackErr := fn()
					unlockAndCloseHookProgressFile(file)
					return callbackErr
				}
				unlockAndCloseHookProgressFile(file)
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				closeHookProgressFile(file)
				return fmt.Errorf("failed to acquire hook progress lock on %s: %w", lockPath, err)
			}
			if !time.Now().Before(deadline) {
				closeHookProgressFile(file)
				return fmt.Errorf("%w on %s after %s (another agent-factory process is holding it)", config.ErrLockTimeout, lockPath, timeout)
			}
			wait := 20 * time.Millisecond
			if remaining := time.Until(deadline); remaining < wait {
				wait = remaining
			}
			time.Sleep(wait)
		}
	}
}

// tryWithBoundedHookProgressFileLock preserves teardown's do-not-queue
// contract while bounding the file open and identity probes that precede the
// nonblocking flock. A late open owns no callback: timeout returns before any
// journal mutation can begin.
func tryWithBoundedHookProgressFileLock(path string, timeout time.Duration, fn func() error) (bool, error) {
	lockPath := path + ".lock"
	file, err := boundedOpenHookProgressLock(lockPath, timeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("%w on %s after %s (lock file open did not complete)", config.ErrLockTimeout, lockPath, timeout)
		}
		return false, fmt.Errorf("failed to open hook progress lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeHookProgressFile(file)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire hook progress lock on %s: %w", lockPath, err)
	}
	current, err := hookProgressLockFileIsCurrent(file, lockPath)
	if err != nil {
		unlockAndCloseHookProgressFile(file)
		return false, fmt.Errorf("failed to validate hook progress lock file %s: %w", lockPath, err)
	}
	if !current {
		unlockAndCloseHookProgressFile(file)
		return false, fmt.Errorf("hook progress lock file %s was replaced while acquiring it", lockPath)
	}
	err = fn()
	unlockAndCloseHookProgressFile(file)
	return true, err
}

func hookProgressLockFileIsCurrent(file *os.File, path string) (bool, error) {
	opened, err := boundedHookProgressLeaseStat(file, path)
	if err != nil {
		return false, err
	}
	linked, err := BoundedLstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return opened.Mode().IsRegular() && os.SameFile(opened, linked), nil
}
