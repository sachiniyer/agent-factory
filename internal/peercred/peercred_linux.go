//go:build linux

package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// unixConnPeerPID reads SO_PEERCRED's pid for the accepted unix socket. The
// credential is fixed at connect time by the kernel; nothing the peer writes
// after connecting can change it.
func unixConnPeerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("cannot reach control connection fd for SO_PEERCRED: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if ctrlErr != nil {
		return 0, fmt.Errorf("cannot access control connection fd for SO_PEERCRED: %w", ctrlErr)
	}
	if credErr != nil {
		return 0, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	return int(cred.Pid), nil
}
