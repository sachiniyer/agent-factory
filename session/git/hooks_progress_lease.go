package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Test seam for lease path lookup. The worker captures it before starting so a
// timed-out test can drain the flight before restoring the seam without racing.
var hookProgressOpenLeaseFile = os.OpenFile

var hookProgressCloseLeaseFile = func(file *os.File) error { return file.Close() }

type hookProgressLeaseOpenFlight struct {
	done     chan struct{}
	file     *os.File
	err      error
	timedOut bool
}

type hookProgressLeaseStatFlight struct {
	done chan struct{}
	info os.FileInfo
	err  error
}

var hookProgressLeaseOpenFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressLeaseOpenFlight
}{byPath: make(map[string]*hookProgressLeaseOpenFlight)}

var hookProgressLeaseStatFlights = struct {
	sync.Mutex
	byPath map[string]*hookProgressLeaseStatFlight
}{byPath: make(map[string]*hookProgressLeaseStatFlight)}

// Lease lookup is metadata I/O on the same potentially remote filesystem as
// the journal. A timed-out worker performs no journal mutation; if its open
// eventually succeeds, it closes the descriptor immediately. The per-path
// latch prevents repeated pruning passes from stacking blocked OS workers.
func boundedOpenHookProgressLease(path string, flags int, mode os.FileMode) (*os.File, error) {
	openFile := hookProgressOpenLeaseFile
	closeLeaseFile := hookProgressCloseLeaseFile
	closeFile := func(file *os.File) { closeHookProgressFileWith(file, closeLeaseFile) }
	return boundedOpenHookProgressLeaseWith(path, flags, mode, openFile, closeFile, relocationIdentityTimeout)
}

func boundedOpenHookProgressLeaseWith(
	path string,
	flags int,
	mode os.FileMode,
	openFile func(string, int, os.FileMode) (*os.File, error),
	closeFile func(*os.File),
	timeout time.Duration,
) (*os.File, error) {
	hookProgressLeaseOpenFlights.Lock()
	if hookProgressLeaseOpenFlights.byPath[path] != nil {
		hookProgressLeaseOpenFlights.Unlock()
		return nil, fmt.Errorf("hook runner lease open for %s is still running after an earlier deadline: %w", path, context.DeadlineExceeded)
	}
	flight := &hookProgressLeaseOpenFlight{done: make(chan struct{})}
	hookProgressLeaseOpenFlights.byPath[path] = flight
	hookProgressLeaseOpenFlights.Unlock()
	go func() {
		file, err := openFile(path, flags, mode)
		var abandoned *os.File
		hookProgressLeaseOpenFlights.Lock()
		if flight.timedOut {
			abandoned = file
		} else {
			flight.file, flight.err = file, err
		}
		if hookProgressLeaseOpenFlights.byPath[path] == flight {
			delete(hookProgressLeaseOpenFlights.byPath, path)
		}
		close(flight.done)
		hookProgressLeaseOpenFlights.Unlock()
		if abandoned != nil {
			closeFile(abandoned)
		}
	}()
	return waitForHookProgressLeaseOpen(path, flight, timeout)
}

func waitForHookProgressLeaseOpen(path string, flight *hookProgressLeaseOpenFlight, timeout time.Duration) (*os.File, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.file, flight.err
	case <-timer.C:
		hookProgressLeaseOpenFlights.Lock()
		if hookProgressLeaseOpenFlights.byPath[path] == flight {
			flight.timedOut = true
			hookProgressLeaseOpenFlights.Unlock()
			return nil, fmt.Errorf("timed out after %s while opening hook runner lease %s: %w", timeout, path, context.DeadlineExceeded)
		}
		hookProgressLeaseOpenFlights.Unlock()
		<-flight.done
		return flight.file, flight.err
	}
}

// Publication holds .progress while creating this lease. The runner retains
// the descriptor across every launch gap until finish; a daemon exit releases
// it automatically, leaving the existing scope/launcher probes to protect the
// survivor. Once opened, no lease timeout, PID-reuse guess, or heartbeat
// freshness is involved.
func newHookProgressLease(dir string) (*os.File, error) {
	return newHookProgressLeaseWithIO(dir, captureHookProgressPrepareIO())
}

