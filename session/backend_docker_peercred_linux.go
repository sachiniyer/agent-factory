//go:build linux

package session

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// socketPeerPid is the test seam over SO_PEERCRED: which process is
// listening on the unix socket af just connected to. The capacity
// namespace proof needs the answering daemon's pid so its /proc/<pid>/ns/mnt
// can be compared to af's own (#5382 review).
var socketPeerPid = platformSocketPeerPid

func platformSocketPeerPid(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil || cred == nil {
		return 0, serr
	}
	return int(cred.Pid), nil
}
