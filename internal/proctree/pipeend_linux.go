//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// holdsPipeEnd walks /proc/<pid>/fd and reports whether any descriptor links
// to "pipe:[N]" for an N in want. Both ends of a Linux pipe share that inode,
// so a match is either end. The caller's own end never matches here, because
// the caller asks about other processes.
//
// The directory is readable only by the process owner, and not at all for a
// process that made itself non-dumpable, so a refusal is returned as an error
// rather than read as "holds nothing". An fd that closed between the listing
// and the readlink is a smaller open set, not a failure to see it.
func holdsPipeEnd(pid int, want map[uint64]bool) (bool, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, ErrProcessExited
		}
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, fmt.Errorf("readlink %s/%s: %w", dir, entry.Name(), err)
		}
		if ino, err := pipeInodeFromLink(target); err == nil && want[ino] {
			return true, nil
		}
	}
	return false, nil
}

// groupMembers scans /proc for every process whose stat pgrp field is pgid.
// Linux has no per-group listing, so this reads each /proc/<pid>/stat once,
// the same read Snapshot makes. A process that exited mid-scan, or is a
// zombie, is skipped: it holds no descriptors.
func groupMembers(pgid int) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	var members []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, pgrp, err := readProcStat(pid)
		if err != nil || pgrp != pgid {
			continue
		}
		members = append(members, p)
	}
	return members, nil
}
