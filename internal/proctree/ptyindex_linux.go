//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ptySlaveIndices reads /proc/<pid>/fd for links into /dev/pts — the slave
// side of each pty the process holds. The directory itself is readable only
// by the process owner (or root) under every procfs mount option, so a read
// failure is "it is gone" or a refusal that means something, never "holds
// none" (same contract openFilePaths keeps).
func ptySlaveIndices(pid int) ([]PtyIndex, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var indices []PtyIndex
	for _, entry := range entries {
		fdPath := filepath.Join(dir, entry.Name())
		target, err := os.Readlink(fdPath)
		if err != nil {
			// The fd closed between the listing and the read: a smaller
			// open set, not a failure to see it.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("readlink %s/%s: %w", dir, entry.Name(), err)
		}
		idx, ok := ptySlaveIndexFromPath(target)
		if !ok {
			continue
		}
		dev, err := ptySlaveDev(fdPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", fdPath, err)
		}
		indices = append(indices, PtyIndex{Dev: dev, Index: idx})
	}
	return indices, nil
}

// ptyMasterFdIndices scans /proc/<pid>/fdinfo for tty-index lines — the
// field only ptmx master fds carry, so every hit is one held pty pair
// paired with its descriptor number. fdinfo is the same owner-readable
// class as fd, so a directory failure shares the "could not see" contract;
// a single fdinfo file that vanished mid-walk is a smaller open set, not a
// failure. The dev half of each identity comes from the same fd the index
// did, so a master can never borrow another instance's qualifier.
func ptyMasterFdIndices(pid int) ([]PtyFdIndex, error) {
	dir := fmt.Sprintf("/proc/%d/fdinfo", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var indices []PtyFdIndex
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		idx, ok, err := ptyMasterIndexAt(pid, fd)
		if errors.Is(err, fs.ErrNotExist) {
			continue // the fd closed between listing and read
		}
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", dir, entry.Name(), err)
		}
		if !ok {
			continue
		}
		dev, err := ptyMasterDev(pid, idx)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("devpts lookup for fd %s of pid %d: %w", entry.Name(), pid, err)
		}
		indices = append(indices, PtyFdIndex{PtyIndex: PtyIndex{Dev: dev, Index: idx}, Fd: fd})
	}
	return indices, nil
}

// ptyMasterFdIndex answers the tty-index of ONE descriptor — the single-fd
// re-verify behind the sweep's ownership claims (#7581 item 2). ok is
// false for any failure to see the fd and for an fd that is not a ptmx
// master: the caller treats both as "proved nothing", never as evidence.
func ptyMasterFdIndex(pid, fd int) (PtyIndex, bool) {
	idx, ok, err := ptyMasterIndexAt(pid, fd)
	if err != nil || !ok {
		return PtyIndex{}, false
	}
	dev, err := ptyMasterDev(pid, idx)
	if err != nil {
		return PtyIndex{}, false
	}
	return PtyIndex{Dev: dev, Index: idx}, true
}

// ptyMasterIndexAt reads one /proc/<pid>/fdinfo/<fd> body for its
// tty-index — the field only a ptmx master fd carries. ok is false when
// the fd holds no tty-index; err is the filesystem read's own failure.
func ptyMasterIndexAt(pid, fd int) (idx int, ok bool, err error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%d", pid, fd))
	if err != nil {
		return 0, false, err
	}
	idx, ok = ptyMasterIndexFromFdinfo(string(body))
	return idx, ok, nil
}

// ptySlaveDev answers which terminal instance a held slave descriptor belongs
// to: the filesystem id of the /dev/pts/N node the fd resolves to — the
// devpts mount's own st_dev, distinct per mount instance. The stat follows
// the /proc fd link to the open inode itself, so the carrier's own mount view
// is irrelevant: a slave inherited from a pane still names the instance it
// was minted on.
func ptySlaveDev(fdPath string) (uint64, error) {
	fi, err := os.Stat(fdPath)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type %T", fi.Sys())
	}
	return st.Dev, nil
}

// ptyMasterDev answers the same instance identity for a master fd. The ptmx
// side cannot be keyed off its own stat — a master's inode does not live in
// devpts (measured: a master's st_dev is the devtmpfs it was opened through,
// never the instance's) — so the lookup stats the SLAVE node index N inside
// the server's own view instead, /proc/<pid>/root/dev/pts/N, whose st_dev is
// the devpts instance the pair was allocated on. Two servers on separate
// devpts mounts (a host and a container) mint the same indices; this keeps a
// carrier's slave matched only to the master of its own pair (Codex on
// #7469). A server whose devpts lives outside the conventional mount point
// contributes no identity — undecidable, the safe direction.
func ptyMasterDev(pid, idx int) (uint64, error) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d/root/dev/pts/%d", pid, idx))
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type %T", fi.Sys())
	}
	return st.Dev, nil
}