func newHookProgressLeaseWithIO(dir string, io hookProgressPrepareIO) (*os.File, error) {
	file, err := boundedOpenHookProgressLeaseWith(
		filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600,
		io.openLeaseFile, io.closeFile, io.timeout,
	)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		io.closeFile(file)
		return nil, err
	}
	return file, nil
}

// Called under .progress after the batched liveness probes. Keep the lease
// locked through deletion. Never create a missing lease: older journals have
// none, and opening for pruning must not recreate a removed receipt directory.
func retireUnleasedHookProgress(path string, p *hookProgress) (string, bool, error) {
	var retired string
	reclaimed, err := withInactiveHookProgressLease(p.Directory, func() error {
		var retireErr error
		retired, retireErr = retireHookProgressName(path, p)
		return retireErr
	})
	return retired, reclaimed, err
}

func withInactiveHookProgressLease(dir string, remove func() error) (bool, error) {
	leasePath := filepath.Join(dir, "runner.lock")
	file, err := boundedOpenHookProgressLease(leasePath, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return true, remove()
	}
	if err != nil {
		return false, err
	}
	locked := false
	defer func() {
		if locked {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		}
		closeHookProgressFile(file)
	}()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	locked = true
	current, err := boundedHookProgressLeaseStat(file, leasePath)
	if err != nil {
		return false, err
	}
	linked, err := BoundedLstat(leasePath)
	if err != nil {
		return false, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(current, linked) {
		return false, fmt.Errorf("hook runner lease was replaced: %s", leasePath)
	}
	return true, remove()
}

// Close may itself wait on remote storage. It never runs while a progress lock
// or bounded-flight mutex is held; an explicit unlock first releases any lease
// whose semantic lifetime has ended.
func closeHookProgressFile(file *os.File) {
	closeHookProgressFileWith(file, hookProgressCloseLeaseFile)
}

func closeHookProgressFileWith(file *os.File, closeFile func(*os.File) error) {
	if file == nil {
		return
	}
	go func() { _ = closeFile(file) }()
}

func unlockAndCloseHookProgressFile(file *os.File) {
	unlockAndCloseHookProgressFileWith(file, closeHookProgressFile)
}

func unlockAndCloseHookProgressFileWith(file *os.File, closeFile func(*os.File)) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeFile(file)
}

func boundedHookProgressLeaseStat(file *os.File, path string) (os.FileInfo, error) {
	duplicateFD, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	duplicate := os.NewFile(uintptr(duplicateFD), path)
	hookProgressLeaseStatFlights.Lock()
	if hookProgressLeaseStatFlights.byPath[path] != nil {
		hookProgressLeaseStatFlights.Unlock()
		_ = duplicate.Close()
		return nil, fmt.Errorf("hook runner lease inspection for %s is still running after an earlier deadline: %w", path, context.DeadlineExceeded)
	}
	flight := &hookProgressLeaseStatFlight{done: make(chan struct{})}
	hookProgressLeaseStatFlights.byPath[path] = flight
	hookProgressLeaseStatFlights.Unlock()
	go func() {
		flight.info, flight.err = duplicate.Stat()
		flight.err = errors.Join(flight.err, duplicate.Close())
		hookProgressLeaseStatFlights.Lock()
		if hookProgressLeaseStatFlights.byPath[path] == flight {
			delete(hookProgressLeaseStatFlights.byPath, path)
		}
		close(flight.done)
		hookProgressLeaseStatFlights.Unlock()
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.info, flight.err
	case <-timer.C:
		hookProgressLeaseStatFlights.Lock()
		if hookProgressLeaseStatFlights.byPath[path] == flight {
			hookProgressLeaseStatFlights.Unlock()
			return nil, fmt.Errorf("timed out after %s while inspecting hook runner lease %s: %w", relocationIdentityTimeout, path, context.DeadlineExceeded)
		}
		hookProgressLeaseStatFlights.Unlock()
		<-flight.done
		return flight.info, flight.err
	}
}
