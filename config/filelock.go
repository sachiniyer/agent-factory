package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// WithFileLock acquires an exclusive flock on a .lock file adjacent to the target path,
// executes fn, and releases the lock. This ensures atomic read-modify-write sequences
// across multiple processes.
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

// LockedUpdate loads a file under an exclusive lock, applies fn to transform
// its contents, and atomically writes the result back. If the file doesn't
// exist, fn receives nil. This is the preferred way to do read-modify-write
// on any shared JSON file.
func LockedUpdate(path string, perm os.FileMode, fn func(data []byte) ([]byte, error)) error {
	return WithFileLock(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}
		newData, err := fn(data)
		if err != nil {
			return err
		}
		return AtomicWriteFile(path, newData, perm)
	})
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

	// Fsync the parent directory to ensure the rename (new directory entry) is
	// persisted. Without this, a crash right after rename could lose the file.
	dirFd, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("failed to open directory %s for sync: %w", dir, err)
	}
	if err := dirFd.Sync(); err != nil {
		dirFd.Close()
		return fmt.Errorf("failed to sync directory %s: %w", dir, err)
	}
	if err := dirFd.Close(); err != nil {
		return fmt.Errorf("failed to close directory %s: %w", dir, err)
	}

	success = true
	return nil
}
