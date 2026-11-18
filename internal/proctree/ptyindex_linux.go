//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ptySlaveIndices reads /proc/<pid>/fd for links into /dev/pts — the slave
// side of each pty the process holds. The directory itself is readable only
// by the process owner (or root) under every procfs mount option, so a read
// failure is "it is gone" or a refusal that means something, never "holds
// none" (same contract openFilePaths keeps).
func ptySlaveIndices(pid int) ([]int, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var indices []int
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			// The fd closed between the listing and the read: a smaller
			// open set, not a failure to see it.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("readlink %s/%s: %w", dir, entry.Name(), err)
		}
		if idx, ok := ptySlaveIndexFromPath(target); ok {
			indices = append(indices, idx)
		}
	}
	return indices, nil
}

// ptyMasterIndices scans /proc/<pid>/fdinfo for tty-index lines — the field
// only ptmx master fds carry, so every hit is one held pty pair. fdinfo is
// the same owner-readable class as fd, so a directory failure shares the
// "could not see" contract; a single fdinfo file that vanished mid-walk is a
// smaller open set, not a failure.
func ptyMasterIndices(pid int) ([]int, error) {
	dir := fmt.Sprintf("/proc/%d/fdinfo", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var indices []int
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s/%s: %w", dir, entry.Name(), err)
		}
		if idx, ok := ptyMasterIndexFromFdinfo(string(body)); ok {
			indices = append(indices, idx)
		}
	}
	return indices, nil
}
