//go:build linux

package session

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// socketPeerCred is the test seam over SO_PEERCRED: which process is
// listening on the unix socket af just connected to — pid and uid. The
// capacity identity proof needs the answering daemon's pid to read its
// mount table, and its uid to recognize a genuinely privileged engine
// when that table is unreadable (#5382 review).
var socketPeerCred = platformSocketPeerCred

func platformSocketPeerCred(conn net.Conn) (pid, uid int, err error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if serr != nil || cred == nil {
		return 0, 0, serr
	}
	return int(cred.Pid), int(cred.Uid), nil
}
