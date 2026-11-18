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

// unixSockets lists pid's descriptors with proc_pidinfo(PROC_PIDLISTFDS) and
// reads each socket on its own with proc_pidfdinfo(PROC_PIDFDSOCKETINFO): the
// per-socket read UnixSockets requires (#7536). A buffer the decoder refuses is
// an error for the whole read: the caller must treat the process as
// unresolved, never as holding no socket.
func unixSockets(pid int) ([]UnixSocket, error) {
	fds, err := listProcFds(pid)
	if err != nil {
		return nil, err
	}
	var socks []UnixSocket
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
		if s.unix {
			socks = append(socks, UnixSocket{Path: s.path, Listening: s.listening})
		}
	}
	return socks, nil
}

// unixListenerPathsByTable: darwin has no socket table to walk, and needs no
// fallback, since its read is already per socket.
func unixListenerPathsByTable(int) ([]string, error) { return nil, ErrUnsupportedPlatform }
