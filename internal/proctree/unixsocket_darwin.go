//go:build darwin

package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	procPIDFdSocketInfo = 3 // PROC_PIDFDSOCKETINFO
	procFdTypeSocket    = 2 // PROX_FDTYPE_SOCKET
)

// unixListenerPaths lists pid's descriptors with proc_pidinfo(PROC_PIDLISTFDS)
// and decodes each socket with proc_pidfdinfo(PROC_PIDFDSOCKETINFO). A buffer
// the decoder refuses is an error for the whole read: the caller must treat the
// process as unresolved, never as holding no listener.
func unixListenerPaths(pid int) ([]string, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, fi := range fds {
		fd := int(int32(binary.LittleEndian.Uint32(fi[procFdInfoFdOffset:])))
		if binary.LittleEndian.Uint32(fi[procFdInfoFdTypeOffset:]) != procFdTypeSocket || fd < 0 {
			continue
		}
		var buf [socketFdInfoSize]byte
		n, err := procInfoRaw(procInfoCallPIDFDInfo, pid, procPIDFdSocketInfo,
			uint64(fd), unsafe.Pointer(&buf[0]), int32(len(buf)))
		if err != nil {
			if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ENOENT) {
				continue // closed since the listing: no longer held
			}
			if errors.Is(err, syscall.ESRCH) {
				return nil, ErrProcessExited
			}
			return nil, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDSOCKETINFO) for fd %d: %w", fd, err)
		}
		if int(n) != len(buf) {
			return nil, fmt.Errorf("proc_pidfdinfo(PROC_PIDFDSOCKETINFO) for fd %d wrote %d bytes, want %d", fd, n, len(buf))
		}
		s, err := unixSocketFromSocketFdInfo(buf[:])
		if err != nil {
			return nil, fmt.Errorf("fd %d: %w", fd, err)
		}
		if s.unix && s.listening && s.path != "" {
			paths = append(paths, s.path)
		}
	}
	return paths, nil
}
