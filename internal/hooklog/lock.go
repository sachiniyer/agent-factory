package hooklog

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockKeptLog never waits for a hook (or another pruning process). O_NOFOLLOW
// prevents a replacement symlink from directing the lock outside this log.
func lockKeptLog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func removeKeptLog(path string, scanned os.FileInfo, now time.Time) (bool, error) {
	file, err := lockKeptLog(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.ELOOP) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()
	// Keep the lock through removal, and check both the descriptor and its
	// path so a replacement never inherits a stale pruning decision.
	current, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(scanned, current) ||
		!scanned.ModTime().Equal(current.ModTime()) || scanned.Size() != current.Size() ||
		now.Sub(current.ModTime()) < logGraceAge {
		return false, nil
	}
	linked, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, linked) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
