package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// TryWithFileLock is WithFileLock for callers that must not wait: it runs fn
// under the same exclusive flock, but only if the lock is free right now.
// It reports whether the lock was acquired; when it was not, fn never runs and
// the caller should treat the work as already in hand elsewhere rather than
// queue behind it. Use this on latency-sensitive paths (a user is waiting)
// where duplicating another process's work is pointless — blocking there turns
// a peer's slow operation into an unexplained hang of your own.
func TryWithFileLock(path string, fn func() error) (acquired bool, err error) {
	lockPath := path + ".lock"

	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return false, fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return false, fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return true, fn()
}

// ErrLockTimeout is returned by WithFileLockTimeout when the flock could not be
// acquired within the caller's budget. Callers match on it (errors.Is) to tell a
// contended lock — retryable, the work never ran — from a real failure of fn.
var ErrLockTimeout = errors.New("timed out waiting for file lock")

// WithFileLockTimeout is WithFileLock bounded by a deadline: it runs fn under the
// same exclusive flock, but gives up with ErrLockTimeout rather than waiting
// forever. fn is never run unless the lock was actually held.
//
// It is the third point on the line TryWithFileLock and WithFileLock already
// stake out, and it exists for callers who must genuinely DO the work (so
// TryWithFileLock's "assume a peer has it in hand" contract is wrong for them)
// but who must also never hang (so WithFileLock is wrong too). The daemon's kill
// path is the motivating case: it must delete the record, and it holds a
// session-wide guard while doing it, so an unbounded wait here does not merely
// stall one write — it makes the session permanently undeletable (#1917).
//
// Acquisition polls LOCK_NB rather than parking in LOCK_EX because flock offers
// no timed acquire: a blocking Flock cannot be interrupted or given a deadline.
// Polling costs a wakeup every lockPollInterval while contended and trades away
// flock's (already unspecified) queueing fairness; the caller's budget bounds how
// long that lasts. The fd is opened ONCE and reused across attempts so a
// contended retry does not churn the lock file.
func WithFileLockTimeout(path string, timeout time.Duration, fn func() error) error {
	lockPath := path + ".lock"

	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}
	defer f.Close()

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w on %s after %s (another agent-factory process is holding it)", ErrLockTimeout, lockPath, timeout)
		}
		// Sleep no longer than the remaining budget, so the effective wait
		// matches the caller's timeout instead of overshooting by up to a poll.
		wait := lockPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		time.Sleep(wait)
	}
}

// lockPollInterval is how often WithFileLockTimeout re-attempts a contended
// flock. A var so tests can shorten it; production never reassigns.
var lockPollInterval = 20 * time.Millisecond

// WithFileLock acquires an exclusive flock on a .lock file adjacent to the target path,
// executes fn, and releases the lock. This ensures atomic read-modify-write sequences
// across multiple processes. It BLOCKS until the lock is free; see
// TryWithFileLock when a user is waiting on the result, or WithFileLockTimeout
// when the caller must do the work but must not hang (#1917).
func WithFileLock(path string, fn func() error) error {
	lockPath := path + ".lock"

	// Ensure the directory exists so the lock file can be created.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

// AtomicWriteFile writes data to a temporary file in the same directory as path
// and atomically renames it to path. This prevents partial writes from being
// visible to readers.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up the temp file on any error path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", path, err)
	}

	// Rename succeeded: data is visible on disk. The contract
	// "err == nil ⟺ data is persisted" must hold from this point on, so the
	// parent-directory fsync below (which only affects crash durability, not
	// visibility) becomes best-effort. Mark success so the deferred temp-file
	// cleanup is a no-op and downstream callers don't roll back persisted data.
	success = true

	// Fsync the parent directory to ensure the rename (new directory entry) is
	// persisted across a crash. Failures here are logged but not returned --
	// the data is already visible to readers.
	dirFd, err := os.Open(dir)
	if err != nil {
		log.WarningLog.Printf("AtomicWriteFile: failed to open directory %s for post-rename sync: %v", dir, err)
		return nil
	}
	if err := dirFd.Sync(); err != nil {
		log.WarningLog.Printf("AtomicWriteFile: failed to fsync directory %s after rename of %s: %v", dir, path, err)
	}
	if err := dirFd.Close(); err != nil {
		log.WarningLog.Printf("AtomicWriteFile: failed to close directory %s after post-rename sync of %s: %v", dir, path, err)
	}
	return nil
}
