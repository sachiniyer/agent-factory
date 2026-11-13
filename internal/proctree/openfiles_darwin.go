//go:build darwin

package proctree

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openFilePaths lists pid's descriptors with proc_pidinfo(PROC_PIDLISTFDS)
// and resolves each vnode fd's absolute path with
// proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO). There is no directory of
// per-process open-file paths on darwin; this is what lsof does.
//
// Errors mean "could not see", never "seen, empty": ESRCH/ENOENT from the
// list call is ErrProcessExited, a refused call propagates, and an fd that
// closed mid-walk (EBADF/ENOENT from the per-fd call) is simply skipped.
const (
	procInfoCallPIDFDInfo = 3 // PROC_INFO_CALL_PIDFDINFO
	// procInfoCallPIDInfo (= 2) lives in proctree_darwin.go: the descriptor
	// LIST is a pidinfo-family flavor, the per-fd path query is pidfdinfo.
	procPIDListFDs           = 1 // PROC_PIDLISTFDS
	procPIDFdVnodePathInfo   = 2 // PROC_PIDFDVNODEPATHINFO
	procFdTypeVnode          = 1 // PROX_FDTYPE_VNODE
	procFdInfoSize           = 8 // sizeof(struct proc_fdinfo)
	procFdInfoFdOffset       = 0 // offsetof(proc_fdinfo.proc_fd)
	procFdInfoFdTypeOffset   = 4 // offsetof(proc_fdinfo.proc_fdtype)
	procFdInfoListGrowFactor = 2
	procFdInfoListInitialCap = 512
)

func openFilePaths(pid int) (map[string]struct{}, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{}, len(fds))
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		fdType := binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:])
		if fdType != procFdTypeVnode || fd < 0 {
			continue
		}
		path, ok, gone := procFdPath(pid, fd)
		switch {
		case gone:
			continue
		case !ok:
			return nil, errProcFdPathFailed
		default:
			paths[path] = struct{}{}
		}
	}
	return paths, nil
}

var errProcFdPathFailed = errors.New("proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO) failed")

// listProcFds returns the proc_fdinfo array for pid. The first call probes
// for the required byte count; a retry loop guards against the fd table
// growing between probe and read.
func listProcFds(pid int) ([][]byte, error) {
	cap := procFdInfoListInitialCap
	for range 4 {
		buf := make([]byte, cap*procFdInfoSize)
		var p unsafe.Pointer
		if len(buf) > 0 {
			p = unsafe.Pointer(&buf[0])
		}
		n, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDListFDs, 0, p, int32(len(buf)))
		if err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
				return nil, ErrProcessExited
			}
			return nil, err
		}
		if n <= 0 {
			return nil, nil
		}
		if int(n) > len(buf) {
			cap = cap * procFdInfoListGrowFactor
			continue
		}
		count := int(n) / procFdInfoSize
		if int(n)%procFdInfoSize != 0 {
			return nil, errProcFdPathFailed
		}
		if count == cap {
			// Exactly-full could be truncation rather than coincidence.
			cap = cap * procFdInfoListGrowFactor
			continue
		}
		out := make([][]byte, 0, count)
		for i := range count {
			out = append(out, buf[i*procFdInfoSize:(i+1)*procFdInfoSize])
		}
		return out, nil
	}
	return nil, errProcFdPathFailed
}

// procFdPath resolves one vnode fd to its absolute path. gone covers the fd
// having been closed between listing and resolution; !ok covers real
// failures so a partially-read descriptor set never looks like an empty one.
func procFdPath(pid, fd int) (path string, ok bool, gone bool) {
	var buf [vnodeFdInfoWithPathSize]byte
	n, err := procInfoRaw(procInfoCallPIDFDInfo, pid, procPIDFdVnodePathInfo,
		uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
	if err != nil {
		if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH) {
			return "", false, true
		}
		return "", false, false
	}
	if int(n) != len(buf) {
		return "", false, false
	}
	p, ok := fdPathFromVnodeFdInfo(buf[:])
	if !ok {
		return "", false, false
	}
	return p, true, false
}

// procInfoRaw issues the proc_info syscall directly, for both the pidinfo and
// pidfdinfo families, with the calling shape readCPUTime uses.
//
// Syscall6, never RawSyscall6 (#7241). proc_pidinfo and proc_pidfdinfo look the
// target up with proc_find, which on XNU waits, uninterruptibly, while the
// target is part-way through an exec (proc_ref_wait_for_exec does an
// assert_wait with THREAD_UNINT, xnu-11417.140.69 kern_proc.c). A raw syscall
// never tells the Go scheduler it is in the kernel, so a call parked there pins
// its P, and every stop-the-world waits for it. A wall-clock bound on the
// caller bounds the caller, not the runtime. The reap scans exactly the
// processes most likely to be forking or exec'ing.
func procInfoRaw(callnum, pid, flavor int, arg uint64, buf unsafe.Pointer, size int32) (int32, error) {
	r1, _, errno := unix.Syscall6(
		unix.SYS_PROC_INFO,
		uintptr(callnum),
		uintptr(pid),
		uintptr(flavor),
		uintptr(arg),
		uintptr(buf),
		uintptr(size),
	)
	if errno != 0 {
		return 0, errno
	}
	return int32(r1), nil
}
