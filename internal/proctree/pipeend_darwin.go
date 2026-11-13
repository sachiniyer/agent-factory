//go:build darwin

package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	procPIDFdPipeInfo = 6 // PROC_PIDFDPIPEINFO
	procFdTypePipe    = 6 // PROX_FDTYPE_PIPE
)

var errProcFdPipeInfoFailed = errors.New("proc_pidfdinfo(PROC_PIDFDPIPEINFO) returned an unusable answer")

// holdsPipeEnd lists pid's descriptors with proc_pidinfo(PROC_PIDLISTFDS),
// the same list openFilePaths walks, and asks proc_pidfdinfo for each pipe
// descriptor's pipe_handle. That handle is the hash of the end's own struct
// pipe, which is what PipeEndID read through fstat (bsd/kern/sys_pipe.c), so
// equality means the very same end.
//
// An fd that closed between the listing and the query (EBADF, ENOENT) is
// skipped. A process that is gone (ESRCH) is ErrProcessExited. Any other
// failure, including an answer of the wrong size, is an error: a partly read
// descriptor set must never look like one that holds nothing.
func holdsPipeEnd(pid int, want map[uint64]bool) (bool, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return false, err
	}
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		if binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:]) != procFdTypePipe || fd < 0 {
			continue
		}
		var buf [pipeFdInfoSize]byte
		n, err := procInfoRaw(procInfoCallPIDFDInfo, pid, procPIDFdPipeInfo,
			uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
		if err != nil {
			switch {
			case errors.Is(err, syscall.EBADF), errors.Is(err, syscall.ENOENT):
				continue
			case errors.Is(err, syscall.ESRCH):
				return false, ErrProcessExited
			default:
				return false, fmt.Errorf("proc_pidfdinfo(%d, fd %d, PROC_PIDFDPIPEINFO): %w", pid, fd, err)
			}
		}
		if int(n) != len(buf) {
			return false, errProcFdPipeInfoFailed
		}
		handle, ok := pipeHandleFromPipeFdInfo(buf[:])
		if !ok {
			return false, errProcFdPipeInfoFailed
		}
		if want[handle] {
			return true, nil
		}
	}
	return false, nil
}

// groupMembers reads kern.proc.pgrp: one sysctl for the members of one group,
// rather than the whole table. processFromKinfo refuses zombies.
func groupMembers(pgid int) ([]Process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("reading process group %d (kern.proc.pgrp): %w", pgid, err)
	}
	members := make([]Process, 0, len(kps))
	for i := range kps {
		if p, ok := processFromKinfo(&kps[i]); ok {
			members = append(members, p)
		}
	}
	return members, nil
}
