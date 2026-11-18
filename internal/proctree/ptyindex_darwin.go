//go:build darwin

package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Darwin decodes each held pty through proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO),
// the same kernel read openFilePaths already performs — plus the vnode's
// vst_rdev, which for a /dev/ptmx master carries the pty index in its minor
// (measured on the macOS runner, tmux 3.4: master's rdev 0x0f000000 = major 15
// minor 0 against its /dev/ttys000 slave's 0x10000000 = major 16 minor 0; the
// shared minor is the index #7469 needs). The slave side keeps its name's
// numeric tail — /dev/ttysNNN — the same convention /dev/pts/N uses on linux.
//
// Offsets inside vnode_fdinfowithpath, from Apple's headers: proc_fileinfo is
// 24 bytes, then vinfo_stat — vst_dev u32 at 24, vst_rdev u32 at 140 — then
// vnode_info's vi_type i32 at 160, vip_path at 176 (fdinfopath.go). vi_type
// VCHR is 4, measured on both ptmx and ttys vnodes.
const (
	vnodeFdInfoRdevOffset = 140
	vnodeFdInfoViTypeOff  = 160
	vnodeFdInfoViTypeVCHR = 4
	darwinPtmxPath        = "/dev/ptmx"
	darwinDevTMinorMask   = 0xffffff // makedev(major<<24 | minor)
)

// ptyFdReadBudget bounds ONE process's fd walk. Every vnode decode goes
// through vn_stat → VNOP_GETATTR, which BLOCKS on a hung NFS, SMB or macFUSE
// mount — and a carrier or tmux server holding a single fd into such a mount
// would otherwise wedge the sweep's attribution read forever (#7469 R2). No
// getattr-free discriminator exists for another process's vnodes — the fd
// list says only "vnode", and both VNODEINFO and VNODEPATHINFO getattr — so
// the deadline is the mechanism: the whole walk runs on a worker goroutine
// (the same idiom exeCheckShared uses for its kernel reads), an expired
// budget reports "could not see" — undecidable, never foreign or own — and
// the abandoned worker is the only thing left on the mount. Package var so
// the deadline test can shrink it.
var ptyFdReadBudget = 2 * time.Second

// errPtyFdTimeout is the expired-budget answer: the fd table could not be
// READ in bounded time — "could not see", never "holds none".
var errPtyFdTimeout = errors.New("pty fd read exceeded its deadline")

// ptySlaveIndices returns the identity of every /dev/ttysNNN vnode pid
// holds. devfs is single-instance — there is no second devpts to mint a
// colliding index — so the Dev half is 0 and the index alone qualifies.
func ptySlaveIndices(pid int) ([]PtyIndex, error) {
	return ptyFdIndices(pid, func(path string, viType int32, rdev uint32) (PtyIndex, bool) {
		if viType == vnodeFdInfoViTypeVCHR && strings.HasPrefix(path, "/dev/ttys") {
			if idx, ok := ptySlaveIndexFromPath(path); ok {
				return PtyIndex{Index: idx}, true
			}
		}
		return PtyIndex{}, false
	})
}

// ptyMasterIndices returns the identity of every /dev/ptmx vnode pid holds,
// read from the vnode's rdev minor — the name cannot say which pty it
// masters.
func ptyMasterIndices(pid int) ([]PtyIndex, error) {
	return ptyFdIndices(pid, func(path string, viType int32, rdev uint32) (PtyIndex, bool) {
		if viType == vnodeFdInfoViTypeVCHR && path == darwinPtmxPath {
			return PtyIndex{Index: int(rdev & darwinDevTMinorMask)}, true
		}
		return PtyIndex{}, false
	})
}

// ptyFdIndices runs pid's vnode walk under ptyFdReadBudget: a walk that
// does not answer in bounded wall-clock time — a getattr parked on a hung
// mount — is "could not see", never a silent partial read (#7469 R2). The
// abandoned worker cannot be cancelled (an uninterruptible syscall is a
// one-way door), so the channel is buffered: it never blocks on send.
func ptyFdIndices(pid int, classify func(path string, viType int32, rdev uint32) (PtyIndex, bool)) ([]PtyIndex, error) {
	type result struct {
		indices []PtyIndex
		err     error
	}
	done := make(chan result, 1)
	go func() {
		indices, err := ptyFdWalk(pid, classify)
		done <- result{indices, err}
	}()
	timer := time.NewTimer(ptyFdReadBudget)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.indices, r.err
	case <-timer.C:
		return nil, errPtyFdTimeout
	}
}

// ptyFdWalk is the unbounded scan ptyFdIndices bounds — a package var so the
// deadline test can stage a walk that never answers. A refused or short read
// of the LIST is "could not see"; a single fd closing mid-walk is a smaller
// open set.
var ptyFdWalk = func(pid int, classify func(path string, viType int32, rdev uint32) (PtyIndex, bool)) ([]PtyIndex, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return nil, err
	}
	var indices []PtyIndex
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		if binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:]) != procFdTypeVnode || fd < 0 {
			continue
		}
		var buf [vnodeFdInfoWithPathSize]byte
		n, err := procInfoRaw(procInfoCallPIDFDInfo, pid, procPIDFdVnodePathInfo,
			uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
		if err != nil {
			if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENOENT) {
				continue // closed since the listing
			}
			if errors.Is(err, syscall.ESRCH) {
				return nil, ErrProcessExited
			}
			return nil, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO) for fd %d: %w", fd, err)
		}
		if int(n) != len(buf) {
			return nil, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO) for fd %d wrote %d bytes, want %d", fd, n, len(buf))
		}
		path, ok := fdPathFromVnodeFdInfo(buf[:])
		if !ok {
			continue
		}
		viType := int32(binary.LittleEndian.Uint32(buf[vnodeFdInfoViTypeOff:]))
		rdev := binary.LittleEndian.Uint32(buf[vnodeFdInfoRdevOffset:])
		if idx, ok := classify(path, viType, rdev); ok {
			indices = append(indices, idx)
		}
	}
	return indices, nil
}
