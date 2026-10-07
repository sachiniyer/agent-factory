//go:build darwin

package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// unixConnPeerPID reads the peer's pid with LOCAL_PEERPID, darwin's
// getpeereid-for-pids equivalent. Like Linux's SO_PEERCRED the value is the
// kernel's record of who connected, not anything the client supplied.
func unixConnPeerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("cannot reach control connection fd for LOCAL_PEERPID: %w", err)
	}
	var pid int
	var pidErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if ctrlErr != nil {
		return 0, fmt.Errorf("cannot access control connection fd for LOCAL_PEERPID: %w", ctrlErr)
	}
	if pidErr != nil {
		return 0, fmt.Errorf("LOCAL_PEERPID: %w", pidErr)
	}
	return pid, nil
}
