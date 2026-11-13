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
// to "pipe:[N]" for the N of one of ends. Both ends of a Linux pipe share that
// inode. A match is either end; the caller's own end never matches here,
// because the caller asks about other processes.
//
// What a match proves is "holds a pipe with this inode number", not "holds
// this pipe" (#7241). pipefs numbers its inodes with get_next_ino(), an
// unsigned int counter that wraps (62% of the way round on a devbox after 199
// days of uptime), so after a wrap a new pipe can take the number of one still
// open. No kill may rest on this answer alone. The holder reap pairs it with
// membership of a group whose members all got their pipes during the command's
// own lifetime, which rules out a pipe from 2^32 pipes earlier.
//
// The directory is readable only by the process owner, and not at all for a
// process that made itself non-dumpable, so a refusal is returned as an error
// rather than read as "holds nothing". An fd that closed between the listing
// and the readlink is a smaller open set, not a failure to see it.
func holdsPipeEnd(pid int, ends []PipeEnd) (bool, error) {
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
		ino, err := pipeInodeFromLink(target)
		if err != nil {
			continue
		}
		for _, end := range ends {
			if ino == end.held {
				return true, nil
			}
		}
	}
	return false, nil
}

// checkPipePair confirms held and kept are the two ends of one Linux pipe,
// which share a pipefs inode.
func checkPipePair(held, kept uint64) error {
	if held != kept {
		return fmt.Errorf("proctree: pipe ends report inodes %d and %d; they are not one pipe", held, kept)
	}
	return nil
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
