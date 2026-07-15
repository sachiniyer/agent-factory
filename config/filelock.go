package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

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

// WithFileLock acquires an exclusive flock on a .lock file adjacent to the target path,
// executes fn, and releases the lock. This ensures atomic read-modify-write sequences
// across multiple processes. It BLOCKS until the lock is free; see
// TryWithFileLock when a user is waiting on the result.
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
